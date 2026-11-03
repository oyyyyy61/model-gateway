package gateway

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/oyyyyy61/model-gateway/internal/metrics"
	"github.com/oyyyyy61/model-gateway/internal/routing"
)

func TestProxyRewritesExplicitRouteForOpenAIUpstreams(t *testing.T) {
	var gotPath string
	var gotModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		var payload struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		gotModel = payload.Model
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	picker := routing.NewPicker([]routing.Instance{{ID: "openai", Model: "demo", Endpoint: upstream.URL, Weight: 1, MaxInflight: 1}})
	proxy := NewProxy(ProxyConfig{Picker: picker, Client: upstream.Client(), MaxAttempts: 1})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/models/demo/chat/completions", strings.NewReader(`{"messages":[]}`))
	if _, err := proxy.ServeHTTP(recorder, request, "demo"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("expected canonical upstream path, got %q", gotPath)
	}
	if gotModel != "demo" {
		t.Fatalf("expected the model to be injected, got %q", gotModel)
	}
}

func TestProxyFailsOverToAnotherInstance(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":"busy"}`)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer good.Close()

	picker := routing.NewPicker([]routing.Instance{
		{ID: "bad", Model: "demo", Endpoint: bad.URL, Weight: 1, MaxInflight: 1},
		{ID: "good", Model: "demo", Endpoint: good.URL, Weight: 1, MaxInflight: 1},
	})
	proxy := NewProxy(ProxyConfig{
		Picker:         picker,
		Client:         good.Client(),
		RequestTimeout: time.Second,
		Logger:         slog.Default(),
		MaxAttempts:    2,
		Metrics:        metrics.New(),
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/models/demo/chat/completions", strings.NewReader(`{"messages":[]}`))
	result, err := proxy.ServeHTTP(recorder, request, "demo")
	if err != nil {
		t.Fatalf("expected failover to succeed, got %v", err)
	}
	if recorder.Code != http.StatusOK || recorder.Body.String() != `{"ok":true}` {
		t.Fatalf("unexpected response: %d %q", recorder.Code, recorder.Body.String())
	}
	if result.Attempts != 2 || result.InstanceID != "good" {
		t.Fatalf("expected two attempts on the good instance, got %#v", result)
	}
}

func TestProxyFailsOverAfterTransportError(t *testing.T) {
	closed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	closedURL := closed.URL
	closed.Close()

	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer good.Close()

	picker := routing.NewPicker([]routing.Instance{
		{ID: "closed", Model: "demo", Endpoint: closedURL, Weight: 1, MaxInflight: 1},
		{ID: "good", Model: "demo", Endpoint: good.URL, Weight: 1, MaxInflight: 1},
	})
	proxy := NewProxy(ProxyConfig{
		Picker:      picker,
		Client:      &http.Client{Timeout: time.Second},
		MaxAttempts: 2,
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/models/demo/chat/completions", strings.NewReader(`{"messages":[]}`))
	result, err := proxy.ServeHTTP(recorder, request, "demo")
	if err != nil {
		t.Fatalf("expected failover to succeed, got %v", err)
	}
	if recorder.Code != http.StatusOK || result.InstanceID != "good" || result.Attempts != 2 {
		t.Fatalf("unexpected failover result: %#v, body=%q", result, recorder.Body.String())
	}
	if picker.Healthy("demo") != 1 {
		t.Fatalf("expected the closed instance to be removed from the healthy set")
	}
}

func TestProxyCapturesUsageFromStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		flusher.Flush()
		_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3,\"total_tokens\":5}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()

	picker := routing.NewPicker([]routing.Instance{{ID: "stream", Model: "demo", Endpoint: upstream.URL, Weight: 1, MaxInflight: 1}})
	proxy := NewProxy(ProxyConfig{Picker: picker, Client: upstream.Client(), MaxAttempts: 1})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/models/demo/chat/completions", strings.NewReader(`{"stream":true}`))
	result, err := proxy.ServeHTTP(recorder, request, "demo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Usage.TotalTokens != 5 || result.Usage.CompletionTokens != 3 {
		t.Fatalf("expected stream usage to be captured, got %#v", result.Usage)
	}
}
