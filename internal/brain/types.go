// Package brain is a hand-rolled HTTP client for the SystemOne decision
// service ("the Jev brain") and every compatible channel.
//
// Port of src/brain.ts, minus the Vercel @ai-sdk/gateway channel which the Go
// rewrite drops (docs/go-feature-parity.md, section 11). Plain net/http via an
// injected *http.Client; no AI SDK anywhere.
package brain

import (
	"github.com/xinyao27/jevonian/internal/config"
)

// BrainModelOption is a decision model a channel can run, for pickers.
// src/brain.ts BrainModelOption.
type BrainModelOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Hint  string `json:"hint,omitempty"`
}

// Channel describes one SystemOne-compatible endpoint family.
// src/brain.ts JevChannel.
type Channel struct {
	ID              string
	Label           string
	BaseURL         string
	Model           string
	APIKeyEnv       string
	RequiresBaseURL bool
	// Preset models this channel can serve, the first being the default.
	Models []BrainModelOption
	// The UI asks for a Cloudflare account id instead of a free-form endpoint.
	RequiresAccountID bool
	Hint              string
	KeysURL           string
	// Endpoint accepts requests without an API key (local Kev); sends
	// PlaceholderKey when none is configured. src/brain.ts keyOptional.
	KeyOptional bool
	// Channel-specific default minConfidence when it differs from the global 0.6.
	DefaultMinConfidence float64
	// Read confidence from the winning option's probability (Kev).
	ConfidenceFromDistribution bool
	// Trim soft evidence (benchmarks, flat candidates, tool blobs) for
	// decision models trained on short states (Kev 4B/9B).
	CompactState bool
}

// PlaceholderKey is the bearer sent to a KeyOptional channel with no key.
// src/brain.ts PLACEHOLDER_BRAIN_KEY.
const PlaceholderKey = "local"

// Channels lists the supported brain routes. src/brain.ts JEV_CHANNELS.
// The "vercel" entry is kept as a marker so stored configs surface a clear
// error; it is never callable (channel "vercel" fails with ErrVercelDropped).
var Channels = []Channel{
	{
		ID:        "typesafe",
		Label:     "TypeSafe (direct)",
		BaseURL:   "https://api.typesafe.ai/v1/systemone",
		Model:     "jev-latest",
		APIKeyEnv: "TYPESAFE_API_KEY",
		KeysURL:   "https://console.typesafe.ai",
		Hint:      "Create an API key at console.typesafe.ai.",
	},
	{
		ID:        "openrouter",
		Label:     "OpenRouter",
		BaseURL:   "https://openrouter.ai/api/alpha/decisions",
		Model:     "typesafe/jev-1.13",
		APIKeyEnv: "OPENROUTER_API_KEY",
		KeysURL:   "https://openrouter.ai/settings/keys",
		Hint:      "Create an API key under OpenRouter → Settings → Keys.",
	},
	{
		ID:        "opencode-zen",
		Label:     "OpenCode Zen",
		BaseURL:   "https://opencode.ai/zen/v1/systemone",
		Model:     "jev-1.13",
		APIKeyEnv: "OPENCODE_API_KEY",
		KeysURL:   "https://opencode.ai/auth",
		Hint:      "Sign in at opencode.ai/auth and copy a Zen API key.",
	},
	{
		ID:        "vercel",
		Label:     "Vercel AI Gateway",
		BaseURL:   "",
		Model:     "typesafe-ai/jev",
		APIKeyEnv: "AI_GATEWAY_API_KEY",
		KeysURL:   "https://vercel.com/docs/ai-gateway",
		Hint:      "Create an AI Gateway API key in the Vercel dashboard.",
	},
	{
		ID:                "cloudflare",
		Label:             "Cloudflare Workers AI",
		BaseURL:           "",
		Model:             "typesafe/jev",
		APIKeyEnv:         "CLOUDFLARE_API_TOKEN",
		RequiresAccountID: true,
		Models: []BrainModelOption{
			{
				ID:    "typesafe/jev",
				Label: "Jev · TypeSafe System One",
				Hint:  "TypeSafe's decision model: 32k context, text only. The long-standing default on this channel.",
			},
			{
				ID:    "@cf/cloudflare/clef",
				Label: "Clef · Cloudflare",
				Hint:  "Cloudflare's decision model: 64k context, vision input, Jev-API compatible.",
			},
			{
				ID:    "@cf/cloudflare/clef-flash",
				Label: "Clef-flash · Cloudflare",
				Hint:  "The smaller Clef, tuned for latency (~40ms median vs Clef's ~210ms).",
			},
		},
		KeysURL: "https://developers.cloudflare.com/workers-ai/",
		Hint:    "Account ID from the Cloudflare dashboard overview; API token needs Workers AI permission.",
	},
	{
		ID:                         "kev",
		Label:                      "Kev (local)",
		BaseURL:                    "http://127.0.0.1:8009/v1/systemone",
		Model:                      "kev-latest",
		APIKeyEnv:                  "KEV_API_KEY",
		KeyOptional:                true,
		DefaultMinConfidence:       0.4,
		ConfidenceFromDistribution: true,
		CompactState:               true,
		KeysURL:                    "https://github.com/jaredpalmer/kev",
		Hint:                       "`jevonian kev --start` deploys it; no key needed unless KEV_API_KEY is set on the server.",
	},
	{
		ID:              "custom",
		Label:           "Custom endpoint",
		BaseURL:         "",
		Model:           "jev-latest",
		APIKeyEnv:       "",
		RequiresBaseURL: true,
		Hint:            "Use any SystemOne-compatible endpoint and its API key.",
	},
}

