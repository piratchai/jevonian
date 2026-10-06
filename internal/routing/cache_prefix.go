package routing

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/xinyao27/jevonian/internal/config"
)

// CachePrefix records hashes of request content that may affect prompt reuse.
// It does not prove that an upstream vendor retained or reused a cache.
type CachePrefix struct {
	Known       bool     `json:"known"`
	StableHash  string   `json:"stableHash,omitempty"`
	MessageHash []string `json:"messageHash,omitempty"`
}

const maxCachePrefixMessages = 128

// BuildCachePrefix hashes conservative prepared-body evidence. It stores no
// prompt text. Only model, stream, and output token limits are omitted.
func BuildCachePrefix(body map[string]any) CachePrefix {
	if body == nil {
		return CachePrefix{}
	}
	stable := make(map[string]any, len(body))
	for key, value := range body {
		if key == "messages" || key == "model" || key == "stream" || isOutputLimitField(key) {
			continue
		}
		stable[key] = value
	}
	messages, ok := body["messages"].([]any)
	if !ok {
		// Some supported wire shapes use input instead of messages. Keep it in
		// stable data so we never claim a prefix without understood boundaries.
		stable["messages"] = body["messages"]
		return CachePrefix{Known: true, StableHash: hashValue(stable)}
	}
	if len(messages) > maxCachePrefixMessages {
		return CachePrefix{}
	}
	hashes := make([]string, len(messages))
	for i, message := range messages {
		hashes[i] = hashValue(message)
		if hashes[i] == "" {
			return CachePrefix{}
		}
	}
	return CachePrefix{Known: true, StableHash: hashValue(stable), MessageHash: hashes}
}

func isOutputLimitField(key string) bool {
	switch key {
	case "max_tokens", "max_completion_tokens", "max_output_tokens":
		return true
	default:
		return false
	}
}

func hashValue(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		// Unsupported values cannot safely establish evidence.
		return ""
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// CompareCachePrefix compares source-request evidence. It returns "extends"
// only when every old message hash is an unchanged prefix and stable fields
// match. Unknown or partial evidence never produces a token-level claim.
func CompareCachePrefix(old, current CachePrefix) string {
	if !old.Known || !current.Known || old.StableHash == "" || current.StableHash == "" || len(old.MessageHash) == 0 || len(current.MessageHash) == 0 {
		return "unknown"
	}
	if old.StableHash != current.StableHash {
		return "changed"
	}
	shared := len(old.MessageHash)
	if len(current.MessageHash) < shared {
		shared = len(current.MessageHash)
	}
	for i := 0; i < shared; i++ {
		if old.MessageHash[i] != current.MessageHash[i] {
			return "changed"
		}
	}
	if len(current.MessageHash) > len(old.MessageHash) {
		return "extends"
	}
	if len(current.MessageHash) < len(old.MessageHash) {
		return "changed"
	}
	return "same"
}

// CacheScope returns a non-reversible scope for provider configuration and
// client protocol. It cannot detect external OAuth account rotation.
func CacheScope(provider config.Provider, kind, resolvedCredential string) string {
	credential := sha256.Sum256([]byte(resolvedCredential))
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s", hashValue(provider), kind, hex.EncodeToString(credential[:]))))
	return hex.EncodeToString(sum[:])
}
