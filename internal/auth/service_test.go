package auth

import "testing"

func TestServiceAuthenticateAndAuthorize(t *testing.T) {
	service := NewService([]Key{
		NewKey("mgw_live_secret", "tenant-1", "primary", []string{"qwen-demo"}, 60, 1000),
		NewKey("mgw_live_wild", "tenant-2", "wildcard", []string{"*"}, 0, 0),
	})

	principal, key, ok := service.Authenticate("Bearer mgw_live_secret")
	if !ok || principal.TenantID != "tenant-1" || principal.KeyID != "primary" {
		t.Fatalf("expected authenticated principal, got %#v, %v", principal, ok)
	}
	if key.RPM != 60 || key.DailyTokens != 1000 {
		t.Fatalf("expected key policy on the returned key, got %#v", key)
	}
	if !key.Allows("qwen-demo") || key.Allows("other") {
		t.Fatal("unexpected model authorization result")
	}
	if _, _, ok := service.Authenticate("Bearer wrong"); ok {
		t.Fatal("wrong API key was accepted")
	}

	wildPrincipal, wildKey, ok := service.Authenticate("mgw_live_wild")
	if !ok || wildPrincipal.TenantID != "tenant-2" {
		t.Fatalf("expected wildcard tenant, got %#v, %v", wildPrincipal, ok)
	}
	if !wildKey.Allows("any-model") {
		t.Fatal("wildcard key should allow every model")
	}
	if service.Len() != 2 {
		t.Fatalf("expected 2 keys, got %d", service.Len())
	}
}

func TestNewServiceDropsIncompleteKeys(t *testing.T) {
	service := NewService([]Key{
		{Hash: "abc"},
		NewKey("", "tenant-1", "empty", []string{"demo"}, 0, 0),
		NewKey("   ", "tenant-2", "blank", []string{"demo"}, 0, 0),
	})
	if service.Len() != 0 {
		t.Fatalf("expected no usable keys, got %d", service.Len())
	}
}
