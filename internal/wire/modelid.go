package wire

import "strings"

// BareModelID is the model segment after the last `/`; the id unchanged when it
// has no prefix. Port of src/model-id.ts — catalogs and resellers prefix a
// model with its vendor (`openai/gpt-6`, `accounts/fireworks/models/kimi-k3`).
func BareModelID(id string) string {
	slash := strings.LastIndex(id, "/")
	if slash < 0 {
		return id
	}
	if tail := id[slash+1:]; tail != "" {
		return tail
	}
	return id
}

// ModelVendor is the vendor segment before the first `/`, or "" when the id
// has no prefix.
func ModelVendor(id string) string {
	slash := strings.Index(id, "/")
	if slash > 0 {
		return id[:slash]
	}
	return ""
}
