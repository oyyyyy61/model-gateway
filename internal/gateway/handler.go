// handler.go 是网关的 HTTP 业务入口。
// 它先处理健康检查和路由解析，然后执行 API Key 认证与模型授权。认证成功后把请求交给 Proxy，由 Proxy 完成实例选择和上游转发。
// Handler 负责将 context.Canceled、请求超时和无健康实例等内部错误转换成统一的 HTTP 错误响应，并记录租户、模型、请求耗时和首字节延迟。
// 对于 SSE 请求，代码通过 ResponseStarted 判断响应是否已经开始，避免流式数据发送后再次写入 JSON 错误。

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/oyyyyy61/model-gateway/internal/auth"
)

type Handler struct {
	proxy  *Proxy
	auth   *auth.Service
	logger *slog.Logger
}

func NewHandler(proxy *Proxy, authService *auth.Service, logger *slog.Logger) http.Handler {
	return &Handler{proxy: proxy, auth: authService, logger: logger}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/readyz" {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ready"})
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/metrics" {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte("# HELP gateway_up 1 when the gateway is serving requests\n# TYPE gateway_up gauge\ngateway_up 1\n"))
		return
	}

	model, ok := modelFromPath(r.URL.Path)
	// 检查请求方法是否为POST，并且路径是否匹配模型路由，如果不匹配，则返回404错误
	if r.Method != http.MethodPost || !ok {
		writeError(w, http.StatusNotFound, "invalid_request", "route not found", requestID(r))
		return
	}
	// 从请求头中获取Authorization字段，并调用Authenticate方法进行认证，返回主体信息和认证状态
	principal, authenticated := h.auth.Authenticate(r.Header.Get("Authorization"))
	if !authenticated {
		writeError(w, http.StatusUnauthorized, "unauthorized", "invalid or missing API key", requestID(r))
		return
	}
	// 检查租户是否有访问指定模型的权限，如果没有，则返回403错误
	if !h.auth.Authorize(model) {
		writeError(w, http.StatusForbidden, "model_access_denied", "tenant is not allowed to access this model", requestID(r))
		return
	}

	started := time.Now()
	result, err := h.proxy.ServeHTTP(w, r, model)
	// 错误分类，根据不同的错误类型返回不同的HTTP状态码和错误信息，并记录日志
	if err != nil {
		status, code, message := http.StatusBadGateway, "upstream_unavailable", "upstream model is unavailable"
		switch {
			// 请求被客户端取消，返回499状态码
		case errors.Is(err, context.Canceled):
			status, code, message = 499, "client_cancelled", "client cancelled the request"
			// 请求超时，返回504状态码
		case errors.Is(err, context.DeadlineExceeded):
			status, code, message = http.StatusGatewayTimeout, "upstream_timeout", "upstream request timed out"
			// 上游服务返回错误，返回502状态码
		case errors.Is(err, ErrUpstreamUnavailable):
			status, code, message = http.StatusServiceUnavailable, "upstream_unavailable", "no healthy model instance is available"
		}
		// 如果响应还没有开始写入，则返回错误信息给客户端
		if !result.ResponseStarted {
			writeError(w, status, code, message, requestID(r))
		}
		// 错误日志，记录请求ID、租户ID、模型、错误码、处理时间和错误信息
		h.logger.Warn("proxy request failed", "request_id", requestID(r), "tenant_id", principal.TenantID, "model", model, "error_code", code, "duration_ms", time.Since(started).Milliseconds(), "error", err)
		return
	}
	// 成功日志，记录请求ID、租户ID、模型、HTTP状态码、处理时间、首字节时间和响应字节数
	h.logger.Info("proxy request completed", "request_id", requestID(r), "tenant_id", principal.TenantID, "key_id", principal.KeyID, "model", model, "status_code", result.StatusCode, "result", result.Result, "stream", result.Stream, "duration_ms", time.Since(started).Milliseconds(), "first_byte_ms", result.FirstByte.Milliseconds(), "response_bytes", result.ResponseBytes)
}

func modelFromPath(path string) (string, bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 5 && parts[0] == "v1" && parts[1] == "models" && parts[3] == "chat" && parts[4] == "completions" && parts[2] != "" {
		return parts[2], true
	}
	return "", false
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
