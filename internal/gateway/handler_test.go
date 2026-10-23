package gateway

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"model-gateway/internal/auth"
	"model-gateway/internal/routing"
)

func TestHandlerProxiesJSONAndEnforcesAuth(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	picker := routing.NewPicker([]routing.Instance{{ID: "test", Model: "demo", Endpoint: upstream.URL, Weight: 1, MaxInflight: 1}})
	proxy := NewProxy(ProxyConfig{Picker: picker, Client: upstream.Client(), RequestTimeout: time.Second, Logger: slog.Default()})
	handler := NewHandler(proxy, auth.NewService("secret", "tenant-1", []string{"demo"}), slog.Default())

	unauthorized := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/models/demo/chat/completions", strings.NewReader(`{"messages":[]}`))
	handler.ServeHTTP(unauthorized, request)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", unauthorized.Code)
	}

	recorder := httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/v1/models/demo/chat/completions", strings.NewReader(`{"messages":[]}`))
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("X-Request-ID", "req_test")
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Body.String() != `{"ok":true}` {
		t.Fatalf("unexpected proxy response: status=%d body=%q", recorder.Code, recorder.Body.String())
	}
}

func TestHandlerProxiesSSE(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: hello\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()

	picker := routing.NewPicker([]routing.Instance{{ID: "test", Model: "demo", Endpoint: upstream.URL, Weight: 1, MaxInflight: 1}})
	proxy := NewProxy(ProxyConfig{Picker: picker, Client: upstream.Client(), RequestTimeout: time.Second, Logger: slog.Default()})
	handler := NewHandler(proxy, auth.NewService("secret", "tenant-1", []string{"demo"}), slog.Default())
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/models/demo/chat/completions", strings.NewReader(`{"stream":true}`))
	request.Header.Set("Authorization", "Bearer secret")
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "data: hello") {
		t.Fatalf("unexpected SSE response: status=%d body=%q", recorder.Code, recorder.Body.String())
	}
}
