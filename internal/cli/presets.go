package cli

import "github.com/xinyao27/jevonian/internal/config"

// preset mirrors ProviderPreset in src/providers.ts (same ids, order, URLs).
type preset struct {
	id, name, typ, url, env, source, billing string
	keysURL, hint                            string
	noKey, sync                              bool
	// explicitAPIKey marks presets that declare `auth: "api-key"` in the TS table.
	explicitAPIKey bool
}

var presets = []preset{
	{id: "deepseek", name: "DeepSeek", typ: "both", url: "https://api.deepseek.com/v1", env: "DEEPSEEK_API_KEY", keysURL: "https://platform.deepseek.com/api_keys", hint: "Create an API key on the DeepSeek platform."},
	{id: "anthropic", name: "Anthropic (Claude)", typ: "anthropic", url: "https://api.anthropic.com/v1", env: "ANTHROPIC_API_KEY", keysURL: "https://console.anthropic.com/settings/keys", hint: "Create an API key in the Anthropic Console."},
	{id: "openai", name: "OpenAI", typ: "openai", url: "https://api.openai.com/v1", env: "OPENAI_API_KEY", keysURL: "https://platform.openai.com/api-keys", hint: "Create an API key on the OpenAI platform."},
	{id: "moonshotai", name: "Moonshot (Kimi)", typ: "openai", url: "https://api.moonshot.ai/v1", env: "MOONSHOT_API_KEY", keysURL: "https://platform.moonshot.ai/console/api-keys", hint: "Create an API key in the Moonshot console."},
	{id: "zai", name: "Z.ai (GLM)", typ: "openai", url: "https://api.z.ai/api/paas/v4", env: "ZAI_API_KEY", keysURL: "https://z.ai/manage-apikey/apikey-list", hint: "Create an API key in the Z.ai console."},
	{id: "minimax", name: "MiniMax", typ: "openai", url: "https://api.minimax.io/v1", env: "MINIMAX_API_KEY", keysURL: "https://platform.minimax.io/user-center/basic-information/interface-key", hint: "Create an interface key in the MiniMax user center."},
	{id: "qwen", name: "Alibaba Qwen", typ: "openai", url: "https://dashscope-intl.aliyuncs.com/compatible-mode/v1", env: "DASHSCOPE_API_KEY", keysURL: "https://bailian.console.alibabacloud.com/#/api-key", hint: "Create a DashScope API key in the Alibaba Cloud Model Studio console."},
	{id: "xai", name: "xAI (Grok)", typ: "openai", url: "https://api.x.ai/v1", env: "XAI_API_KEY", keysURL: "https://console.x.ai/team/default/api-keys", hint: "Create an API key in the xAI console."},
	{id: "google", name: "Google Gemini", typ: "openai", url: "https://generativelanguage.googleapis.com/v1beta/openai", env: "GEMINI_API_KEY", keysURL: "https://aistudio.google.com/apikey", hint: "Create an API key in Google AI Studio."},
	{id: "openrouter", name: "OpenRouter", typ: "both", url: "https://openrouter.ai/api/v1", env: "OPENROUTER_API_KEY", keysURL: "https://openrouter.ai/settings/keys", hint: "Create an API key under OpenRouter → Settings → Keys."},
	{id: "orcarouter", name: "OrcaRouter", typ: "openai", url: "https://api.orcarouter.ai/v1", env: "ORCAROUTER_API_KEY", keysURL: "https://www.orcarouter.ai/console", hint: "Create an API key in the OrcaRouter console."},
	{id: "opencode-go", name: "OpenCode Go", typ: "both", url: "https://opencode.ai/zen/go/v1", env: "OPENCODE_GO_API_KEY", billing: "subscription", sync: true, keysURL: "https://opencode.ai/auth", hint: "Sign in at opencode.ai/auth and copy a Go / Zen API key."},
	{id: "opencode-zen", name: "OpenCode Zen", typ: "both", url: "https://opencode.ai/zen/v1", env: "OPENCODE_API_KEY", billing: "subscription", sync: true, keysURL: "https://opencode.ai/auth", hint: "Sign in at opencode.ai/auth and copy a Zen API key."},
	{id: "commandcode", name: "Command Code", typ: "both", url: "https://api.commandcode.ai/provider/v1", env: "COMMAND_CODE_API_KEY", billing: "subscription", sync: true, keysURL: "https://commandcode.ai", hint: "Sign in on Command Code and create a provider API key."},
	{id: "mistral", name: "Mistral", typ: "openai", url: "https://api.mistral.ai/v1", env: "MISTRAL_API_KEY", sync: true, keysURL: "https://console.mistral.ai/api-keys", hint: "Create an API key in the Mistral console."},
	{id: "groq", name: "Groq", typ: "openai", url: "https://api.groq.com/openai/v1", env: "GROQ_API_KEY", sync: true, keysURL: "https://console.groq.com/keys", hint: "Create an API key in the Groq console."},
	{id: "ollama", name: "Ollama", typ: "openai", url: "http://127.0.0.1:11434/v1", noKey: true, sync: true, keysURL: "https://ollama.com", hint: "Run Ollama locally; no API key required."},
	{id: "lmstudio", name: "LM Studio", typ: "openai", url: "http://127.0.0.1:1234/v1", noKey: true, sync: true, keysURL: "https://lmstudio.ai", hint: "Start LM Studio's local server; no API key required."},
	{id: "claude-subscription", name: "Claude (Pro/Max)", typ: "anthropic", url: "https://api.anthropic.com/v1", source: "claude-code", billing: "subscription", keysURL: "https://code.claude.com/docs/en/oauth", hint: "Sign in with `claude`; Jevonian reads ~/.claude/.credentials.json."},
	{id: "chatgpt-subscription", name: "ChatGPT (Codex)", typ: "responses", url: "https://chatgpt.com/backend-api/codex", source: "codex", billing: "subscription", keysURL: "https://github.com/openai/codex#authentication", hint: "Sign in with `codex`; Jevonian reads ~/.codex/auth.json."},
	{id: "antigravity", name: "Antigravity (Google)", typ: "gemini", url: "https://daily-cloudcode-pa.googleapis.com", source: "antigravity", billing: "subscription", keysURL: "https://antigravity.google", hint: "Sign in with the Antigravity IDE or `agy`; reads the local token and project id."},
	{id: "devin-subscription", name: "Devin", typ: "devin", url: "https://server.codeium.com", source: "devin", billing: "subscription", keysURL: "https://docs.devin.ai/cli", hint: "Sign in with `devin auth login`; Jevonian reads ~/.local/share/devin/credentials.toml."},
	{id: "cursor-subscription", name: "Cursor", typ: "cursor", url: "https://api2.cursor.sh", source: "cursor", billing: "subscription", keysURL: "https://cursor.com/install", hint: "Install Cursor's CLI and sign in with `cursor-agent login`; Jevonian reads its keychain entry or auth.json."},
	{id: "workbuddy-ai-subscription", name: "WorkBuddy AI", typ: "openai", url: "https://www.workbuddy.ai/v2", source: "workbuddy-ai", billing: "subscription", keysURL: "https://www.workbuddy.ai", hint: "Sign in through Jevonian (browser); or use a plaintext WorkBuddy AI desktop session (credential protection off)."},
	{id: "freebuff-subscription", name: "Freebuff", typ: "openai", url: "https://www.codebuff.com", source: "freebuff", billing: "subscription", keysURL: "https://freebuff.com", hint: "Free, ad-supported coding agent. Save opens a browser sign-in; or set FREEBUFF_AUTH_TOKEN. Daily quota resets at midnight Pacific."},
	{id: "jevonian-remote", name: "Jevonian (another machine)", typ: "both", url: "http://192.168.1.10:8789/v1", env: "JEVONIAN_REMOTE_KEY", explicitAPIKey: true, keysURL: "https://github.com/xinyao27/jevonian", hint: "Run Jevonian on the other machine with `--lan`, then use its LAN /v1 URL and a Jevonian API key from that dashboard. The LAN surface is key-protected and never exposes the dashboard."},
}

