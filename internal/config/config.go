// Package config loads the gateway configuration from environment variables.
//
// The gateway is intentionally configuration-file free: every deployment
// artifact (docker compose, Kubernetes ConfigMap, tests) can set the same
// variables.
package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Upstream describes one routable model instance.
type Upstream struct {
	ID          string
	Model       string
	Endpoint    string
	Weight      int
	MaxInflight int
}

// APIKeyConfig is one entry of MGW_API_KEYS.
type APIKeyConfig struct {
	Key         string
	TenantID    string
	Name        string
	Models      []string
	RPM         int
	DailyTokens int64
}

// Config is the fully resolved gateway configuration.
type Config struct {
	ListenAddr      string
	APIKeys         []APIKeyConfig
	RequestTimeout  time.Duration
	Upstreams       []Upstream
	HealthInterval  time.Duration
	HealthPath      string
	HealthFailures  int
	HealthSuccesses int
	RetryAttempts   int
	AdminToken      string
	ShutdownTimeout time.Duration
	MaxBodyBytes    int64
	LogLevel        string
}

// Load resolves the configuration, applying demo friendly defaults.
func Load() Config {
	return Config{
		ListenAddr:      env("MGW_LISTEN_ADDR", ":8080"),
		APIKeys:         parseAPIKeys(),
		RequestTimeout:  durationEnv("MGW_REQUEST_TIMEOUT", 120*time.Second),
		Upstreams:       parseUpstreams(env("MGW_UPSTREAMS", "qwen-demo-01|qwen-demo|http://mock-model-1:8080|100|8;qwen-demo-02|qwen-demo|http://mock-model-2:8080|100|8")),
		HealthInterval:  durationEnv("MGW_HEALTH_INTERVAL", 15*time.Second),
		HealthPath:      env("MGW_HEALTH_PATH", "/healthz"),
		HealthFailures:  intEnv("MGW_HEALTH_FAILURES", 2),
		HealthSuccesses: intEnv("MGW_HEALTH_SUCCESSES", 2),
		RetryAttempts:   intEnv("MGW_RETRY_ATTEMPTS", 2),
		AdminToken:      env("MGW_ADMIN_TOKEN", ""),
		ShutdownTimeout: durationEnv("MGW_SHUTDOWN_TIMEOUT", 10*time.Second),
		MaxBodyBytes:    int64Env("MGW_MAX_BODY_BYTES", 10<<20),
		LogLevel:        strings.ToLower(env("MGW_LOG_LEVEL", "info")),
	}
}

// parseAPIKeys reads MGW_API_KEYS:
//
//	token|tenant|model1,model2|rpm|daily_tokens|name
//
// A single "*" in the model list grants access to every model. When
// MGW_API_KEYS is empty the legacy single-key variables are used so existing
// deployments keep working.
func parseAPIKeys() []APIKeyConfig {
	raw := strings.TrimSpace(os.Getenv("MGW_API_KEYS"))
	if raw == "" {
		models := splitEnv("MGW_ALLOWED_MODELS", "qwen-demo")
		return []APIKeyConfig{{
			Key:         env("MGW_API_KEY", "mgw_live_demo"),
			TenantID:    env("MGW_TENANT_ID", "tenant-demo"),
			Name:        "legacy",
			Models:      models,
			RPM:         intEnv("MGW_RPM", 0),
			DailyTokens: int64Env("MGW_DAILY_TOKENS", 0),
		}}
	}

	var keys []APIKeyConfig
	for _, entry := range strings.Split(raw, ";") {
		parts := strings.Split(entry, "|")
		if len(parts) < 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		tenant := strings.TrimSpace(parts[1])
		if key == "" || tenant == "" {
			continue
		}
		keys = append(keys, APIKeyConfig{
			Key:         key,
			TenantID:    tenant,
			Name:        field(parts, 5),
			Models:      splitList(field(parts, 2)),
			RPM:         atoiDefault(field(parts, 3), 0),
			DailyTokens: int64Default(field(parts, 4), 0),
		})
	}
	return keys
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

func field(parts []string, index int) string {
	if index >= len(parts) {
		return ""
	}
	return strings.TrimSpace(parts[index])
}

func splitList(raw string) []string {
	values := strings.Split(raw, ",")
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			result = append(result, value)
		}
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
	return splitList(env(key, fallback))
}

func durationEnv(key string, fallback time.Duration) time.Duration {
	value := os.Getenv(key)
	if parsed, err := time.ParseDuration(value); err == nil && parsed > 0 {
		return parsed
	}
	return fallback
}

func intEnv(key string, fallback int) int {
	return atoiDefault(os.Getenv(key), fallback)
}

func int64Env(key string, fallback int64) int64 {
	return int64Default(os.Getenv(key), fallback)
}

func atoiDefault(value string, fallback int) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return fallback
	}
	return parsed
}

func int64Default(value string, fallback int64) int64 {
	parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return fallback
	}
	return parsed
}
