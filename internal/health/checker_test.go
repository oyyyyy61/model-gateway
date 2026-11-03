package health

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oyyyyy61/model-gateway/internal/metrics"
	"github.com/oyyyyy61/model-gateway/internal/routing"
)

func TestCheckerMarksUnhealthyAndRecovers(t *testing.T) {
	var failing atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		if failing.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	picker := routing.NewPicker([]routing.Instance{{ID: "one", Model: "demo", Endpoint: server.URL, Weight: 1, MaxInflight: 1}})
	checker := New(Config{
		Picker:    picker,
		Client:    server.Client(),
		Interval:  time.Second,
		Path:      "/healthz",
		Failures:  1,
		Successes: 1,
		Metrics:   metrics.New(),
	})
	ctx := context.Background()

	checker.CheckOnce(ctx)
	if picker.Healthy("demo") != 1 {
		t.Fatal("expected the instance to stay healthy")
	}

	failing.Store(true)
	checker.CheckOnce(ctx)
	if picker.Healthy("demo") != 0 {
		t.Fatal("expected one failed probe to mark the instance unhealthy")
	}

	failing.Store(false)
	checker.CheckOnce(ctx)
	if picker.Healthy("demo") != 1 {
		t.Fatal("expected one successful probe to restore the instance")
	}

	statuses := checker.Status()
	if len(statuses) != 1 || statuses[0].ID != "one" || !statuses[0].Healthy {
		t.Fatalf("unexpected status: %#v", statuses)
	}
}

func TestCheckerStopsWithContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	picker := routing.NewPicker([]routing.Instance{{ID: "one", Model: "demo", Endpoint: server.URL, Weight: 1, MaxInflight: 1}})
	checker := New(Config{Picker: picker, Client: server.Client(), Interval: 10 * time.Millisecond, Path: "/healthz"})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		checker.Run(ctx)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("checker did not stop after context cancellation")
	}
}
