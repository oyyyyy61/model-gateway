package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	// API key鉴权
	"github.com/oyyyyy61/model-gateway/internal/auth"
	// 配置读取
	"github.com/oyyyyy61/model-gateway/internal/config"
	// HTTP handler和代理
	"github.com/oyyyyy61/model-gateway/internal/gateway"
	// 中间件负载均衡
	"github.com/oyyyyy61/model-gateway/internal/middleware"
	// 模型实例选择、负载均衡策略
	"github.com/oyyyyy61/model-gateway/internal/routing"
)

func main() {
	// 读取环境变量配置，返回Config struct，路径声明在internal\config\config.go
	cfg := config.Load()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// 创建鉴权服务，读取配置
	authService := auth.NewService(cfg.APIKey, cfg.TenantID, cfg.AllowedModels)
	// 创建模型实例列表
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

	// 创建代理服务，传入模型实例列表、HTTP客户端、请求超时时间和日志记录器
	proxy := gateway.NewProxy(gateway.ProxyConfig{
		Picker:         routing.NewPicker(instances),
		Client:         &http.Client{},
		RequestTimeout: cfg.RequestTimeout,
		Logger:         logger,
	})
	// 创建HTTP handler，传入代理服务、鉴权服务和日志记录器
	handler := gateway.NewHandler(proxy, authService, logger)
	root := middleware.Chain(handler, logger)
	// 创建HTTP服务器，传入监听地址、HTTP handler、读取头超时时间和空闲超时时间
	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           root,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       90 * time.Second,
	}

	// 启动HTTP服务器，监听中断信号，优雅关闭
	go func() {
		logger.Info("gateway started", "addr", cfg.ListenAddr, "models", strings.Join(cfg.AllowedModels, ","))
		// ListenAndServe表示启动HTTP服务器，监听指定的地址和端口，并处理传入的HTTP请求。
		// 如果服务器启动失败，且错误不是http.ErrServerClosed，则记录错误并退出程序。
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("gateway stopped unexpectedly", "error", err)
			os.Exit(1)
		}
	}()

	// 监听中断信号
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	// 优雅关闭HTTP服务器，关闭监听不在接收新请求，等待当前请求处理完成后再关闭服务器。若在10秒内未完成，则强制关闭服务器。
	// ctx传给 Shutdown，作为"最多等多久"的计时器；cancel函数在Shutdown完成后调用，释放资源。
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		logger.Error("gateway shutdown failed", "error", err)
	}
}
