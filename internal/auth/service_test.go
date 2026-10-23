package auth

import "testing"

func TestServiceAuthenticateAndAuthorize(t *testing.T) {
	service := NewService("mgw_live_secret", "tenant-1", []string{"qwen-demo"})
	principal, ok := service.Authenticate("Bearer mgw_live_secret")
	if !ok || principal.TenantID != "tenant-1" {
		t.Fatalf("expected authenticated principal, got %#v, %v", principal, ok)
	}
	if !service.Authorize("qwen-demo") || service.Authorize("other") {
		t.Fatal("unexpected model authorization result")
	}
	if _, ok := service.Authenticate("Bearer wrong"); ok {
		t.Fatal("wrong API key was accepted")
	}
}
