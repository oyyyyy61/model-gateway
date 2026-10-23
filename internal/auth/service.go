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

func NewService(apiKey, tenantID string, allowedModels []string) *Service {
	allowed := make(map[string]struct{}, len(allowedModels))
	for _, model := range allowedModels {
		allowed[model] = struct{}{}
	}
	return &Service{keyHash: HashKey(apiKey), principal: Principal{TenantID: tenantID, KeyID: Prefix(apiKey)}, allowedModels: allowed}
}

func (s *Service) Authenticate(value string) (Principal, bool) {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(strings.ToLower(value), "bearer ") {
		value = strings.TrimSpace(value[len("bearer "):])
	}
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
