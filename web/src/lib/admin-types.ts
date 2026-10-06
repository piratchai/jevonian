/**
 * The admin API's shared vocabulary: the value sets the server accepts and the dashboard
 * offers. Type-only on purpose — the web app imports this file directly, so nothing here may
 * pull server code into the browser bundle.
 *
 * When a wire or a sign-in source is added, it is added here once; the Go server checks its
 * runtime lists against these unions, and the dashboard's form types follow.
 */

/** Every wire a provider can speak. */
export type ProviderTypeName =
  | "openai"
  | "anthropic"
  | "responses"
  | "both"
  | "gemini"
  | "devin"
  | "cursor";

/** Every local sign-in the router can read. */
export type OAuthSourceName =
  | "claude-code"
  | "codex"
  | "antigravity"
  | "devin"
  | "cursor"
  | "workbuddy-ai"
  | "freebuff"
  | "static";

export type ProviderAuthName = "api-key" | "oauth";

export type ProviderBillingName = "api" | "subscription";
