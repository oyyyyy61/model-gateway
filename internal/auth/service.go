// apikey认证、模型访问的授权服务

package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

type Principal struct {
	TenantID string
	KeyID    string
}

type Service struct {
	keyHash       string
	principal     Principal
	allowedModels map[string]struct{}
}

// NewService 创建一个新的Service实例，传入API密钥、租户ID和允许的模型列表
func NewService(apiKey, tenantID string, allowedModels []string) *Service {
	allowed := make(map[string]struct{}, len(allowedModels))
	for _, model := range allowedModels {
		allowed[model] = struct{}{}
	}
	return &Service{keyHash: HashKey(apiKey), principal: Principal{TenantID: tenantID, KeyID: Prefix(apiKey)}, allowedModels: allowed}
}

func (s *Service) Authenticate(value string) (Principal, bool) {
	// TrimSpace去除value前后空格，并检查是否以"Bearer "开头，如果是，则去掉前缀
	value = strings.TrimSpace(value)
	if strings.HasPrefix(strings.ToLower(value), "bearer ") {
		value = strings.TrimSpace(value[len("bearer "):])
	}
	// 检查value是否为空，或者经过HashKey处理后的值是否与存储的keyHash不匹配，如果是，则返回空的Principal和false，表示认证失败
	if value == "" || HashKey(value) != s.keyHash {
		return Principal{}, false
	}
	return s.principal, true
}

func (s *Service) Authorize(model string) bool {
	_, ok := s.allowedModels[model]
	return ok
}

func HashKey(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func Prefix(value string) string {
	if len(value) <= 8 {
		return value
	}
	return value[:8] + "****"
}
