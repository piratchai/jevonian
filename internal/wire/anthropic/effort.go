package anthropic

import (
	"github.com/xinyao27/jevonian/internal/wire"
)

// ReasoningEffort levels, lowest to highest — mirrors src/capabilities.ts.
var effortDepth = map[string]int{
	"none": 0, "minimal": 1, "low": 2, "medium": 3,
	"high": 4, "xhigh": 5, "max": 6, "ultra": 8,
}

// IsReasoningEffort reports whether value names a known level.
func IsReasoningEffort(value string) bool {
	_, ok := effortDepth[value]
	return ok
}

// EffortBudgets are the thinking budgets in tokens for the Anthropic wire, by
// depth. Ordered slice: `effortInBody` finds the first (shallowest) level
// whose budget covers a request, so order is semantic.
var effortBudgets = []struct {
	Level  string
	Tokens int
}{
	{"minimal", 1024},
	{"low", 2048},
	{"medium", 8192},
	{"high", 16384},
	{"xhigh", 24576},
	{"max", 32768},
	{"ultra", 32768},
}

func effortBudget(effort string) int {
	for _, entry := range effortBudgets {
		if entry.Level == effort {
			return entry.Tokens
		}
	}
	return 32768
}

// WireKind is a client/upstream wire identifier.
type WireKind string

const (
	WireOpenAI    WireKind = "openai"
	WireAnthropic WireKind = "anthropic"
	WireResponses WireKind = "responses"
)

// effortField is the thinking-level field each wire uses.
func effortField(w WireKind) string {
	switch w {
	case WireAnthropic:
		return "thinking"
	case WireResponses:
		return "reasoning"
	default:
		return "reasoning_effort"
	}
}

// StripForeignEffort drops thinking-level fields that do not belong to `w`.
// A body can arrive spelled for a different wire than the endpoint it
// targets — Codex sends `reasoning_effort` on native /v1/responses calls —
// and forwarding that spelling to a Responses upstream is rejected outright
// with "Unsupported parameter: reasoning_effort".
func StripForeignEffort(body wire.Body, w WireKind) wire.Body {
	keep := effortField(w)
	var foreign []string
	for _, field := range []string{"thinking", "reasoning_effort", "reasoning"} {
		if field != keep {
			if _, present := body[field]; present {
				foreign = append(foreign, field)
			}
		}
	}
	if len(foreign) == 0 {
		return body
	}
	next := cloneMap(body)
	for _, field := range foreign {
		delete(next, field)
	}
	return next
}

// withOutputEffort writes `output_config.effort`, keeping any other
// `output_config` keys the body carries.
func withOutputEffort(body wire.Body, effort string) wire.Body {
	next := cloneMap(body)
	outputConfig := cloneMap(wire.AsRecord(body["output_config"]))
	outputConfig["effort"] = effort
	next["output_config"] = outputConfig
	return next
}

// anthropicWithEffort writes the router's level in the shape the target
// Claude model accepts: legacy models take a token budget, adaptive models
// (Claude 4.6+) take `thinking: {type: "adaptive"}` plus
// `output_config.effort`. "Off" stays an explicit disabled where the model
// allows it; always-on models get the lowest effort instead.
func anthropicWithEffort(body wire.Body, effort string) wire.Body {
	support := ThinkingSupportFor(body["model"])
	if !support.Adaptive {
		if effort == "none" {
			next := cloneMap(body)
			next["thinking"] = wire.Body{"type": "disabled"}
			return next
		}
		next := cloneMap(body)
		next["thinking"] = wire.Body{"type": "enabled", "budget_tokens": float64(effortBudget(effort))}
		return next
	}
	if effort == "none" && !support.RejectsDisabled {
		next := cloneMap(body)
		next["thinking"] = wire.Body{"type": "disabled"}
		return next
	}
	// Keep a client's `display` choice; everything else in `thinking` is the
	// router's to set.
	thinking := wire.Body{"type": "adaptive"}
	if display, present := wire.AsRecord(body["thinking"])["display"]; present {
		thinking["display"] = display
	}
	next := cloneMap(body)
	next["thinking"] = thinking
	return withOutputEffort(next, AdaptiveEffort(effort, support))
}

// NormalizeThinking translates thinking shapes the target model rejects —
// typically sent by a client targeting an older model — into the adaptive
// equivalent: `disabled` on always-on models becomes the lowest effort, and a
// `budget_tokens` request on models without extended thinking becomes the
// nearest effort level. An `output_config.effort` the client already set is
// kept.
func NormalizeThinking(body wire.Body) wire.Body {
	thinking := wire.AsRecord(body["thinking"])
	support := ThinkingSupportFor(body["model"])
	disabled := thinking["type"] == "disabled" && support.RejectsDisabled
	enabled := thinking["type"] == "enabled" && support.RejectsEnabled
	if !disabled && !enabled {
		return body
	}
	// `display` is invalid alongside `disabled` but valid with `adaptive`,
	// so keep what remains after dropping the legacy keys.
	nextThinking := cloneMap(thinking)
	delete(nextThinking, "budget_tokens")
	delete(nextThinking, "type")
	nextThinking["type"] = "adaptive"
	next := cloneMap(body)
	next["thinking"] = nextThinking
	if _, ok := wire.AsRecord(body["output_config"])["effort"].(string); ok {
		return next
	}
	level := "low"
	if enabled {
		if budget, ok := thinking["budget_tokens"]; ok && wire.IsNumber(budget) {
			// The shallowest level whose budget covers the request, so thinking
			// is never cut short.
			tokens := int(wire.Number(budget))
			level = "max"
			for _, entry := range effortBudgets {
				if entry.Tokens >= tokens {
					level = entry.Level
					break
				}
			}
		}
	}
	return withOutputEffort(next, AdaptiveEffort(level, support))
}

