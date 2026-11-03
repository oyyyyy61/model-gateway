package gateway

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/oyyyyy61/model-gateway/internal/auth"
	"github.com/oyyyyy61/model-gateway/internal/limits"
	"github.com/oyyyyy61/model-gateway/internal/metrics"
	"github.com/oyyyyy61/model-gateway/internal/routing"
)

type testSetup struct {
	handler http.Handler
	picker  *routing.Picker
}

func newTestSetup(t *testing.T, upstream http.Handler, key auth.Key, proxyMutate func(*ProxyConfig), handlerMutate func(*HandlerConfig)) testSetup {
	t.Helper()
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)

	picker := routing.NewPicker([]routing.Instance{{ID: "test", Model: "demo", Endpoint: server.URL, Weight: 1, MaxInflight: 8}})
	registry := metrics.New()
	proxyConfig := ProxyConfig{
		Picker:         picker,
		Client:         server.Client(),
		RequestTimeout: time.Second,
		Logger:         slog.Default(),
		MaxAttempts:    2,
		MaxBodyBytes:   1 << 20,
		Metrics:        registry,
	}
	if proxyMutate != nil {
		proxyMutate(&proxyConfig)
	}
	handlerConfig := HandlerConfig{
		Proxy:   NewProxy(proxyConfig),
		Auth:    auth.NewService([]auth.Key{key}),
		Limits:  limits.New(),
		Metrics: registry,
		Logger:  slog.Default(),
	}
	if handlerMutate != nil {
		handlerMutate(&handlerConfig)
	}
	return testSetup{handler: NewHandler(handlerConfig), picker: picker}
}

func testKey(models []string, rpm int, dailyTokens int64) auth.Key {
	return auth.NewKey("secret", "tenant-1", "test-key", models, rpm, dailyTokens)
}

func authorizedChat(body string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/v1/models/demo/chat/completions", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("X-Request-ID", "req_test")
	return request
}

func jsonUpstream(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	})
}

func TestHandlerProxiesJSONAndEnforcesAuth(t *testing.T) {
	setup := newTestSetup(t, jsonUpstream(`{"ok":true}`), testKey([]string{"demo"}, 0, 0), nil, nil)

	unauthorized := httptest.NewRecorder()
	setup.handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodPost, "/v1/models/demo/chat/completions", strings.NewReader(`{"messages":[]}`)))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", unauthorized.Code)
	}

	recorder := httptest.NewRecorder()
	setup.handler.ServeHTTP(recorder, authorizedChat(`{"messages":[]}`))
	if recorder.Code != http.StatusOK || recorder.Body.String() != `{"ok":true}` {
		t.Fatalf("unexpected proxy response: status=%d body=%q", recorder.Code, recorder.Body.String())
	}

	denied := newTestSetup(t, jsonUpstream(`{"ok":true}`), testKey([]string{"other"}, 0, 0), nil, nil)
	recorder = httptest.NewRecorder()
	denied.handler.ServeHTTP(recorder, authorizedChat(`{"messages":[]}`))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a model outside the key policy, got %d", recorder.Code)
	}
}

func TestHandlerProxiesSSE(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: hello\n\n")
		flusher.Flush()
	})
	setup := newTestSetup(t, upstream, testKey([]string{"demo"}, 0, 0), nil, nil)
	recorder := httptest.NewRecorder()
	setup.handler.ServeHTTP(recorder, authorizedChat(`{"stream":true}`))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "data: hello") {
		t.Fatalf("unexpected SSE response: status=%d body=%q", recorder.Code, recorder.Body.String())
	}
}

