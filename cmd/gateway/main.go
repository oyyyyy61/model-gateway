package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/oyyyyy61/model-gateway/internal/auth"
	"github.com/oyyyyy61/model-gateway/internal/config"
	"github.com/oyyyyy61/model-gateway/internal/gateway"
	"github.com/oyyyyy61/model-gateway/internal/health"
	"github.com/oyyyyy61/model-gateway/internal/limits"
	"github.com/oyyyyy61/model-gateway/internal/metrics"
	"github.com/oyyyyy61/model-gateway/internal/middleware"
	"github.com/oyyyyy61/model-gateway/internal/routing"
	"github.com/oyyyyy61/model-gateway/internal/version"
)

// main.go 是网关进程入口：读取配置，构建鉴权、路由、指标、健康检查、代理和
// HTTP 处理器，启动服务并在收到 SIGINT/SIGTERM 后优雅关闭。
func main() {
	// 配置读取：环境变量 → config.Config；默认值和解析规则在 internal/config/config.go。
	cfg := config.Load()
	logger := newLogger(cfg.LogLevel)

	// 租户 Key 鉴权：每个配置项转成 auth.Key，只保存 SHA-256 哈希和策略。
	keys := make([]auth.Key, 0, len(cfg.APIKeys))
	for _, item := range cfg.APIKeys {
		key := auth.NewKey(item.Key, item.TenantID, item.Name, item.Models, item.RPM, item.DailyTokens)
		keys = append(keys, key)
		logger.Info("api key configured",
			"tenant_id", key.TenantID,
			"key_id", key.ID,
			"models", strings.Join(key.ModelList(), ","),
			"rpm", key.RPM,
			"daily_tokens", key.DailyTokens,
		)
	}
	// 创建鉴权服务；内部按 Key 哈希索引，空 token/租户会被过滤。
	authService := auth.NewService(keys)
	if authService.Len() == 0 {
		logger.Warn("no usable api key configured; every model request will be rejected")
	}

	// 创建模型实例列表：id|model|endpoint|weight|max_inflight。
	instances := make([]routing.Instance, 0, len(cfg.Upstreams))
	for _, upstream := range cfg.Upstreams {
		instances = append(instances, routing.Instance{
			ID:          upstream.ID,
			Model:       upstream.Model,
			Endpoint:    upstream.Endpoint,
			Weight:      upstream.Weight,
			MaxInflight: upstream.MaxInflight,
		})
	}
	// 模型实例选择/负载均衡：平滑加权轮询 + 每实例并发信号量。
	picker := routing.NewPicker(instances)

	// 指标注册表；先把每个实例的初始健康状态写入 gauge。
	registry := metrics.New()
	registry.BuildInfo.Set(1, map[string]string{"version": version.Version})
	for _, snapshot := range picker.Snapshot() {
		registry.UpstreamHealthy.Set(boolFloat(snapshot.Healthy), map[string]string{"instance": snapshot.ID, "model": snapshot.Model})
	}

	// 健康探测器：后台周期探测，连续失败下线、连续成功恢复。
	checker := health.New(health.Config{
		Picker:    picker,
		Client:    &http.Client{Timeout: 5 * time.Second},
		Interval:  cfg.HealthInterval,
		Path:      cfg.HealthPath,
		Failures:  cfg.HealthFailures,
		Successes: cfg.HealthSuccesses,
		Logger:    logger,
		Metrics:   registry,
	})

	checkerContext, stopChecker := context.WithCancel(context.Background())
	defer stopChecker()
	checkerDone := make(chan struct{})
	go func() {
		defer close(checkerDone)
		checker.Run(checkerContext)
	}()

	// 代理：请求体限制、实例选择、故障转移、SSE 透传和 usage 解析。
	proxy := gateway.NewProxy(gateway.ProxyConfig{
		Picker:         picker,
		Client:         upstreamClient(),
		RequestTimeout: cfg.RequestTimeout,
		Logger:         logger,
		MaxAttempts:    cfg.RetryAttempts,
		MaxBodyBytes:   cfg.MaxBodyBytes,
		Metrics:        registry,
	})
	// HTTP 处理器：路由、认证、模型授权、限流/配额、指标和审计。
	handler := gateway.NewHandler(gateway.HandlerConfig{
		Proxy:      proxy,
		Auth:       authService,
		Limits:     limits.New(),
		Metrics:    registry,
		Health:     checker,
		AdminToken: cfg.AdminToken,
		Version:    version.Version,
		Logger:     logger,
	})
	// 中间件顺序：request id → 访问日志 → panic recovery（recovery 在最外层）。
	root := middleware.Chain(handler, logger)

	// HTTP 服务器：只设置读头超时和空闲超时；单请求超时由代理层控制。
	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           root,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       90 * time.Second,
	}

	// 启动 HTTP 服务器；ListenAndServe 阻塞运行，ErrServerClosed 是 Shutdown 的正常返回。
	go func() {
		logger.Info("gateway started",
			"addr", cfg.ListenAddr,
			"version", version.Version,
			"models", strings.Join(picker.Models(), ","),
			"upstreams", len(instances),
			"health_interval", cfg.HealthInterval.String(),
			"admin_enabled", cfg.AdminToken != "",
		)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("gateway stopped unexpectedly", "error", err)
			os.Exit(1)
		}
	}()

	// 监听中断信号。
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	stopChecker()
	select {
	case <-checkerDone:
	case <-time.After(2 * time.Second):
		logger.Warn("health checker did not stop in time")
	}

	// 优雅关闭：先停健康探测器，再等待在途请求；Shutdown 的 ctx 是“最多等多久”的计时器。
	shutdownContext, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	// cancel 函数在 Shutdown 完成后调用，释放计时器资源。
	defer cancel()
	if err := server.Shutdown(shutdownContext); err != nil {
		logger.Error("gateway shutdown failed", "error", err)
	}
}

// upstreamClient 构建带连接池和超时的上游 HTTP 客户端；请求级超时由代理层控制。
func upstreamClient() *http.Client {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{Transport: transport}
}

// newLogger 输出 JSON 日志到标准输出，级别来自 MGW_LOG_LEVEL。
func newLogger(level string) *slog.Logger {
	var parsed slog.Level
	switch strings.ToLower(level) {
	case "debug":
		parsed = slog.LevelDebug
	case "warn":
		parsed = slog.LevelWarn
	case "error":
		parsed = slog.LevelError
	default:
		parsed = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: parsed}))
}

// boolFloat 把布尔值转成 Prometheus Gauge 指标的 0/1。
func boolFloat(value bool) float64 {
	if value {
		return 1
	}
	return 0
}
