package metrics

import (
	"strings"
	"testing"
)

func TestRenderCountersGaugesAndHistograms(t *testing.T) {
	registry := New()
	registry.Requests.Inc(map[string]string{"model": "demo", "tenant": "tenant-1", "status": "success"})
	registry.Requests.Inc(map[string]string{"model": "demo", "tenant": "tenant-1", "status": "success"})
	registry.Inflight.Set(2, map[string]string{"model": "demo"})
	registry.Duration.Observe(0.02, map[string]string{"model": "demo"})
	registry.Duration.Observe(0.4, map[string]string{"model": "demo"})
	registry.BuildInfo.Set(1, map[string]string{"version": "test"})

	output := registry.Render()
	checks := []string{
		"# TYPE gateway_requests_total counter",
		`gateway_requests_total{model="demo",tenant="tenant-1",status="success"} 2`,
		"# TYPE gateway_request_duration_seconds histogram",
		`gateway_request_duration_seconds_bucket{model="demo",le="0.025"} 1`,
		`gateway_request_duration_seconds_bucket{model="demo",le="0.5"} 2`,
		`gateway_request_duration_seconds_bucket{model="demo",le="+Inf"} 2`,
		`gateway_request_duration_seconds_count{model="demo"} 2`,
		`gateway_inflight_requests{model="demo"} 2`,
		`gateway_build_info{version="test"} 1`,
	}
	for _, check := range checks {
		if !strings.Contains(output, check) {
			t.Fatalf("rendered metrics missing %q:\n%s", check, output)
		}
	}
}

func TestRenderEscapesLabelValues(t *testing.T) {
	registry := New()
	registry.Requests.Inc(map[string]string{"model": `demo"x`, "tenant": "a\\b", "status": "success"})
	output := registry.Render()
	if !strings.Contains(output, `model="demo\"x"`) {
		t.Fatalf("expected escaped quote in labels:\n%s", output)
	}
	if !strings.Contains(output, `tenant="a\\b"`) {
		t.Fatalf("expected escaped backslash in labels:\n%s", output)
	}
}
