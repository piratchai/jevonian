package server

import (
	"net/http"
	"strings"

	"github.com/xinyao27/jevonian/internal/routing"
)

// Port of src/claude-gateway.ts. Claude Desktop's third-party ("gateway") mode
// ships a registry of Claude ids it knows how to render, so Jevonian stands in
// for one of those per injected model and translates the id back on requests.

type gatewaySlot struct {
	id        string
	family    string
	createdAt string
}

var slotTemplates = []gatewaySlot{
	{id: "claude-sonnet-5", family: "sonnet", createdAt: "2026-06-30T00:00:00Z"},
	{id: "claude-opus-5", family: "opus", createdAt: "2026-07-24T00:00:00Z"},
	{id: "claude-sonnet-4-6", family: "sonnet", createdAt: "2025-11-18T00:00:00Z"},
	{id: "claude-haiku-4-5-20251001", family: "haiku", createdAt: "2025-10-01T00:00:00Z"},
}

// labelFor renders `jevonian/auto` as "Jevonian Auto"; other ids keep their
// own words.
func labelFor(model string) string {
	tail := routing.BareModelID(model)
	words := strings.FieldsFunc(tail, func(r rune) bool {
		return r == '-' || r == '_' || r == '.' || r == ' '
	})
	out := []string{"Jevonian"}
	for _, w := range words {
		if w == "" {
			continue
		}
		out = append(out, strings.ToUpper(w[:1])+w[1:])
	}
	return strings.Join(out, " ")
}

// isClaudeGatewayRequest is true when the client stamps anthropic-version —
// the only surface that speaks the Anthropic model list.
func isClaudeGatewayRequest(h http.Header) bool {
	return strings.TrimSpace(h.Get("anthropic-version")) != ""
}

// claudeGatewayModels is the Anthropic model-list page. `anthropic_family_tier`
// is what makes an entry visible to the app at all.
func claudeGatewayModels(models []string) map[string]any {
	n := len(models)
	if n > len(slotTemplates) {
		n = len(slotTemplates)
	}
	data := make([]map[string]any, 0, n)
	for i := 0; i < n; i++ {
		t := slotTemplates[i]
		data = append(data, map[string]any{
			"id":                    t.id,
			"type":                  "model",
			"display_name":          labelFor(models[i]),
			"created_at":            t.createdAt,
			"anthropic_family_tier": t.family,
		})
	}
	var first, last any
	if len(data) > 0 {
		first = data[0]["id"]
		last = data[len(data)-1]["id"]
	}
	return map[string]any{
		"data":     data,
		"first_id": first,
		"last_id":  last,
		"has_more": false,
	}
}

// resolveClaudeGatewayModel turns a Claude stand-in id back into the Jevonian
// model it stands for, or "" when the id is not one of ours.
func resolveClaudeGatewayModel(requested string, desktop []string, autoMode bool, pinned func(string) bool, fromGateway bool) string {
	for i, model := range desktop {
		if i >= len(slotTemplates) {
			break
		}
		if slotTemplates[i].id != requested {
			continue
		}
		if autoMode {
			return model
		}
		if !fromGateway {
			return ""
		}
		if pinned != nil && pinned(requested) {
			return ""
		}
		return model
	}
	return ""
}
