package anthropic

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/xinyao27/jevonian/internal/wire"
)

// ThinkingSupport is what a Claude model accepts for `thinking`, derived from
// its id. Port of src/anthropic-thinking.ts: Anthropic moved from extended
// thinking (`thinking: {type: "enabled", budget_tokens}`) to adaptive thinking
// (`thinking: {type: "adaptive"}` steered by `output_config.effort`), and newer
// models reject the legacy shapes with a 400.
type ThinkingSupport struct {
	// Adaptive accepts `thinking: {type: "adaptive"}` and `output_config.effort`.
	Adaptive bool
	// RejectsEnabled rejects `thinking: {type: "enabled", budget_tokens}`.
	RejectsEnabled bool
	// RejectsDisabled: thinking is always on; `type: "disabled"` is rejected.
	RejectsDisabled bool
	// Xhigh accepts `output_config.effort: "xhigh"`.
	Xhigh bool
}

var legacyThinkingSupport = ThinkingSupport{}

var claudeModelID = regexp.MustCompile(`claude-(opus|sonnet|haiku|fable|mythos)-(\d+)(?:[-.](\d{1,2}))?(?:$|[^0-9])`)

var fableMythosNoVersion = regexp.MustCompile(`claude-(fable|mythos)(?:$|[^a-z0-9])`)

// modelIDParts extracts family + version from a Claude model id.
// Returns ok=false for non-matching ids.
func modelIDParts(model string) (family string, version float64, ok bool) {
	match := claudeModelID.FindStringSubmatch(strings.ToLower(model))
	if match == nil {
		return "", 0, false
	}
	major, _ := strconv.Atoi(match[2])
	version = float64(major)
	if match[3] != "" {
		minor, _ := strconv.Atoi(match[3])
		version += float64(minor) / 10
	}
	return match[1], version, true
}

// ThinkingSupportFor reports what a Claude model accepts for `thinking`.
// Unknown or pre-4.6 ids keep the legacy extended-thinking behaviour so
// nothing changes for models we cannot place.
func ThinkingSupportFor(model any) ThinkingSupport {
	s, ok := model.(string)
	if !ok {
		return legacyThinkingSupport
	}
	id := strings.ToLower(s)
	family, version, ok := modelIDParts(id)
	if !ok {
		return legacyThinkingSupport
	}
	if family == "fable" || family == "mythos" {
		// Mythos Preview still accepts extended thinking; every Fable/Mythos
		// rejects "disabled".
		preview := strings.Contains(id, "preview")
		return ThinkingSupport{
			Adaptive:        true,
			RejectsEnabled:  !preview,
			RejectsDisabled: true,
			Xhigh:           !preview,
		}
	}
	if version < 4.6 {
		return legacyThinkingSupport
	}
	return ThinkingSupport{
		Adaptive:       true,
		RejectsEnabled: version >= 4.7,
		// Opus 5.5 is always on; later generations are assumed to follow it
		// rather than risk a 400.
		RejectsDisabled: ((family == "opus" || family == "sonnet") && version >= 5.5) || version >= 6,
		Xhigh:           version >= 4.7,
	}
}

// RejectsAssistantPrefill reports whether the model rejects a trailing
// `assistant` turn ("prefill") with 400. Claude 4.6 and later, plus the
// Fable/Mythos family, answer such a request with "This model does not
// support assistant message prefill". Claude 4.5 / Haiku 4.5 and older still
// accept one prefilled assistant turn.
func RejectsAssistantPrefill(model any) bool {
	s, ok := model.(string)
	if !ok {
		return false
	}
	id := strings.ToLower(s)
	family, version, ok := modelIDParts(id)
	if !ok {
		// The Fable/Mythos previews carry no version digit, so the strict id
		// pattern does not match them, but they still reject prefill.
		return fableMythosNoVersion.MatchString(id)
	}
	if family == "fable" || family == "mythos" {
		return true
	}
	return version >= 4.6
}

// ThinkingHeadroom is the room left for the visible answer on top of a legacy
// thinking budget.
const ThinkingHeadroom = 4096

// BridgedThinkingMaxTokens is the default `max_tokens` for a bridged request
// with thinking on when the client stated none. The bridge's plain default
// (4096) is sized for a bare answer; adaptive thinking spends from the same
// pool, so at that size a thinking turn routinely stops at max_tokens with a
// truncated or missing answer.
const BridgedThinkingMaxTokens = 16384

// MaxTokensOptions tweaks FitThinkingMaxTokens.
type MaxTokensOptions struct {
	// ClientSetMax: the client set `max_tokens` itself; never re-default it.
	ClientSetMax bool
	// MaxOutput is the model's stated output cap, when known.
	MaxOutput int
}

// FitThinkingMaxTokens makes `max_tokens` consistent with the body's thinking
// configuration: extended thinking requires max_tokens > budget_tokens, else
// Anthropic answers 400, so max_tokens is raised to budget + headroom (or the
// budget shrinks when that exceeds the model's cap). Adaptive thinking gets
// the roomier BridgedThinkingMaxTokens default when the client set none.
func FitThinkingMaxTokens(body wire.Body, options MaxTokensOptions) wire.Body {
	thinking := wire.AsRecord(body["thinking"])
	current := int(wire.Number(body["max_tokens"]))
	limit := options.MaxOutput
	capped := func(value int) int {
		if limit > 0 && value > limit {
			return limit
		}
		return value
	}

	if thinking["type"] == "enabled" && wire.IsNumber(thinking["budget_tokens"]) {
		budget := int(wire.Number(thinking["budget_tokens"]))
		if current > budget {
			return body
		}
		max := budget + ThinkingHeadroom
		if limit > 0 && max > limit {
			// Anthropic's floor for a thinking budget is 1024 tokens.
			max = limit
			if shrunk := limit - ThinkingHeadroom; shrunk < budget {
				budget = shrunk
			}
			if budget < 1024 {
				budget = 1024
			}
		}
		next := cloneBody(body)
		next["max_tokens"] = float64(max)
		nextThinking := cloneBody(thinking)
		nextThinking["budget_tokens"] = float64(budget)
		next["thinking"] = nextThinking
		return next
	}
	if thinking["type"] == "adaptive" && !options.ClientSetMax {
		max := capped(BridgedThinkingMaxTokens)
		if current >= max {
			return body
		}
		next := cloneBody(body)
		next["max_tokens"] = float64(max)
		return next
	}
	return body
}

// AdaptiveEffort maps a router effort level to the `output_config.effort`
// value the model takes: low, medium, high, xhigh or max.
func AdaptiveEffort(effort string, support ThinkingSupport) string {
	switch effort {
	case "none", "minimal", "low":
		return "low"
	case "medium":
		return "medium"
	case "high":
		return "high"
	case "xhigh":
		if support.Xhigh {
			return "xhigh"
		}
		return "high"
	default:
		return "max"
	}
}

func cloneBody(body wire.Body) wire.Body {
	next := make(wire.Body, len(body))
	for k, v := range body {
		next[k] = v
	}
	return next
}
