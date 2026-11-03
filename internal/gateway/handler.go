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

	model, ok := modelFromPath(r.URL.Path)
	canonical := r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions"
	if r.Method != http.MethodPost || (!ok && !canonical) {
		writeError(w, http.StatusNotFound, "invalid_request", "route not found", requestID(r))
		return
	}

	// Authenticate before reading the body so unauthenticated callers cannot
	// make the gateway buffer large payloads.
	principal, key, authenticated := h.auth.Authenticate(r.Header.Get("Authorization"))
	if !authenticated {
		writeError(w, http.StatusUnauthorized, "unauthorized", "invalid or missing API key", requestID(r))
		return
	}

	if canonical {
		body, parsedModel, err := readModelBody(r, h.proxy.MaxBodyBytes())
		if err != nil {
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
	if !key.Allows(model) {
		writeError(w, http.StatusForbidden, "model_access_denied", "tenant is not allowed to access this model", requestID(r))
		return
	}

	if allowed, wait := h.limits.AllowRequest(principal.KeyHash, key.RPM); !allowed {
		h.metrics.RateLimited.Inc(map[string]string{"tenant": principal.TenantID, "reason": "rpm"})
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(wait)))
		writeError(w, http.StatusTooManyRequests, "rate_limited", "request rate limit exceeded", requestID(r))
		return
	}
	if exceeded, used := h.limits.QuotaExceeded(principal.KeyHash, key.DailyTokens); exceeded {
		h.metrics.RateLimited.Inc(map[string]string{"tenant": principal.TenantID, "reason": "daily_tokens"})
		writeError(w, http.StatusTooManyRequests, "quota_exceeded", "daily token quota exceeded ("+strconv.FormatInt(used, 10)+" tokens used)", requestID(r))
		return
	}

	started := time.Now()
	h.metrics.Inflight.Add(1, map[string]string{"model": model})
	result, err := h.proxy.ServeHTTP(w, r, model)
	h.metrics.Inflight.Add(-1, map[string]string{"model": model})
	duration := time.Since(started)
	h.metrics.Duration.Observe(duration.Seconds(), map[string]string{"model": model})

	if !result.Usage.Empty() {
		h.limits.AddTokens(principal.KeyHash, result.Usage.TotalTokens)
		h.metrics.Tokens.Add(float64(result.Usage.PromptTokens), map[string]string{"tenant": principal.TenantID, "model": model, "kind": "prompt"})
		h.metrics.Tokens.Add(float64(result.Usage.CompletionTokens), map[string]string{"tenant": principal.TenantID, "model": model, "kind": "completion"})
		h.metrics.Tokens.Add(float64(result.Usage.TotalTokens), map[string]string{"tenant": principal.TenantID, "model": model, "kind": "total"})
	}

	status := result.StatusCode
	statusLabel := result.Result
	var code, message string
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

func classifyProxyError(err error) (int, string, string) {
	switch {
	case errors.Is(err, context.Canceled):
		return 499, "client_cancelled", "client cancelled the request"
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, "upstream_timeout", "upstream request timed out"
	case errors.Is(err, ErrRequestTooLarge):
		return http.StatusRequestEntityTooLarge, "request_too_large", "request body is too large"
	case errors.Is(err, ErrUpstreamUnavailable):
		return http.StatusServiceUnavailable, "upstream_unavailable", "no healthy model instance is available"
	default:
		return http.StatusBadGateway, "upstream_error", "upstream model request failed"
	}
}

func adminTokenMatches(expected, header string) bool {
	header = strings.TrimSpace(header)
	if strings.HasPrefix(strings.ToLower(header), "bearer ") {
		header = strings.TrimSpace(header[len("bearer "):])
	}
	return subtle.ConstantTimeCompare([]byte(expected), []byte(header)) == 1
}

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

func modelFromPath(path string) (string, bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 5 && parts[0] == "v1" && parts[1] == "models" && parts[3] == "chat" && parts[4] == "completions" && parts[2] != "" {
		return parts[2], true
	}
	return "", false
}

// readModelBody buffers an OpenAI request body so the handler can resolve the
// model before proxying. The body is restored by the caller.
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

func writeError(w http.ResponseWriter, status int, code, message, requestID string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message, "request_id": requestID}})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func requestID(r *http.Request) string {
	if value := r.Header.Get("X-Request-ID"); value != "" {
		return value
	}
	return "req-unknown"
}
