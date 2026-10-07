package config

// UpstreamWire is a client/provider protocol wire.
type UpstreamWire string

const (
	WireOpenAI    UpstreamWire = "openai"
	WireAnthropic UpstreamWire = "anthropic"
	WireResponses UpstreamWire = "responses"
)

// ProviderType is the provider host capability declared in config.
type ProviderType string

const (
	ProviderTypeOpenAI     ProviderType = "openai"
	ProviderTypeAnthropic  ProviderType = "anthropic"
	ProviderTypeResponses  ProviderType = "responses"
	ProviderTypeBoth       ProviderType = "both"
	ProviderTypeGemini     ProviderType = "gemini"
	ProviderTypeDevin      ProviderType = "devin"
	ProviderTypeCursor     ProviderType = "cursor"
	ProviderTypeChatGPTWeb ProviderType = "chatgpt-web"
)

// ProviderAuth is how a provider authenticates.
type ProviderAuth string

const (
	AuthAPIKey ProviderAuth = "api-key"
	AuthOAuth  ProviderAuth = "oauth"
)

// ProviderBilling is how a provider is billed.
type ProviderBilling string

const (
	BillingAPI          ProviderBilling = "api"
	BillingSubscription ProviderBilling = "subscription"
)

// OAuthSource names a local sign-in the router can read.
type OAuthSource string

const (
	OAuthClaudeCode  OAuthSource = "claude-code"
	OAuthCodex       OAuthSource = "codex"
	OAuthAntigravity OAuthSource = "antigravity"
	OAuthDevin       OAuthSource = "devin"
	OAuthCursor      OAuthSource = "cursor"
	OAuthWorkbuddyAI OAuthSource = "workbuddy-ai"
	OAuthFreebuff    OAuthSource = "freebuff"
	OAuthStatic      OAuthSource = "static"
)

// ModelEntry is a model id with an optional per-model wire pin.
// Bare strings in JSON become {ID}.
type ModelEntry struct {
	ID   string
	Wire []UpstreamWire // nil means infer; one or more pins when set
}

// ProviderQuotaSpec is a provider's dollar budget windows.
type ProviderQuotaSpec struct {
	FiveHourUSD *float64 `json:"fiveHourUsd,omitempty"`
	WeeklyUSD   *float64 `json:"weeklyUsd,omitempty"`
	MonthlyUSD  *float64 `json:"monthlyUsd,omitempty"`
}

// ProviderLogin is an alternate local sign-in for a provider.
type ProviderLogin struct {
	Label           string `json:"label,omitempty"`
	Home            string `json:"home,omitempty"`
	CredentialsPath string `json:"credentialsPath,omitempty"`
	KeychainService string `json:"keychainService,omitempty"`
	KeychainAccount string `json:"keychainAccount,omitempty"`
}

// Provider is one upstream endpoint in config.
type Provider struct {
	Name              string             `json:"name"`
	Type              ProviderType       `json:"type"`
	BaseURL           string             `json:"baseUrl"`
	APIKey            string             `json:"apiKey,omitempty"`
	APIKeyEnv         string             `json:"apiKeyEnv,omitempty"`
	Auth              ProviderAuth       `json:"auth"`
	OAuthSource       OAuthSource        `json:"oauthSource,omitempty"`
	Billing           ProviderBilling    `json:"billing"`
	Quota             *ProviderQuotaSpec `json:"quota,omitempty"`
	Login             *ProviderLogin     `json:"login,omitempty"`
	Models            []ModelEntry       `json:"models"`
	InjectStreamUsage bool               `json:"injectStreamUsage"`
	Headers           map[string]string  `json:"headers,omitempty"`
	SyncModels        *bool              `json:"syncModels,omitempty"`
	NoKey             bool               `json:"noKey,omitempty"`
	ExcludeModels     []string           `json:"excludeModels,omitempty"`
}

// RoutingEntry is one routing category the brain (or jevonian/<id>) can choose.
type RoutingEntry struct {
	ID          string              `json:"id"`
	Label       string              `json:"label"`
	Description string              `json:"description"`
	Models      []string            `json:"models"`
	Providers   map[string][]string `json:"providers,omitempty"`
	Effort      string              `json:"effort,omitempty"`
}