// FindChannel returns the channel with id, or nil. src/brain.ts findJevChannel.
func FindChannel(id string) *Channel {
	for i := range Channels {
		if Channels[i].ID == id {
			return &Channels[i]
		}
	}
	return nil
}

// CredentialName is the credentials-file key for a brain channel.
// src/brain.ts brainCredentialName.
func CredentialName(channel string) string {
	return "brain:" + channel
}

// Usage mirrors the token accounting SystemOne reports.
// src/pricing.ts Usage (subset the brain reads).
type Usage struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheRead  int `json:"cacheRead"`
	CacheWrite int `json:"cacheWrite"`
}

// Verdict is the brain's routing answer. src/brain.ts BrainVerdict.
type Verdict struct {
	// Model is the winning option: a routing id when routings were offered.
	Model string `json:"model"`
	// Confidence is Jev's calibrated score, or the distribution top for
	// channels flagged ConfidenceFromDistribution.
	Confidence float64 `json:"confidence"`
	// Probabilities is the full distribution over options, when returned.
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	// Effort is the thinking level the brain wants, when asked.
	Effort string `json:"effort,omitempty"`
	// EffortProbabilities is the effort distribution, when returned.
	EffortProbabilities map[string]float64 `json:"effortProbabilities,omitempty"`
	// ModelName is the decision model's own id from the response.
	ModelName string `json:"modelName,omitempty"`
	Usage     *Usage `json:"usage,omitempty"`
}

// Failure explains why an ask produced no verdict. src/brain.ts AskJevFailure.
type Failure struct {
	Status int
	Error  string
}

// FreeformQuestion is a one-off question shape, used when something other than
// model choice is being asked (compaction's noul questions).
// src/brain.ts FreeformQuestion.
type FreeformQuestion struct {
	Name         string
	Instructions string
	// Criteria maps option name to its description; a null value is kept as
	// the JSON null TS emits.
	Criteria map[string]*string
}

// Input is one brain request. src/brain.ts BrainInput.
type Input struct {
	Brain config.BrainConfig
	// State is the routing state the brain judges; marshalled verbatim.
	State map[string]any
	// APIKey overrides credential resolution (tests / one-off calls).
	APIKey string
	// ModelOnly asks only for the model, not the thinking level. The router
	// sets this when routing.brainPicksEffort is off.
	ModelOnly bool
	// Freeform asks something other than "which model".
	Freeform *FreeformQuestion
}

// Outcome is either a verdict or why there is none — on the call's own result,
// never through shared state. src/brain.ts askJevOutcome.
type Outcome struct {
	Verdict *Verdict
	Failure *Failure
}

// ---- request shapes (src/brain.ts httpQuestions) ----

// Question is a SystemOne "choice" question.
type Question struct {
	Type         string             `json:"type"`
	Instructions string             `json:"instructions"`
	Criteria     map[string]*string `json:"criteria"`
}

// systemOneRequest is the POST body for SystemOne-shaped channels.
type systemOneRequest struct {
	Model     string              `json:"model"`
	State     map[string]any      `json:"state"`
	Questions map[string]Question `json:"questions"`
}

// cloudflareRunRequest is the Jev-envelope body for the bare /ai/run endpoint.
type cloudflareRunRequest struct {
	Model string `json:"model"`
	Input struct {
		State     map[string]any      `json:"state"`
		Questions map[string]Question `json:"questions"`
	} `json:"input"`
}

// cloudflareCatalogRequest is the body for @cf/... catalog models (Clef).
type cloudflareCatalogRequest struct {
	Model     string              `json:"model"`
	State     map[string]any      `json:"state"`
	Questions map[string]Question `json:"questions"`
}

// parseOutcome is the parsed half of a SystemOne response, pre-verdict.
type parseOutcome struct {
	Model               string
	Confidence          float64
	Probabilities       map[string]float64
	Effort              string
	EffortProbabilities map[string]float64
	ModelName           string
	Usage               *Usage
}

func jsonNumber(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}

func jsonString(v any) string {
	s, _ := v.(string)
	return s
}
