package gateway

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/oyyyyy61/model-gateway/internal/auth"
	"github.com/oyyyyy61/model-gateway/internal/health"
	"github.com/oyyyyy61/model-gateway/internal/limits"
	"github.com/oyyyyy61/model-gateway/internal/metrics"
	"github.com/oyyyyy61/model-gateway/internal/version"
)

// handler.go 是网关的 HTTP 业务入口：先处理健康检查、指标和管理端点，再按
// /v1/chat/completions 或 /v1/models/{model}/chat/completions 解析模型，执行认证、
// 模型授权、RPM/每日配额，最后交给 Proxy 选实例转发，并记录指标和审计日志。

// HandlerConfig wires the HTTP handler.
type HandlerConfig struct {
	Proxy      *Proxy
	Auth       *auth.Service
	Limits     *limits.Limiter
	Metrics    *metrics.Metrics
	Health     *health.Checker
	AdminToken string
	Version    string
	Logger     *slog.Logger
}

// Handler implements the public gateway API and a small admin surface.
// Handler 实现公开 API 和最小管理面。
type Handler struct {
	proxy      *Proxy
	auth       *auth.Service
	limits     *limits.Limiter
	metrics    *metrics.Metrics
	health     *health.Checker
	adminToken string
	version    string
	logger     *slog.Logger
	started    time.Time
}

// NewHandler applies safe defaults for optional configuration.
// NewHandler 为 Limits/Metrics/Version/Logger 填默认值。
func NewHandler(cfg HandlerConfig) http.Handler {
	handler := &Handler{
		proxy:      cfg.Proxy,
		auth:       cfg.Auth,
		limits:     cfg.Limits,
		metrics:    cfg.Metrics,
		health:     cfg.Health,
		adminToken: cfg.AdminToken,
		version:    cfg.Version,
		logger:     cfg.Logger,
		started:    time.Now(),
	}
	if handler.limits == nil {
		handler.limits = limits.New()
	}
	if handler.metrics == nil {
		handler.metrics = metrics.New()
	}
	if handler.version == "" {
		handler.version = version.Version
	}
	if handler.logger == nil {
		handler.logger = slog.Default()
	}
	return handler
}

