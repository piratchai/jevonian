package multiacct

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestLoginKeyAndLabel(t *testing.T) {
	if Key("codex", nil) != "codex" {
		t.Fatal("bare source")
	}
	if Key("codex", &Login{}) != "codex" {
		t.Fatal("empty login keys as source")
	}
	login := &Login{Home: "/h", KeychainAccount: "a"}
	if Key("codex", login) != "codex\x00/h\x00\x00\x00a" {
		t.Fatalf("key %q", Key("codex", login))
	}
	if RenewalKey(nil) != "" || RenewalKey(login) != "/h\x00\x00\x00a" {
		t.Fatal("renewal key")
	}
	if Label(nil) != "" {
		t.Fatal("no login → no label")
	}
	if Label(&Login{Label: "work"}) != "work" ||
		Label(&Login{CredentialsPath: "/f"}) != "/f" ||
		Label(&Login{Home: "/h"}) != "/h" ||
		Label(&Login{KeychainService: "kc"}) != "kc" ||
		Label(&Login{Label: " "}) != " " { // label wins verbatim
		t.Fatal("label precedence")
	}
	if Label(&Login{KeychainAccount: "acct"}) != "custom" {
		t.Fatal("account-only login labels custom")
	}
}

func TestFromFlags(t *testing.T) {
	login, err := FromFlags(map[string]string{}, "oauth")
	if login != nil || err != nil {
		t.Fatalf("empty: %v %v", login, err)
	}
	if _, err := FromFlags(map[string]string{"login-home": "~/.claude-work"}, "api-key"); err != ErrLoginNeedsOAuth {
		t.Fatalf("api-key refused: %v", err)
	}
	login, err = FromFlags(map[string]string{
		"login-home":     "~/work",
		"login-file":     "/abs/creds.json",
		"login-keychain": "svc:acct",
		"login-label":    "work",
	}, "oauth")
	if err != nil {
		t.Fatal(err)
	}
	if login.Label != "work" || login.KeychainService != "svc" || login.KeychainAccount != "acct" ||
		login.CredentialsPath != "/abs/creds.json" {
		t.Fatalf("%+v", login)
	}
	home, _ := os.UserHomeDir()
	if login.Home != filepath.Join(home, "work") {
		t.Fatalf("~ expansion: %q", login.Home)
	}
}

// ---------------------------------------------------------------------------
// src/account.ts — native Codex routing
// ---------------------------------------------------------------------------

func headers(kvs ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kvs); i += 2 {
		h.Set(kvs[i], kvs[i+1])
	}
	return h
}

func TestShouldProxyNativeCodex(t *testing.T) {
	// Jevonian models stay local.
	if route, ok := ShouldProxyNativeCodex("jevonian/auto", nil); ok || route != "" {
		t.Fatal("jevonian/auto routed away")
	}
	if _, ok := ShouldProxyNativeCodex("auto", headers("chatgpt-account-id", "a")); ok {
		t.Fatal("bare auto routed away")
	}
	// Account session → ChatGPT backend.
	if route, ok := ShouldProxyNativeCodex("gpt-5.6-sol", headers("chatgpt-account-id", "acct-1")); !ok || route != RouteChatGPT {
		t.Fatalf("account session → %q", route)
	}
	// A real API key → OpenAI.
	if route, ok := ShouldProxyNativeCodex("gpt-5.6-sol", headers("authorization", "Bearer sk-test")); !ok || route != RouteOpenAI {
		t.Fatalf("api key → %q", route)
	}
	// The loopback sentinel is not an OpenAI credential.
	if _, ok := ShouldProxyNativeCodex("gpt-5.6-sol", headers("authorization", "Bearer jevonian-local")); ok {
		t.Fatal("sentinel forwarded")
	}
	if _, ok := ShouldProxyNativeCodex("gpt-5.6-sol", nil); ok {
		t.Fatal("no credentials forwarded")
	}
}

func TestNativeCodexTarget(t *testing.T) {
	req, _ := http.NewRequest("POST", "http://127.0.0.1:8787/v1/responses?x=1", nil)
	got, err := NativeCodexTarget(RouteChatGPT, req)
	// new URL("/responses?x=1", "https://chatgpt.com/backend-api/codex/")
	// resolves with the absolute path — the base path is replaced.
	if err != nil || got.String() != "https://chatgpt.com/responses?x=1" {
		t.Fatalf("chatgpt %v %v", got, err)
	}
	got, err = NativeCodexTarget(RouteOpenAI, req)
	if err != nil || got.String() != "https://api.openai.com/v1/responses?x=1" {
		t.Fatalf("openai %v %v", got, err)
	}
	t.Setenv("JEVONIAN_CHATGPT_CODEX_UPSTREAM", "http://127.0.0.1:9/")
	got, _ = NativeCodexTarget(RouteChatGPT, req)
	if got.String() != "http://127.0.0.1:9/responses?x=1" {
		t.Fatalf("override %v", got)
	}
}

// ---------------------------------------------------------------------------
// src/credentials.ts
// ---------------------------------------------------------------------------

func TestCredentialStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := &Store{Path: filepath.Join(dir, "credentials.json")}
	if err := store.Set("deepseek", "sk-test-1234567890"); err != nil {
		t.Fatal(err)
	}
	if got := store.Get("deepseek"); got != "sk-test-1234567890" {
		t.Fatalf("get %q", got)
	}
	info, err := os.Stat(store.Path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v %v", info.Mode(), err)
	}
	if err := store.Remove("deepseek"); err != nil || store.Get("deepseek") != "" {
		t.Fatal("remove")
	}
	// Removing absent is a no-op.
	if err := store.Remove("deepseek"); err != nil {
		t.Fatal(err)
	}
}

func TestCredentialStoreMalformed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	_ = os.WriteFile(path, []byte("{ not json"), 0o600)
	store := &Store{Path: path}
	if got := store.Get("missing"); got != "" {
		t.Fatalf("%q", got)
	}
	if err := store.Set("deepseek", "sk-test"); err != nil || store.Get("deepseek") != "sk-test" {
		t.Fatal("rewrite after malformed")
	}
}

func TestMaskKey(t *testing.T) {
	if MaskKey("sk-1234567890abcdef") != "sk-1…cdef" || MaskKey("short") != "****" {
		t.Fatal("masking")
	}
}