// WithEffort writes the router's chosen thinking level into an outgoing body,
// in the field the target wire expects. A level the client set itself wins:
// the caller was explicit, and overriding an instruction with a guess is
// worse than ignoring the router's choice. `none` is written explicitly —
// a model that defaults to thinking would otherwise keep thinking.
func WithEffort(body wire.Body, effort string, w WireKind, clientEffort string) wire.Body {
	target := StripForeignEffort(body, w)
	if w == WireAnthropic {
		// Normalized on every Anthropic body, client-set levels included:
		// newer models answer the legacy shapes with a 400.
		if effort == "" || clientEffort != "" {
			return NormalizeThinking(target)
		}
		return anthropicWithEffort(target, effort)
	}
	if effort == "" || clientEffort != "" {
		return target
	}
	if w == WireResponses {
		// The Responses wire spells "off" as a null reasoning object.
		if effort == "none" {
			next := cloneMap(target)
			next["reasoning"] = nil
			return next
		}
		next := cloneMap(target)
		next["reasoning"] = wire.Body{"effort": effort}
		return next
	}
	next := cloneMap(target)
	next["reasoning_effort"] = effort
	return next
}

// adaptiveEffortInBody reads an adaptive `output_config.effort` back as a
// router level.
func adaptiveEffortInBody(body wire.Body, hint string) string {
	effort, ok := wire.AsRecord(body["output_config"])["effort"].(string)
	if !ok || !IsReasoningEffort(effort) {
		return ""
	}
	// The router's own level wins when it is what was written (e.g. `minimal`
	// sent as `low`); `none` is excluded because an always-on model sent `low`
	// really does think.
	if hint != "" && hint != "none" {
		support := ThinkingSupportFor(body["model"])
		if AdaptiveEffort(hint, support) == effort {
			return hint
		}
	}
	return effort
}

// EffortInBody reads the thinking level an outgoing body actually carries,
// from whichever field the wire uses — what the log reports. `hint`
// disambiguates budgets that collide (`max` and `ultra` share an Anthropic
// budget) so a level is never reported as a shallower one by accident.
func EffortInBody(body wire.Body, w WireKind, hint string) string {
	if w == WireAnthropic {
		thinking := wire.AsRecord(body["thinking"])
		if thinking["type"] == "disabled" {
			return "none"
		}
		if adaptive := adaptiveEffortInBody(body, hint); adaptive != "" {
			return adaptive
		}
		budget := thinking["budget_tokens"]
		if !wire.IsNumber(budget) {
			return ""
		}
		tokens := int(wire.Number(budget))
		if hint != "" && effortBudget(hint) == tokens {
			return hint
		}
		for _, entry := range effortBudgets {
			if entry.Tokens == tokens {
				return entry.Level
			}
		}
		return ""
	}
	if w == WireResponses {
		if reasoning, present := body["reasoning"]; present && reasoning == nil {
			return "none"
		}
		if effort, ok := wire.AsRecord(body["reasoning"])["effort"].(string); ok && IsReasoningEffort(effort) {
			return effort
		}
		return ""
	}
	if effort, ok := body["reasoning_effort"].(string); ok && IsReasoningEffort(effort) {
		return effort
	}
	return ""
}

// ClientEffortOf reads the thinking level the client asked for itself, in
// whatever field its wire uses. Checked before the router's own choice so an
// explicit instruction is never overridden.
func ClientEffortOf(body wire.Body, w WireKind) string {
	if w == WireAnthropic {
		thinking := wire.AsRecord(body["thinking"])
		if thinking["type"] == "disabled" {
			return "none"
		}
		// A client on adaptive thinking states its level in output_config.effort.
		if adaptive := adaptiveEffortInBody(body, ""); adaptive != "" {
			return adaptive
		}
		budget := thinking["budget_tokens"]
		if !wire.IsNumber(budget) {
			return ""
		}
		tokens := int(wire.Number(budget))
		for _, entry := range effortBudgets {
			if entry.Tokens == tokens {
				return entry.Level
			}
		}
		return ""
	}
	if w == WireResponses {
		return EffortInBody(body, WireResponses, "")
	}
	return EffortInBody(body, WireOpenAI, "")
}

// BridgedBodyOptions are the knobs BridgedAnthropicBody needs.
type BridgedBodyOptions struct {
	Model        string
	Stream       bool
	Effort       string
	ClientEffort string
	MaxOutput    int
}

// BridgedAnthropicBody builds an Anthropic Messages body from a Chat
// Completions body (native, or folded from Responses). The client's own
// reasoning level is carried into the wire shape the model takes, and
// `max_tokens` is made consistent with the resulting thinking configuration.
// Claude 4.6+ rejects a conversation ending on an assistant turn ("prefill"),
// so the trailing prefill is dropped last.
func BridgedAnthropicBody(chatBody wire.Body, options BridgedBodyOptions) wire.Body {
	base := ChatToAnthropic(chatBody)
	base["model"] = options.Model
	base["stream"] = options.Stream
	effort := options.ClientEffort
	if effort == "" {
		effort = options.Effort
	}
	max := chatBody["max_completion_tokens"]
	if max == nil {
		max = chatBody["max_tokens"]
	}
	fitted := FitThinkingMaxTokens(WithEffort(base, effort, WireAnthropic, ""), MaxTokensOptions{
		ClientSetMax: wire.IsNumber(max) && wire.Number(max) > 0,
		MaxOutput:    options.MaxOutput,
	})
	return NormalizePrefill(fitted)
}
