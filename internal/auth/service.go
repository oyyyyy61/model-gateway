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
func NewKey(rawKey, tenantID, name string, models []string, rpm int, dailyTokens int64) Key {
	rawKey = strings.TrimSpace(rawKey)
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
func (k *Key) Allows(model string) bool {
	if _, ok := k.Models["*"]; ok {
		return true
	}
	_, ok := k.Models[model]
	return ok
}

// ModelList returns the key's models in stable order.
func (k *Key) ModelList() []string {
	models := make([]string, 0, len(k.Models))
	for model := range k.Models {
		models = append(models, model)
	}
	sort.Strings(models)
	return models
}

// Service authenticates bearer tokens against a static key set.
type Service struct {
	keys map[string]*Key
}

// NewService indexes keys by hash. Entries without a token or tenant are
// ignored so a malformed environment variable never produces a usable key.
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
func (s *Service) Len() int {
	return len(s.keys)
}

// Authenticate validates the Authorization header and returns the matched key.
func (s *Service) Authenticate(header string) (Principal, *Key, bool) {
	token := bearerToken(header)
	if token == "" {
		return Principal{}, nil, false
	}
	hash := HashKey(token)

	// Iterate with a constant-time comparison instead of relying only on a map
	// lookup. This keeps the rejection cost independent of which byte differs
	// if the store is ever swapped for a non-hashed one.
	var matched *Key
	for candidateHash, key := range s.keys {
		if subtle.ConstantTimeCompare([]byte(candidateHash), []byte(hash)) == 1 {
			matched = key
		}
	}
	if matched == nil {
		return Principal{}, nil, false
	}
	return Principal{
		TenantID: matched.TenantID,
		KeyID:    matched.ID,
		KeyHash:  matched.Hash,
		Name:     matched.Name,
	}, matched, true
}

func bearerToken(header string) string {
	header = strings.TrimSpace(header)
	if strings.HasPrefix(strings.ToLower(header), "bearer ") {
		header = strings.TrimSpace(header[len("bearer "):])
	}
	return header
}

// HashKey returns the hex encoded SHA-256 digest stored for a token.
func HashKey(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// Prefix returns a short, non-secret representation of a token for logs.
func Prefix(value string) string {
	if len(value) <= 8 {
		return value
	}
	return value[:8] + "****"
}
