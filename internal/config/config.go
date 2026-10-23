package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

type Upstream struct {
	ID          string
	Model       string
	Endpoint    string
	Weight      int
	MaxInflight int
}

type Config struct {
	ListenAddr     string
	APIKey         string
	TenantID       string
	AllowedModels  []string
	RequestTimeout time.Duration
	Upstreams      []Upstream
}

func Load() Config {
	models := splitEnv("MGW_ALLOWED_MODELS", "qwen-demo")
	return Config{
		ListenAddr:     env("MGW_LISTEN_ADDR", ":8080"),
		APIKey:         env("MGW_API_KEY", "mgw_live_demo"),
		TenantID:       env("MGW_TENANT_ID", "tenant-demo"),
		AllowedModels:  models,
		RequestTimeout: durationEnv("MGW_REQUEST_TIMEOUT", 120*time.Second),
		Upstreams:      parseUpstreams(env("MGW_UPSTREAMS", "qwen-demo-01|qwen-demo|http://mock-model-1:8080|100|8")),
	}
}

func parseUpstreams(raw string) []Upstream {
	var result []Upstream
	for _, item := range strings.Split(raw, ";") {
		parts := strings.Split(item, "|")
		if len(parts) != 5 {
			continue
		}
		weight, errWeight := strconv.Atoi(parts[3])
		maxInflight, errInflight := strconv.Atoi(parts[4])
		if parts[0] == "" || parts[1] == "" || parts[2] == "" || errWeight != nil || errInflight != nil || weight <= 0 || maxInflight <= 0 {
			continue
		}
		result = append(result, Upstream{ID: parts[0], Model: parts[1], Endpoint: strings.TrimRight(parts[2], "/"), Weight: weight, MaxInflight: maxInflight})
	}
	return result
}

func env(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func splitEnv(key, fallback string) []string {
	values := strings.Split(env(key, fallback), ",")
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			result = append(result, value)
		}
	}
	return result
}

func durationEnv(key string, fallback time.Duration) time.Duration {
	value := os.Getenv(key)
	if parsed, err := time.ParseDuration(value); err == nil && parsed > 0 {
		return parsed
	}
	return fallback
}
