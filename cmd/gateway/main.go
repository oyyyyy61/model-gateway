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

func main() {
	cfg := config.Load()
	logger := newLogger(cfg.LogLevel)

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
	authService := auth.NewService(keys)
	if authService.Len() == 0 {
		logger.Warn("no usable api key configured; every model request will be rejected")
	}

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
	picker := routing.NewPicker(instances)

	registry := metrics.New()
	registry.BuildInfo.Set(1, map[string]string{"version": version.Version})
	for _, snapshot := range picker.Snapshot() {
		registry.UpstreamHealthy.Set(boolFloat(snapshot.Healthy), map[string]string{"instance": snapshot.ID, "model": snapshot.Model})
	}

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

	proxy := gateway.NewProxy(gateway.ProxyConfig{
		Picker:         picker,
		Client:         upstreamClient(),
		RequestTimeout: cfg.RequestTimeout,
		Logger:         logger,
		MaxAttempts:    cfg.RetryAttempts,
		MaxBodyBytes:   cfg.MaxBodyBytes,
		Metrics:        registry,
	})
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
	root := middleware.Chain(handler, logger)

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           root,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       90 * time.Second,
	}

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

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	stopChecker()
	select {
	case <-checkerDone:
	case <-time.After(2 * time.Second):
		logger.Warn("health checker did not stop in time")
	}

	shutdownContext, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownContext); err != nil {
		logger.Error("gateway shutdown failed", "error", err)
	}
}

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

func boolFloat(value bool) float64 {
	if value {
		return 1
	}
	return 0
}