func TestHandlerAcceptsCanonicalOpenAIPath(t *testing.T) {
	setup := newTestSetup(t, jsonUpstream(`{"ok":true}`), testKey([]string{"demo"}, 0, 0), nil, nil)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"demo","messages":[]}`))
	request.Header.Set("Authorization", "Bearer secret")
	recorder := httptest.NewRecorder()
	setup.handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Body.String() != `{"ok":true}` {
		t.Fatalf("unexpected response: %d %q", recorder.Code, recorder.Body.String())
	}
}

func TestHandlerRequiresModelOnCanonicalPath(t *testing.T) {
	setup := newTestSetup(t, jsonUpstream(`{"ok":true}`), testKey([]string{"demo"}, 0, 0), nil, nil)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"messages":[]}`))
	request.Header.Set("Authorization", "Bearer secret")
	recorder := httptest.NewRecorder()
	setup.handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "missing_model") {
		t.Fatalf("expected missing_model, got %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestHandlerRateLimitsRequests(t *testing.T) {
	setup := newTestSetup(t, jsonUpstream(`{"ok":true}`), testKey([]string{"demo"}, 1, 0), nil, nil)

	first := httptest.NewRecorder()
	setup.handler.ServeHTTP(first, authorizedChat(`{"messages":[]}`))
	if first.Code != http.StatusOK {
		t.Fatalf("expected the first request to pass, got %d", first.Code)
	}

	second := httptest.NewRecorder()
	setup.handler.ServeHTTP(second, authorizedChat(`{"messages":[]}`))
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", second.Code)
	}
	if second.Header().Get("Retry-After") == "" {
		t.Fatal("expected a Retry-After header")
	}
	if !strings.Contains(second.Body.String(), "rate_limited") {
		t.Fatalf("unexpected body: %s", second.Body.String())
	}
}

func TestHandlerEnforcesDailyTokenQuota(t *testing.T) {
	upstream := jsonUpstream(`{"usage":{"prompt_tokens":3,"completion_tokens":7,"total_tokens":10}}`)
	setup := newTestSetup(t, upstream, testKey([]string{"demo"}, 0, 5), nil, nil)

	first := httptest.NewRecorder()
	setup.handler.ServeHTTP(first, authorizedChat(`{"messages":[]}`))
	if first.Code != http.StatusOK {
		t.Fatalf("expected the first request to pass, got %d", first.Code)
	}

	second := httptest.NewRecorder()
	setup.handler.ServeHTTP(second, authorizedChat(`{"messages":[]}`))
	if second.Code != http.StatusTooManyRequests || !strings.Contains(second.Body.String(), "quota_exceeded") {
		t.Fatalf("expected quota rejection, got %d %s", second.Code, second.Body.String())
	}
}

func TestHandlerListsModelsForTenant(t *testing.T) {
	setup := newTestSetup(t, jsonUpstream(`{}`), testKey([]string{"*"}, 0, 0), nil, nil)

	unauthorized := httptest.NewRecorder()
	setup.handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", unauthorized.Code)
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer secret")
	setup.handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"id":"demo"`) {
		t.Fatalf("unexpected models response: %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestAdminStatusRequiresToken(t *testing.T) {
	setup := newTestSetup(t, jsonUpstream(`{}`), testKey([]string{"demo"}, 0, 0), nil, func(cfg *HandlerConfig) {
		cfg.AdminToken = "admin-secret"
	})

	unauthorized := httptest.NewRecorder()
	setup.handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/admin/status", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", unauthorized.Code)
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/admin/status", nil)
	request.Header.Set("Authorization", "Bearer admin-secret")
	setup.handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"version"`) {
		t.Fatalf("unexpected admin response: %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestHandlerRejectsOversizedBody(t *testing.T) {
	setup := newTestSetup(t, jsonUpstream(`{}`), testKey([]string{"demo"}, 0, 0), func(cfg *ProxyConfig) {
		cfg.MaxBodyBytes = 8
	}, nil)

	recorder := httptest.NewRecorder()
	setup.handler.ServeHTTP(recorder, authorizedChat(`{"messages":[]}`))
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestReadyzReflectsUpstreamHealth(t *testing.T) {
	setup := newTestSetup(t, jsonUpstream(`{}`), testKey([]string{"demo"}, 0, 0), nil, nil)

	ready := httptest.NewRecorder()
	setup.handler.ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if ready.Code != http.StatusOK {
		t.Fatalf("expected ready, got %d", ready.Code)
	}

	setup.picker.SetHealth("test", false)
	notReady := httptest.NewRecorder()
	setup.handler.ServeHTTP(notReady, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if notReady.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", notReady.Code)
	}
}
