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
	if r.Method != http.MethodPost || !ok {
		writeError(w, http.StatusNotFound, "invalid_request", "route not found", requestID(r))
		return
	}
	principal, authenticated := h.auth.Authenticate(r.Header.Get("Authorization"))
	if !authenticated {
		writeError(w, http.StatusUnauthorized, "unauthorized", "invalid or missing API key", requestID(r))
		return
	}
	if !h.auth.Authorize(model) {
		writeError(w, http.StatusForbidden, "model_access_denied", "tenant is not allowed to access this model", requestID(r))
		return
	}

	started := time.Now()
	result, err := h.proxy.ServeHTTP(w, r, model)
	if err != nil {
		status, code, message := http.StatusBadGateway, "upstream_unavailable", "upstream model is unavailable"
		switch {
		case errors.Is(err, context.Canceled):
			status, code, message = 499, "client_cancelled", "client cancelled the request"
		case errors.Is(err, context.DeadlineExceeded):
			status, code, message = http.StatusGatewayTimeout, "upstream_timeout", "upstream request timed out"
		case errors.Is(err, ErrUpstreamUnavailable):
			status, code, message = http.StatusServiceUnavailable, "upstream_unavailable", "no healthy model instance is available"
		}
		if !result.ResponseStarted {
			writeError(w, status, code, message, requestID(r))
		}
		h.logger.Warn("proxy request failed", "request_id", requestID(r), "tenant_id", principal.TenantID, "model", model, "error_code", code, "duration_ms", time.Since(started).Milliseconds(), "error", err)
		return
	}
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
