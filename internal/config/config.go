// config 包从环境变量加载网关配置。
//
// 网关不依赖配置文件：Docker Compose、K8s ConfigMap 和测试都可以设置
// 同一套环境变量。
package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Upstream 描述一个可路由的模型实例。
type Upstream struct {
	ID       string
	Model    string
	Endpoint string
	Weight   int
	// MaxInflight 限制实例同时处理的请求数量。
	MaxInflight int
}

// APIKeyConfig 对应 MGW_API_KEYS 的一项。
type APIKeyConfig struct {
	Key         string
	TenantID    string
	Name        string
	Models      []string
	RPM         int
	DailyTokens int64
}

// Config 是解析后的完整配置。
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

// Load 读取环境变量并填充默认值；非法值会回退到默认值。
func Load() Config {
	return Config{
		ListenAddr: env("MGW_LISTEN_ADDR", ":8080"),
		// 多租户 Key；为空时回退到旧版单 Key 变量。
		APIKeys:        parseAPIKeys(),
		RequestTimeout: durationEnv("MGW_REQUEST_TIMEOUT", 120*time.Second),
		// 上游实例列表；格式 id|model|endpoint|weight|max_inflight。
		Upstreams:       parseUpstreams(env("MGW_UPSTREAMS", "qwen-demo-01|qwen-demo|http://mock-model-1:8080|100|8;qwen-demo-02|qwen-demo|http://mock-model-2:8080|100|8")),
		HealthInterval:  durationEnv("MGW_HEALTH_INTERVAL", 15*time.Second),
		HealthPath:      env("MGW_HEALTH_PATH", "/healthz"),
		HealthFailures:  intEnv("MGW_HEALTH_FAILURES", 2),
		HealthSuccesses: intEnv("MGW_HEALTH_SUCCESSES", 2),
		RetryAttempts:   intEnv("MGW_RETRY_ATTEMPTS", 2),
		// 管理端点 Token；为空时 /admin/status 返回 404。
		AdminToken:      env("MGW_ADMIN_TOKEN", ""),
		ShutdownTimeout: durationEnv("MGW_SHUTDOWN_TIMEOUT", 10*time.Second),
		// 请求体上限，默认 10 MiB。
		MaxBodyBytes: int64Env("MGW_MAX_BODY_BYTES", 10<<20),
		// 日志级别：debug/info/warn/error。
		LogLevel: strings.ToLower(env("MGW_LOG_LEVEL", "info")),
	}
}

// parseAPIKeys 解析 MGW_API_KEYS：
//
//	token|tenant|model1,model2|rpm|daily_tokens|name
//
// 模型列表里的 "*" 表示允许所有模型；MGW_API_KEYS 为空时回退到旧版
// 单 Key 变量，保证已有部署继续工作。
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
	// 分号分隔多个 Key；字段不足或 token/tenant 为空的条目直接跳过。
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

// parseUpstreams 解析 id|model|endpoint|weight|max_inflight；非法条目跳过。
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

// field 返回 parts[index]，越界或空白时返回空串。
func field(parts []string, index int) string {
	if index >= len(parts) {
		return ""
	}
	return strings.TrimSpace(parts[index])
}

// splitList 按逗号分割并去掉每项空白。
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

// env 读取非空环境变量，否则返回默认值。
func env(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

// splitEnv 把环境变量按逗号分割成列表。
func splitEnv(key, fallback string) []string {
	return splitList(env(key, fallback))
}

// durationEnv 解析 Go duration；非法或非正数回退默认值。
func durationEnv(key string, fallback time.Duration) time.Duration {
	value := os.Getenv(key)
	if parsed, err := time.ParseDuration(value); err == nil && parsed > 0 {
		return parsed
	}
	return fallback
}

// intEnv 读取整数环境变量，非法值回退默认值。
func intEnv(key string, fallback int) int {
	return atoiDefault(os.Getenv(key), fallback)
}

// int64Env 读取 int64 环境变量，非法值回退默认值。
func int64Env(key string, fallback int64) int64 {
	return int64Default(os.Getenv(key), fallback)
}

// atoiDefault 解析 int，失败时返回默认值。
func atoiDefault(value string, fallback int) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return fallback
	}
	return parsed
}

// int64Default 解析 int64，失败时返回默认值。
func int64Default(value string, fallback int64) int64 {
	parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return fallback
	}
	return parsed
}