// ServeHTTP dispatches operational endpoints and the model API.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 运维端点不要求租户 Key；/metrics 直接输出 Prometheus 文本。
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/healthz":
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
		return
	case r.Method == http.MethodGet && r.URL.Path == "/readyz":
		h.handleReady(w)
		return
	case r.Method == http.MethodGet && r.URL.Path == "/metrics":
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = io.WriteString(w, h.metrics.Render())
		return
	case strings.HasPrefix(r.URL.Path, "/admin/"):
		h.handleAdmin(w, r)
		return
	case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
		h.handleModels(w, r)
		return
	}

	// 解析显式模型路径；标准 OpenAI 路径的 model 在 body 里。
	model, ok := modelFromPath(r.URL.Path)
	canonical := r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions"
	// 检查请求方法是否为 POST，并且路径是否匹配模型路由；不匹配返回 404。
	if r.Method != http.MethodPost || (!ok && !canonical) {
		writeError(w, http.StatusNotFound, "invalid_request", "route not found", requestID(r))
		return
	}

	// 从请求头获取 Authorization，调用 Authenticate 认证，返回主体信息和 Key。
	// 先认证再读 body：未认证请求不能触发大 body 缓冲。
	// Authenticate before reading the body so unauthenticated callers cannot
	// make the gateway buffer large payloads.
	principal, key, authenticated := h.auth.Authenticate(r.Header.Get("Authorization"))
	if !authenticated {
		writeError(w, http.StatusUnauthorized, "unauthorized", "invalid or missing API key", requestID(r))
		return
	}

	// 标准 OpenAI 路径：读取 body 取 model，再把 body 放回给 Proxy 转发。
	if canonical {
		body, parsedModel, err := readModelBody(r, h.proxy.MaxBodyBytes())
		if err != nil {
			// body 超过上限 413；不是合法 JSON 400。
			if errors.Is(err, ErrRequestTooLarge) {
				writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body is too large", requestID(r))
			} else {
				writeError(w, http.StatusBadRequest, "invalid_request", "request body must be a JSON object", requestID(r))
			}
			return
		}
		if parsedModel == "" {
			writeError(w, http.StatusBadRequest, "missing_model", "model is required for /v1/chat/completions", requestID(r))
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		model = parsedModel
	}
	// 检查租户是否有访问指定模型的权限；没有返回 403。
	if !key.Allows(model) {
		writeError(w, http.StatusForbidden, "model_access_denied", "tenant is not allowed to access this model", requestID(r))
		return
	}

	// RPM 令牌桶；超限返回 429 + Retry-After。
	if allowed, wait := h.limits.AllowRequest(principal.KeyHash, key.RPM); !allowed {
		h.metrics.RateLimited.Inc(map[string]string{"tenant": principal.TenantID, "reason": "rpm"})
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(wait)))
		writeError(w, http.StatusTooManyRequests, "rate_limited", "request rate limit exceeded", requestID(r))
		return
	}
	// 每日 token 配额：请求前软检查，响应后累加实际用量。
	if exceeded, used := h.limits.QuotaExceeded(principal.KeyHash, key.DailyTokens); exceeded {
		h.metrics.RateLimited.Inc(map[string]string{"tenant": principal.TenantID, "reason": "daily_tokens"})
		writeError(w, http.StatusTooManyRequests, "quota_exceeded", "daily token quota exceeded ("+strconv.FormatInt(used, 10)+" tokens used)", requestID(r))
		return
	}

	// 进入代理前记录 inflight 和开始时间。
	started := time.Now()
	h.metrics.Inflight.Add(1, map[string]string{"model": model})
	result, err := h.proxy.ServeHTTP(w, r, model)
	h.metrics.Inflight.Add(-1, map[string]string{"model": model})
	duration := time.Since(started)
	h.metrics.Duration.Observe(duration.Seconds(), map[string]string{"model": model})

	// 代理返回后：累加 token 配额，记录 token、请求数和耗时指标。
	if !result.Usage.Empty() {
		h.limits.AddTokens(principal.KeyHash, result.Usage.TotalTokens)
		h.metrics.Tokens.Add(float64(result.Usage.PromptTokens), map[string]string{"tenant": principal.TenantID, "model": model, "kind": "prompt"})
		h.metrics.Tokens.Add(float64(result.Usage.CompletionTokens), map[string]string{"tenant": principal.TenantID, "model": model, "kind": "completion"})
		h.metrics.Tokens.Add(float64(result.Usage.TotalTokens), map[string]string{"tenant": principal.TenantID, "model": model, "kind": "total"})
	}

	status := result.StatusCode
	statusLabel := result.Result
	var code, message string
	// 错误分类；只有响应还没开始写时才补错误 body，避免破坏已发送的 SSE。
	if err != nil {
		var mappedStatus int
		mappedStatus, code, message = classifyProxyError(err)
		if !result.ResponseStarted {
			status = mappedStatus
			statusLabel = code
			// A cancelled client is already gone; writing an error body is futile.
			if mappedStatus != 499 {
				writeError(w, mappedStatus, code, message, requestID(r))
			}
		}
	}
	if statusLabel == "" {
		statusLabel = code
	}
	if statusLabel == "" {
		statusLabel = "unknown"
	}
	h.metrics.Requests.Inc(map[string]string{"model": model, "tenant": principal.TenantID, "status": statusLabel})

	// 审计日志：记录请求 ID、租户、Key、模型、实例、尝试次数、状态、耗时、
	// 首字节、字节数和 token；不记录 prompt/回答正文。
	attributes := []any{
		"request_id", requestID(r),
		"tenant_id", principal.TenantID,
		"key_id", principal.KeyID,
		"model", model,
		"instance_id", result.InstanceID,
		"attempts", result.Attempts,
		"status_code", status,
		"result", statusLabel,
		"stream", result.Stream,
		"duration_ms", duration.Milliseconds(),
		"first_byte_ms", result.FirstByte.Milliseconds(),
		"response_bytes", result.ResponseBytes,
		"prompt_tokens", result.Usage.PromptTokens,
		"completion_tokens", result.Usage.CompletionTokens,
		"total_tokens", result.Usage.TotalTokens,
	}
	if err != nil {
		attributes = append(attributes, "error_code", code, "error", err)
		h.logger.Warn("request audit", attributes...)
		return
	}
	h.logger.Info("request audit", attributes...)
}

// handleReady 处理 /readyz：至少一个上游健康才返回 200，否则 503。
func (h *Handler) handleReady(w http.ResponseWriter) {
	healthy := 0
	if h.health != nil {
		healthy = h.health.HealthyCount()
	} else if h.proxy != nil {
		healthy = h.proxy.picker.HealthyCount()
	}
	status := http.StatusOK
	label := "ready"
	if healthy == 0 {
		status = http.StatusServiceUnavailable
		label = "not_ready"
	}
	writeJSON(w, status, map[string]any{"status": label, "healthy_upstreams": healthy})
}

