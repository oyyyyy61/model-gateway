// 用于验证服务，确保在传入正确的API密钥和租户ID时，服务能够正确地进行认证和授权。

package auth

import "testing"

func TestServiceAuthenticateAndAuthorize(t *testing.T) {
	// 传入服务的apiKey、租户ID和允许的模型列表，创建一个新的Service实例
	service := NewService("mgw_live_secret", "tenant-1", []string{"qwen-demo"})
	// principal表示认证后的主体信息，ok表示认证是否成功
	principal, ok := service.Authenticate("Bearer mgw_live_secret")
	// 验证认证结果是否符合预期，确保返回的主体信息和认证状态正确
	if !ok || principal.TenantID != "tenant-1" {
		t.Fatalf("expected authenticated principal, got %#v, %v", principal, ok)
	}
	// 验证授权结果，确保服务能够正确地判断模型是否被允许
	if !service.Authorize("qwen-demo") || service.Authorize("other") {
		t.Fatal("unexpected model authorization result")
	}
	// 验证错误的API密钥是否被拒绝，确保服务能够正确地处理无效的认证请求
	if _, ok := service.Authenticate("Bearer wrong"); ok {
		t.Fatal("wrong API key was accepted")
	}
}