// RoutingTiers mirrors the four builtins for older configs/callers.
type RoutingTiers struct {
	Plan    []string `json:"plan"`
	Execute []string `json:"execute"`
	Utility []string `json:"utility"`
	Chat    []string `json:"chat"`
}

// BrainConfig is one brain channel used for auto routing.
type BrainConfig struct {
	Channel       string  `json:"channel"`
	BaseURL       string  `json:"baseUrl,omitempty"`
	AccountID     string  `json:"accountId,omitempty"`
	APIKeyEnv     string  `json:"apiKeyEnv,omitempty"`
	Model         string  `json:"model,omitempty"`
	TimeoutMs     int     `json:"timeoutMs"`
	MinConfidence float64 `json:"minConfidence"`
	FullPrompt    bool    `json:"fullPrompt,omitempty"`
}

// QuotaGuardConfig controls quota-aware candidate ordering.
type QuotaGuardConfig struct {
	Enabled    bool    `json:"enabled"`
	LowPercent float64 `json:"lowPercent"`
	ResetAware bool    `json:"resetAware"`
}

// ModelCapacityConfig is an optional per-model capacity override.
type ModelCapacityConfig struct {
	ContextWindow *int     `json:"contextWindow,omitempty"`
	MaxOutput     *int     `json:"maxOutput,omitempty"`
	Efforts       []string `json:"efforts,omitempty"`
}

// RoutingConfig is the routing block in config.
type RoutingConfig struct {
	Mode              string                         `json:"mode"`
	Routings          []RoutingEntry                 `json:"routings"`
	Tiers             RoutingTiers                   `json:"tiers"`
	SessionTTLMinutes int                            `json:"sessionTtlMinutes"`
	BaselineModel     string                         `json:"baselineModel,omitempty"`
	QuotaGuard        QuotaGuardConfig               `json:"quotaGuard"`
	Brains            []BrainConfig                  `json:"brains"`
	Capacities        map[string]ModelCapacityConfig `json:"capacities,omitempty"`
	DefaultEffort     string                         `json:"defaultEffort,omitempty"`
	BrainPicksEffort  bool                           `json:"brainPicksEffort"`
}

// TunnelConfig is optional public tunnel exposure.
type TunnelConfig struct {
	Enabled    bool   `json:"enabled"`
	Provider   string `json:"provider"`
	Command    string `json:"command,omitempty"`
	URL        string `json:"url,omitempty"`
	PublicPort *int   `json:"publicPort,omitempty"`
}

// LanConfig exposes authenticated /v1 on the LAN.
type LanConfig struct {
	Enabled bool   `json:"enabled"`
	Host    string `json:"host,omitempty"`
	Port    *int   `json:"port,omitempty"`
}

// ModelSyncConfig controls background model discovery.
type ModelSyncConfig struct {
	Enabled         bool `json:"enabled"`
	IntervalMinutes int  `json:"intervalMinutes"`
}

// PromptRewriteRule is an operator-supplied prompt rewrite.
type PromptRewriteRule struct {
	Match   string `json:"match"`
	Flags   string `json:"flags,omitempty"`
	Replace string `json:"replace"`
}

// PromptPolicyConfig is outgoing prompt hygiene (stub fields for now).
type PromptPolicyConfig struct {
	Builtins bool                `json:"builtins"`
	Rewrites []PromptRewriteRule `json:"rewrites"`
}

// TokenSaverConfig is RTK-based tool-result compression.
type TokenSaverConfig struct {
	Enabled   bool   `json:"enabled"`
	Command   string `json:"command"`
	TimeoutMs int    `json:"timeoutMs"`
}

// Config is the top-level Jevonian configuration.
type Config struct {
	Listen          ListenConfig        `json:"listen"`
	DefaultProvider string              `json:"defaultProvider,omitempty"`
	Providers       []Provider          `json:"providers"`
	ModelAliases    map[string][]string `json:"modelAliases,omitempty"`
	Tunnel          TunnelConfig        `json:"tunnel"`
	Lan             LanConfig           `json:"lan"`
	Routing         RoutingConfig       `json:"routing"`
	ModelSync       ModelSyncConfig     `json:"modelSync"`
	PromptPolicy    PromptPolicyConfig  `json:"promptPolicy"`
	TokenSaver      TokenSaverConfig    `json:"tokenSaver"`
}

// ListenConfig is the loopback bind address.
type ListenConfig struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}
