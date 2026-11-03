// Package auth resolves bearer tokens into tenants and enforces per-key model
// authorization.
//
// Keys are configured statically for now. The key hash (SHA-256) is what the
// service stores; the plaintext token never leaves the request handling path.
// The database migration in migrations/001_init.up.sql keeps the same shape so
// the static store can later be replaced by a MySQL-backed implementation
// without changing callers.
package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"sort"
	"strings"
)

// Principal is the authenticated identity attached to a request.
// Principal 是认证后的主体信息：租户、Key ID、Key 哈希和显示名称。
type Principal struct {
	TenantID string
	KeyID    string
	KeyHash  string
	Name     string
}

// Key is one configured API key and the policy attached to it.
//
// An empty Models set denies every model. The wildcard model "*" grants access
// to every model exposed by the gateway.
// Key 是一个已配置的 API Key 及其策略；Models 为空表示拒绝所有模型，
// "*" 表示允许网关暴露的全部模型。
type Key struct {
	Hash        string
	ID          string
	TenantID    string
	Name        string
	Models      map[string]struct{}
	RPM         int
	DailyTokens int64
}

// NewKey builds a Key from a plaintext token and its policy.
// NewKey 从明文 token 构建 Key；只保存 SHA-256 哈希，名称缺省时用短前缀。
func NewKey(rawKey, tenantID, name string, models []string, rpm int, dailyTokens int64) Key {
	rawKey = strings.TrimSpace(rawKey)
	// 空 token 返回零值 Key，NewService 会把它过滤掉。
	if rawKey == "" {
		return Key{}
	}
	allowed := make(map[string]struct{}, len(models))
	for _, model := range models {
		if model = strings.TrimSpace(model); model != "" {
			allowed[model] = struct{}{}
		}
	}
	if name == "" {
		name = Prefix(rawKey)
	}
	return Key{
		Hash:        HashKey(rawKey),
		ID:          name,
		TenantID:    strings.TrimSpace(tenantID),
		Name:        name,
		Models:      allowed,
		RPM:         rpm,
		DailyTokens: dailyTokens,
	}
}

// Allows reports whether the key may call the given model.
// Allows 判断 Key 是否允许访问指定模型；"*" 优先放行。
func (k *Key) Allows(model string) bool {
	if _, ok := k.Models["*"]; ok {
		return true
	}
	_, ok := k.Models[model]
	return ok
}

// ModelList returns the key's models in stable order.
// ModelList 返回排序后的模型列表，便于日志和 /v1/models 输出。
func (k *Key) ModelList() []string {
	models := make([]string, 0, len(k.Models))
	for model := range k.Models {
		models = append(models, model)
	}
	sort.Strings(models)
	return models
}

// Service authenticates bearer tokens against a static key set.
// Service 持有按哈希索引的 Key 集合；当前是进程内实现，后续可替换为 MySQL/Redis。
type Service struct {
	keys map[string]*Key
}

// NewService indexes keys by hash. Entries without a token or tenant are
// ignored so a malformed environment variable never produces a usable key.
// NewService 过滤掉没有 token 或租户的 Key，避免错误配置产生可用 Key。
func NewService(keys []Key) *Service {
	indexed := make(map[string]*Key, len(keys))
	for i := range keys {
		key := keys[i]
		if key.Hash == "" || key.TenantID == "" {
			continue
		}
		copy := key
		indexed[key.Hash] = &copy
	}
	return &Service{keys: indexed}
}

// Len reports how many usable keys are configured.
// Len 返回可用 Key 数量。
func (s *Service) Len() int {
	return len(s.keys)
}

// Authenticate validates the Authorization header and returns the matched key.
// Authenticate 解析 Bearer token，只比较哈希，返回主体信息和 Key 策略。
func (s *Service) Authenticate(header string) (Principal, *Key, bool) {
	// 去掉可选的 "Bearer " 前缀；空 token 直接拒绝。
	token := bearerToken(header)
	if token == "" {
		return Principal{}, nil, false
	}
	// 只比较 SHA-256 哈希，明文 token 不落库、不写日志。
	hash := HashKey(token)

	// Iterate with a constant-time comparison instead of relying only on a map
	// lookup. This keeps the rejection cost independent of which byte differs
	// if the store is ever swapped for a non-hashed one.
	// 恒定时间比较，避免根据比较耗时推断 Key。
	var matched *Key
	for candidateHash, key := range s.keys {
		if subtle.ConstantTimeCompare([]byte(candidateHash), []byte(hash)) == 1 {
			matched = key
		}
	}
	if matched == nil {
		return Principal{}, nil, false
	}
	// 认证成功：返回租户、Key ID 和名称。
	return Principal{
		TenantID: matched.TenantID,
		KeyID:    matched.ID,
		KeyHash:  matched.Hash,
		Name:     matched.Name,
	}, matched, true
}

// bearerToken 去掉大小写不敏感的 "Bearer " 前缀并 trim。
func bearerToken(header string) string {
	header = strings.TrimSpace(header)
	if strings.HasPrefix(strings.ToLower(header), "bearer ") {
		header = strings.TrimSpace(header[len("bearer "):])
	}
	return header
}

// HashKey returns the hex encoded SHA-256 digest stored for a token.
// HashKey 返回 SHA-256 的十六进制摘要，作为 Key 的存储形式。
func HashKey(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// Prefix returns a short, non-secret representation of a token for logs.
// Prefix 返回用于日志的短前缀，避免泄露完整 Key。
func Prefix(value string) string {
	if len(value) <= 8 {
		return value
	}
	return value[:8] + "****"
}
