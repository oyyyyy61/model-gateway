package auth

import "testing"

// 验证：正确的 Key 能认证并带回租户/策略；错误 Key 被拒绝；通配 Key 放行任意模型。
func TestServiceAuthenticateAndAuthorize(t *testing.T) {
	// 创建两个 Key：一个只允许 qwen-demo，一个允许所有模型。
	service := NewService([]Key{
		NewKey("mgw_live_secret", "tenant-1", "primary", []string{"qwen-demo"}, 60, 1000),
		NewKey("mgw_live_wild", "tenant-2", "wildcard", []string{"*"}, 0, 0),
	})

	// principal 表示认证后的主体信息，ok 表示认证是否成功。
	principal, key, ok := service.Authenticate("Bearer mgw_live_secret")
	// 验证认证结果：租户、Key ID 和策略都应正确返回。
	if !ok || principal.TenantID != "tenant-1" || principal.KeyID != "primary" {
		t.Fatalf("expected authenticated principal, got %#v, %v", principal, ok)
	}
	if key.RPM != 60 || key.DailyTokens != 1000 {
		t.Fatalf("expected key policy on the returned key, got %#v", key)
	}
	// 验证授权结果：只允许白名单模型，拒绝其他模型。
	if !key.Allows("qwen-demo") || key.Allows("other") {
		t.Fatal("unexpected model authorization result")
	}
	// 验证错误的 API Key 被拒绝。
	if _, _, ok := service.Authenticate("Bearer wrong"); ok {
		t.Fatal("wrong API key was accepted")
	}

	// 验证通配 Key 能认证，并放行任意模型。
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

// 不完整或空 Key 不应产生可用 Key。
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