func findPreset(id string) *preset {
	for i := range presets {
		if presets[i].id == id {
			return &presets[i]
		}
	}
	return nil
}

// PresetViews is the dashboard `presets` payload (PRESETS in src/providers.ts).
func PresetViews() []map[string]any {
	out := make([]map[string]any, 0, len(presets))
	for _, p := range presets {
		v := map[string]any{"id": p.id, "name": p.name, "type": p.typ, "baseUrl": p.url, "hint": p.hint}
		if p.env != "" {
			v["apiKeyEnv"] = p.env
		}
		if p.keysURL != "" {
			v["keysUrl"] = p.keysURL
		}
		if p.source != "" {
			v["auth"] = "oauth"
			v["oauthSource"] = p.source
		} else if p.explicitAPIKey {
			v["auth"] = "api-key"
		}
		if p.billing != "" {
			v["billing"] = p.billing
		}
		if p.noKey {
			v["noKey"] = true
		}
		if p.sync {
			v["syncModels"] = true
		}
		out = append(out, v)
	}
	return out
}

func (p preset) provider() config.Provider {
	auth := config.AuthAPIKey
	if p.source != "" {
		auth = config.AuthOAuth
	}
	billing := config.BillingAPI
	if p.billing != "" {
		billing = config.ProviderBilling(p.billing)
	}
	out := config.Provider{Name: p.id, Type: config.ProviderType(p.typ), BaseURL: p.url, APIKeyEnv: p.env, Auth: auth, OAuthSource: config.OAuthSource(p.source), Billing: billing, NoKey: p.noKey, Models: []config.ModelEntry{}, InjectStreamUsage: true}
	if p.sync {
		yes := true
		out.SyncModels = &yes
	}
	return out
}
