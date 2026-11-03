package config

import "testing"

func TestParseAPIKeys(t *testing.T) {
	t.Setenv("MGW_API_KEYS", "key-one|tenant-a|m1,m2|60|1000|alpha;key-two|tenant-b|*|0|0|beta")
	cfg := Load()
	if len(cfg.APIKeys) != 2 {
		t.Fatalf("expected 2 keys, got %#v", cfg.APIKeys)
	}
	first := cfg.APIKeys[0]
	if first.Key != "key-one" || first.TenantID != "tenant-a" || first.Name != "alpha" {
		t.Fatalf("unexpected first key: %#v", first)
	}
	if first.RPM != 60 || first.DailyTokens != 1000 || len(first.Models) != 2 || first.Models[1] != "m2" {
		t.Fatalf("unexpected first key policy: %#v", first)
	}
	second := cfg.APIKeys[1]
	if len(second.Models) != 1 || second.Models[0] != "*" {
		t.Fatalf("expected wildcard models, got %#v", second.Models)
	}
}

func TestLegacyAPIKeyFallback(t *testing.T) {
	t.Setenv("MGW_API_KEYS", "")
	t.Setenv("MGW_API_KEY", "legacy-key")
	t.Setenv("MGW_TENANT_ID", "legacy-tenant")
	t.Setenv("MGW_ALLOWED_MODELS", "m1,m2")
	cfg := Load()
	if len(cfg.APIKeys) != 1 {
		t.Fatalf("expected one legacy key, got %#v", cfg.APIKeys)
	}
	key := cfg.APIKeys[0]
	if key.Key != "legacy-key" || key.TenantID != "legacy-tenant" || len(key.Models) != 2 {
		t.Fatalf("unexpected legacy key: %#v", key)
	}
}

func TestParseUpstreamsDropsInvalidEntries(t *testing.T) {
	t.Setenv("MGW_UPSTREAMS", "one|demo|http://one:8080|10|2;broken;two|demo|http://two:8080|0|2")
	cfg := Load()
	if len(cfg.Upstreams) != 1 || cfg.Upstreams[0].ID != "one" {
		t.Fatalf("expected only the valid upstream, got %#v", cfg.Upstreams)
	}
}

func TestLoadDurationsAndLimits(t *testing.T) {
	t.Setenv("MGW_HEALTH_INTERVAL", "3s")
	t.Setenv("MGW_HEALTH_FAILURES", "4")
	t.Setenv("MGW_RETRY_ATTEMPTS", "3")
	cfg := Load()
	if cfg.HealthInterval.Seconds() != 3 || cfg.HealthFailures != 4 || cfg.RetryAttempts != 3 {
		t.Fatalf("unexpected config: %#v", cfg)
	}
}
