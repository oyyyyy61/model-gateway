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

	"model-gateway/internal/auth"
	"model-gateway/internal/config"
	"model-gateway/internal/gateway"
	"model-gateway/internal/middleware"
	"model-gateway/internal/routing"
)

func main() {
	cfg := config.Load()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	authService := auth.NewService(cfg.APIKey, cfg.TenantID, cfg.AllowedModels)
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

	proxy := gateway.NewProxy(gateway.ProxyConfig{
		Picker:         routing.NewPicker(instances),
		Client:         &http.Client{},
		RequestTimeout: cfg.RequestTimeout,
		Logger:         logger,
	})
	handler := gateway.NewHandler(proxy, authService, logger)
	root := middleware.Chain(handler, logger)

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           root,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       90 * time.Second,
	}

	go func() {
		logger.Info("gateway started", "addr", cfg.ListenAddr, "models", strings.Join(cfg.AllowedModels, ","))
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("gateway stopped unexpectedly", "error", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		logger.Error("gateway shutdown failed", "error", err)
	}
}
