package middleware

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChainPropagatesRequestID(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))

	var seen string
	handler := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestID(r.Context())
		w.WriteHeader(http.StatusOK)
	}), logger)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if seen == "" {
		t.Fatal("expected the handler to receive a request id")
	}
	if got := recorder.Header().Get("X-Request-ID"); got != seen {
		t.Fatalf("expected response header %q, got %q", seen, got)
	}
	if !strings.Contains(output.String(), "request_id="+seen) {
		t.Fatalf("access log did not include the request id: %s", output.String())
	}
}

func TestChainKeepsIncomingRequestID(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	var seen string
	handler := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestID(r.Context())
	}), logger)

	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	request.Header.Set("X-Request-ID", "req_client")
	handler.ServeHTTP(httptest.NewRecorder(), request)
	if seen != "req_client" {
		t.Fatalf("expected incoming request id to be preserved, got %q", seen)
	}
}