// handleModels 处理 /v1/models：通配 Key 返回全部已配置模型，否则返回 Key 的白名单。
func (h *Handler) handleModels(w http.ResponseWriter, r *http.Request) {
	principal, key, ok := h.auth.Authenticate(r.Header.Get("Authorization"))
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "invalid or missing API key", requestID(r))
		return
	}
	models := key.ModelList()
	if key.Allows("*") && h.proxy != nil {
		models = h.proxy.picker.Models()
	}
	data := make([]map[string]string, 0, len(models))
	for _, model := range models {
		data = append(data, map[string]string{"id": model, "object": "model", "owned_by": principal.TenantID})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// handleAdmin 处理 /admin/status：未配置 token 时 404，使用恒定时间比较。
func (h *Handler) handleAdmin(w http.ResponseWriter, r *http.Request) {
	if h.adminToken == "" || r.URL.Path != "/admin/status" {
		writeError(w, http.StatusNotFound, "invalid_request", "route not found", requestID(r))
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "invalid_request", "method not allowed", requestID(r))
		return
	}
	if !adminTokenMatches(h.adminToken, r.Header.Get("Authorization")) {
		writeError(w, http.StatusUnauthorized, "unauthorized", "invalid or missing admin token", requestID(r))
		return
	}

	upstreams := []health.Status{}
	if h.health != nil {
		upstreams = h.health.Status()
	}
	models := []string{}
	if h.proxy != nil {
		models = h.proxy.picker.Models()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":        h.version,
		"uptime_seconds": int64(time.Since(h.started).Seconds()),
		"models":         models,
		"keys":           h.auth.Len(),
		"upstreams":      upstreams,
	})
}

// classifyProxyError 把内部错误映射为 HTTP 状态码和错误码。
func classifyProxyError(err error) (int, string, string) {
	switch {
	case errors.Is(err, context.Canceled):
		// 请求被客户端取消，返回 499。
		return 499, "client_cancelled", "client cancelled the request"
	case errors.Is(err, context.DeadlineExceeded):
		// 请求超时，返回 504。
		return http.StatusGatewayTimeout, "upstream_timeout", "upstream request timed out"
	case errors.Is(err, ErrRequestTooLarge):
		return http.StatusRequestEntityTooLarge, "request_too_large", "request body is too large"
	case errors.Is(err, ErrUpstreamUnavailable):
		// 没有健康实例，返回 503。
		return http.StatusServiceUnavailable, "upstream_unavailable", "no healthy model instance is available"
	default:
		// 其他上游错误，返回 502。
		return http.StatusBadGateway, "upstream_error", "upstream model request failed"
	}
}

// adminTokenMatches 支持可选的 "Bearer " 前缀，并使用恒定时间比较。
func adminTokenMatches(expected, header string) bool {
	header = strings.TrimSpace(header)
	if strings.HasPrefix(strings.ToLower(header), "bearer ") {
		header = strings.TrimSpace(header[len("bearer "):])
	}
	return subtle.ConstantTimeCompare([]byte(expected), []byte(header)) == 1
}

// retryAfterSeconds 把等待时间向上取整为秒，至少 1 秒。
func retryAfterSeconds(wait time.Duration) int {
	seconds := int(wait.Seconds())
	if wait > time.Duration(seconds)*time.Second {
		seconds++
	}
	if seconds < 1 {
		seconds = 1
	}
	return seconds
}

// modelFromPath 从 /v1/models/{model}/chat/completions 中取出 model。
func modelFromPath(path string) (string, bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 5 && parts[0] == "v1" && parts[1] == "models" && parts[3] == "chat" && parts[4] == "completions" && parts[2] != "" {
		return parts[2], true
	}
	return "", false
}

// readModelBody buffers an OpenAI request body so the handler can resolve the
// model before proxying. The body is restored by the caller.
// readModelBody 读取 body 并限制大小，返回 model；调用方负责把 body 放回 r.Body。
func readModelBody(r *http.Request, limit int64) ([]byte, string, error) {
	if limit <= 0 {
		limit = 10 << 20
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(body)) > limit {
		return nil, "", ErrRequestTooLarge
	}
	var payload struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, "", err
	}
	return body, strings.TrimSpace(payload.Model), nil
}

// writeError 输出统一的错误 JSON，包含 code、message 和 request_id。
func writeError(w http.ResponseWriter, status int, code, message, requestID string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message, "request_id": requestID}})
}

// writeJSON 统一设置 Content-Type 并写 JSON 响应。
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// requestID 读取中间件注入的 X-Request-ID；缺失时返回占位值。
func requestID(r *http.Request) string {
	if value := r.Header.Get("X-Request-ID"); value != "" {
		return value
	}
	return "req-unknown"
}
