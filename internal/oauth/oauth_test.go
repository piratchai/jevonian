package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMain isolates every source's own override so a provider login is what
// reads the temp dir, never a machine sign-in that happens to be present.
func TestMain(m *testing.M) {
	for _, key := range []string{
		"JEVONIAN_CLAUDE_CREDENTIALS", "JEVONIAN_CODEX_AUTH", "JEVONIAN_ANTIGRAVITY_TOKEN",
		"JEVONIAN_DEVIN_CREDENTIALS", "JEVONIAN_WORKBUDDY_AI_AUTH", "JEVONIAN_CURSOR_AUTH",
		"CLAUDE_CONFIG_DIR", "CODEX_HOME",
	} {
		_ = os.Unsetenv(key)
	}
	os.Exit(m.Run())
}

func jwt(expSeconds int64, sub string) string {
	enc := func(v any) string {
		data, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(data)
	}
	return enc(map[string]any{"alg": "none", "typ": "JWT"}) + "." +
		enc(map[string]any{"exp": expSeconds, "sub": sub}) + ".sig"
}

// resolveHTTP routes a specific URL to a JSON response; others fail.
type stubTransport struct {
	hits   *[]string
	match  string
	status int
	body   any
}

func (s stubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	*s.hits = append(*s.hits, req.URL.String())
	if s.match != "" && req.URL.String() != s.match {
		return nil, fmt.Errorf("unexpected url %s", req.URL)
	}
	payload, _ := json.Marshal(s.body)
	return &http.Response{
		StatusCode: s.status,
		Status:     http.StatusText(s.status),
		Header:     http.Header{"content-type": {"application/json"}},
		Body:       ioNopCloser{strings.NewReader(string(payload))},
		Request:    req,
	}, nil
}

type ioNopCloser struct{ *strings.Reader }

func (c ioNopCloser) Close() error { return nil }

func stubClient(hits *[]string, match string, status int, body any) *http.Client {
	return &http.Client{Transport: stubTransport{hits, match, status, body}}
}

func writeClaude(t *testing.T, path, accessToken string, fresh bool) {
	t.Helper()
	expires := time.Now().UnixMilli() + 3_600_000
	if !fresh {
		expires = time.Now().UnixMilli() - 1_000
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]any{
		"claudeAiOauth": map[string]any{
			"accessToken":  accessToken,
			"refreshToken": "refresh-" + accessToken,
			"expiresAt":    expires,
		},
	})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeCodex(t *testing.T, path, token, accountID string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]any{
		"tokens": map[string]any{
			"access_token":  token,
			"refresh_token": "refresh-1",
			"account_id":    accountID,
		},
	})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeFreshToken(t *testing.T) {
	dir := t.TempDir()
	claudePath := filepath.Join(dir, "claude-credentials.json")
	t.Setenv("JEVONIAN_CLAUDE_CREDENTIALS", claudePath)
	writeClaude(t, claudePath, "fresh-token", true)
	var hits []string
	r := &Resolver{HTTP: stubClient(&hits, "", 500, nil)}
	got, err := r.Resolve(t.Context(), ResolveOptions{Source: "claude-code"})
	if err != nil || got.Token != "fresh-token" {
		t.Fatalf("%+v %v", got, err)
	}
	if len(hits) != 0 {
		t.Fatalf("fresh token refreshed: %v", hits)
	}
}

func TestClaudeRefreshWritesBack(t *testing.T) {
	dir := t.TempDir()
	claudePath := filepath.Join(dir, "claude-credentials.json")
	t.Setenv("JEVONIAN_CLAUDE_CREDENTIALS", claudePath)
	writeClaude(t, claudePath, "old-token", false)
	var hits []string
	r := &Resolver{HTTP: stubClient(&hits, claudeRefreshURL, 200,
		map[string]any{"access_token": "new-token", "refresh_token": "refresh-2", "expires_in": 3600})}
	got, err := r.Resolve(t.Context(), ResolveOptions{Source: "claude-code"})
	if err != nil || got.Token != "new-token" {
		t.Fatalf("%+v %v", got, err)
	}
	written, ok := readJSONFile(claudePath)
	if !ok {
		t.Fatal("no written file")
	}
	oauth := asRecord(written["claudeAiOauth"])
	if oauth["accessToken"] != "new-token" || oauth["refreshToken"] != "refresh-2" {
		t.Fatalf("write-back: %+v", oauth)
	}
}

func TestClaudeMissing(t *testing.T) {
	t.Setenv("JEVONIAN_CLAUDE_CREDENTIALS", filepath.Join(t.TempDir(), "absent.json"))
	_, err := (&Resolver{}).Resolve(t.Context(), ResolveOptions{Source: "claude-code"})
	if err == nil || !strings.Contains(err.Error(), "Claude Code credentials not found") {
		t.Fatalf("err = %v", err)
	}
}

func TestAntigravityKeyringPayloadRefresh(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "antigravity.json")
	data, _ := json.Marshal(map[string]any{
		"token": map[string]any{
			"access_token":  "old",
			"refresh_token": "refresh-1",
			"expiry":        time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
		},
		"auth_method": "consumer",
	})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JEVONIAN_ANTIGRAVITY_TOKEN", path)
	var hits []string
	r := &Resolver{HTTP: stubClient(&hits, antigravityRefreshURL, 200,
		map[string]any{"access_token": "new-token", "expires_in": 3600})}
	got, err := r.Resolve(t.Context(), ResolveOptions{Source: "antigravity"})
	if err != nil || got.Token != "new-token" {
		t.Fatalf("%+v %v", got, err)
	}
	written, _ := readJSONFile(path)
	token := asRecord(written["token"])
	if token["access_token"] != "new-token" || token["refresh_token"] != "refresh-1" {
		t.Fatalf("write-back: %+v", token)
	}
}

func TestAntigravityFreshNoRefresh(t *testing.T) {
	path := filepath.Join(t.TempDir(), "antigravity-fresh.json")
	data, _ := json.Marshal(map[string]any{
		"token": map[string]any{
			"access_token":  "fresh",
			"refresh_token": "refresh-1",
			"expiry":        time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		},
	})
	_ = os.WriteFile(path, data, 0o600)
	t.Setenv("JEVONIAN_ANTIGRAVITY_TOKEN", path)
	var hits []string
	r := &Resolver{HTTP: stubClient(&hits, "", 500, nil)}
	got, err := r.Resolve(t.Context(), ResolveOptions{Source: "antigravity"})
	if err != nil || got.Token != "fresh" || len(hits) != 0 {
		t.Fatalf("%+v %v hits %v", got, err, hits)
	}
}

func TestDevinTOML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.toml")
	content := "# written by devin auth login\n" +
		`windsurf_api_key = "devin-session-token$abc\"def\\\\ghi"` + "\n" +
		`api_server_url = "https://server.example.com/"` + "\n" +
		`devin_webapp_host = "https://app.devin.ai"` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JEVONIAN_DEVIN_CREDENTIALS", path)
	r := &Resolver{}
	got, err := r.Resolve(t.Context(), ResolveOptions{Source: "devin"})
	if err != nil {
		t.Fatal(err)
	}
	want := `devin-session-token$abc"def\\ghi`
	if got.Token != want {
		t.Fatalf("token %q want %q", got.Token, want)
	}
	if got.ExpiresAt != 0 {
		t.Fatalf("devin tokens never expire: %d", got.ExpiresAt)
	}
	if !r.HasCredential("devin", nil) {
		t.Fatal("hasCredential")
	}
	if CredentialLabel("devin") != "Devin credentials" {
		t.Fatalf("label %q", CredentialLabel("devin"))
	}
	if DevinServerURL(nil) != "https://server.example.com" {
		t.Fatalf("server url %q", DevinServerURL(nil))
	}
}

func TestDevinReReadsAfterInvalidate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.toml")
	t.Setenv("JEVONIAN_DEVIN_CREDENTIALS", path)
	_ = os.WriteFile(path, []byte(`windsurf_api_key = "one"`+"\n"), 0o600)
	r := &Resolver{}
	first, err := r.Resolve(t.Context(), ResolveOptions{Source: "devin"})
	if err != nil || first.Token != "one" {
		t.Fatalf("%+v %v", first, err)
	}
	_ = os.WriteFile(path, []byte(`windsurf_api_key = "two"`+"\n"), 0o600)
	r.Invalidate("devin", nil)
	second, err := r.Resolve(t.Context(), ResolveOptions{Source: "devin"})
	if err != nil || second.Token != "two" {
		t.Fatalf("%+v %v", second, err)
	}
}

func TestDevinMissingAndShapeless(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("JEVONIAN_DEVIN_CREDENTIALS", filepath.Join(dir, "missing.toml"))
	r := &Resolver{}
	_, err := r.Resolve(t.Context(), ResolveOptions{Source: "devin"})
	if err == nil || err.Error() !=
		"Devin credentials not found. Sign in with `devin auth login`, or set JEVONIAN_DEVIN_CREDENTIALS." {
		t.Fatalf("err = %v", err)
	}
	if r.HasCredential("devin", nil) {
		t.Fatal("hasCredential on missing")
	}
	path := filepath.Join(dir, "credentials.toml")
	_ = os.WriteFile(path, []byte(`api_server_url = "https://server.codeium.com"`+"\n"), 0o600)
	t.Setenv("JEVONIAN_DEVIN_CREDENTIALS", path)
	_, err = r.Resolve(t.Context(), ResolveOptions{Source: "devin"})
	if err == nil || !strings.Contains(err.Error(), "unexpected shape") {
		t.Fatalf("err = %v", err)
	}
}

func TestDevinServerURLDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.toml")
	t.Setenv("JEVONIAN_DEVIN_CREDENTIALS", path)
	if got := DevinServerURL(nil); got != "https://server.codeium.com" {
		t.Fatalf("default %q", got)
	}
	_ = os.WriteFile(path, []byte(`windsurf_api_key = "t"`+"\n"+`api_server_url = "https://server.example.com/"`+"\n"), 0o600)
	if got := DevinServerURL(nil); got != "https://server.example.com" {
		t.Fatalf("file %q", got)
	}
}

func TestCodexTokenAndAccount(t *testing.T) {
	dir := t.TempDir()
	codexPath := filepath.Join(dir, "auth.json")
	t.Setenv("JEVONIAN_CODEX_AUTH", codexPath)
	token := jwt(time.Now().Unix()+3600, "")
	writeCodex(t, codexPath, token, "acct_1")
	var hits []string
	r := &Resolver{HTTP: stubClient(&hits, "", 500, nil)}
	got, err := r.Resolve(t.Context(), ResolveOptions{Source: "codex"})
	if err != nil || got.Token != token || got.AccountID != "acct_1" {
		t.Fatalf("%+v %v", got, err)
	}
	if len(hits) != 0 {
		t.Fatalf("fresh token hit network: %v", hits)
	}
}

func TestCodexRefreshWritesRotation(t *testing.T) {
	dir := t.TempDir()
	codexPath := filepath.Join(dir, "auth.json")
	t.Setenv("JEVONIAN_CODEX_AUTH", codexPath)
	expired := jwt(time.Now().Unix()-10, "")
	fresh := jwt(time.Now().Unix()+3600, "")
	writeCodex(t, codexPath, expired, "acct_1")
	var hits []string
	r := &Resolver{HTTP: stubClient(&hits, codexRefreshURL, 200,
		map[string]any{"access_token": fresh, "refresh_token": "refresh-2"})}
	got, err := r.Resolve(t.Context(), ResolveOptions{Source: "codex"})
	if err != nil || got.Token != fresh || got.AccountID != "acct_1" {
		t.Fatalf("%+v %v", got, err)
	}
	written, _ := readJSONFile(codexPath)
	tokens := asRecord(written["tokens"])
	if tokens["access_token"] != fresh || tokens["refresh_token"] != "refresh-2" {
		t.Fatalf("rotation write-back: %+v", tokens)
	}
	if _, ok := written["last_refresh"].(string); !ok {
		t.Fatalf("last_refresh missing: %+v", written)
	}
}

// ---------------------------------------------------------------------------
// Multi-account (multi-account.test.ts)
// ---------------------------------------------------------------------------

func TestCodexSecondAccountByHomeAndFile(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, ".codex-work")
	token := jwt(time.Now().Unix()+3600, "work")
	writeCodex(t, filepath.Join(home, "auth.json"), token, "acct_work")

	r := &Resolver{}
	got, err := r.Resolve(t.Context(), ResolveOptions{Source: "codex", Login: &Login{Home: home}})
	if err != nil || got.Token != token || got.AccountID != "acct_work" {
		t.Fatalf("%+v %v", got, err)
	}

	explicit := filepath.Join(dir, "auth.json")
	writeCodex(t, explicit, jwt(time.Now().Unix()+3600, ""), "acct_explicit")
	got, err = r.Resolve(t.Context(), ResolveOptions{
		Source: "codex", Login: &Login{CredentialsPath: explicit},
	})
	if err != nil || got.AccountID != "acct_explicit" {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestClaudeSecondAccountNoKeychainFallback(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.json")
	r := &Resolver{}
	_, err := r.Resolve(t.Context(), ResolveOptions{
		Source: "claude-code", Login: &Login{CredentialsPath: missing},
	})
	if err == nil || !strings.Contains(err.Error(), "Claude Code credentials not found") {
		t.Fatalf("err = %v", err)
	}
}

func TestDevinSecondAccount(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "devin-work")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(home, "credentials.toml"),
		[]byte(`windsurf_api_key = "work-devin"`+"\n"+`api_server_url = "https://server.work.com/"`+"\n"), 0o600)
	login := &Login{Home: home}
	r := &Resolver{}
	got, err := r.Resolve(t.Context(), ResolveOptions{Source: "devin", Login: login})
	if err != nil || got.Token != "work-devin" {
		t.Fatalf("%+v %v", got, err)
	}
	if DevinServerURL(login) != "https://server.work.com" {
		t.Fatalf("url %q", DevinServerURL(login))
	}
}

func TestTwoCodexAccountsCacheSeparately(t *testing.T) {
	dir := t.TempDir()
	workHome := filepath.Join(dir, ".codex-work")
	homeHome := filepath.Join(dir, ".codex-home")
	workToken := jwt(time.Now().Unix()+3600, "work")
	homeToken := jwt(time.Now().Unix()+3600, "home")
	writeCodex(t, filepath.Join(workHome, "auth.json"), workToken, "acct_work")
	writeCodex(t, filepath.Join(homeHome, "auth.json"), homeToken, "acct_home")

	r := &Resolver{}
	work, err := r.Resolve(t.Context(), ResolveOptions{Source: "codex", Login: &Login{Home: workHome}})
	if err != nil {
		t.Fatal(err)
	}
	home, err := r.Resolve(t.Context(), ResolveOptions{Source: "codex", Login: &Login{Home: homeHome}})
	if err != nil {
		t.Fatal(err)
	}
	if work.AccountID != "acct_work" || home.AccountID != "acct_home" || work.Token == home.Token {
		t.Fatalf("work %+v home %+v", work, home)
	}
}

func TestInvalidateOneAccountOnly(t *testing.T) {
	dir := t.TempDir()
	workHome := filepath.Join(dir, ".codex-work")
	homeHome := filepath.Join(dir, ".codex-home")
	fresh1 := jwt(time.Now().Unix()+3600, "")
	fresh2 := jwt(time.Now().Unix()+3601, "")
	writeCodex(t, filepath.Join(workHome, "auth.json"), fresh1, "acct_work")
	writeCodex(t, filepath.Join(homeHome, "auth.json"), fresh2, "acct_home")
	r := &Resolver{}
	if _, err := r.Resolve(t.Context(), ResolveOptions{Source: "codex", Login: &Login{Home: workHome}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(t.Context(), ResolveOptions{Source: "codex", Login: &Login{Home: homeHome}}); err != nil {
		t.Fatal(err)
	}

	rotated := jwt(time.Now().Unix()+7200, "")
	writeCodex(t, filepath.Join(workHome, "auth.json"), rotated, "acct_work")
	r.Invalidate("codex", &Login{Home: workHome})
	work, _ := r.Resolve(t.Context(), ResolveOptions{Source: "codex", Login: &Login{Home: workHome}})
	home, _ := r.Resolve(t.Context(), ResolveOptions{Source: "codex", Login: &Login{Home: homeHome}})
	if work.Token != rotated || home.Token != fresh2 {
		t.Fatalf("work %q home %q", work.Token, home.Token)
	}
}

func TestBareInvalidateClearsEveryLogin(t *testing.T) {
	dir := t.TempDir()
	workHome := filepath.Join(dir, ".codex-work")
	writeCodex(t, filepath.Join(workHome, "auth.json"), jwt(time.Now().Unix()+3600, ""), "a")
	r := &Resolver{}
	if _, err := r.Resolve(t.Context(), ResolveOptions{Source: "codex", Login: &Login{Home: workHome}}); err != nil {
		t.Fatal(err)
	}
	r.Invalidate("codex", nil)
	next := jwt(time.Now().Unix()+9999, "")
	writeCodex(t, filepath.Join(workHome, "auth.json"), next, "a")
	after, _ := r.Resolve(t.Context(), ResolveOptions{Source: "codex", Login: &Login{Home: workHome}})
	if after.Token != next {
		t.Fatalf("after %q", after.Token)
	}
}

func TestHasCredentialScoped(t *testing.T) {
	dir := t.TempDir()
	r := &Resolver{}
	home := filepath.Join(dir, ".codex-work")
	if r.HasCredential("codex", &Login{Home: home}) {
		t.Fatal("premature credential")
	}
	writeCodex(t, filepath.Join(home, "auth.json"), jwt(time.Now().Unix()+3600, ""), "a")
	if !r.HasCredential("codex", &Login{Home: home}) {
		t.Fatal("credential not found")
	}
}

func TestSourceEnvOverrideBeatsLoginHome(t *testing.T) {
	// The source's own override wins over a provider login: an explicit
	// environment is a deliberate local choice (src/oauth.ts).
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", filepath.Join(dir, ".codex-env"))
	if got := CodexAuthPath(&Login{Home: filepath.Join(dir, "work")}); got != filepath.Join(dir, ".codex-env", "auth.json") {
		t.Fatalf("codex path %q", got)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(dir, ".claude-env"))
	if got := ClaudeCredentialsPath(&Login{Home: filepath.Join(dir, "work")}); got != filepath.Join(dir, ".claude-env", ".credentials.json") {
		t.Fatalf("claude path %q", got)
	}
	// An explicit login file still beats the *_HOME dir.
	if got := CodexAuthPath(&Login{CredentialsPath: "/x/auth.json"}); got != "/x/auth.json" {
		t.Fatalf("codex file %q", got)
	}
}

func TestClaudeSecondAccountByHome(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, ".claude-work")
	writeClaude(t, filepath.Join(home, ".credentials.json"), "work-token", true)
	var hits []string
	r := &Resolver{HTTP: stubClient(&hits, "", 500, nil)}
	got, err := r.Resolve(t.Context(), ResolveOptions{Source: "claude-code", Login: &Login{Home: home}})
	if err != nil || got.Token != "work-token" || len(hits) != 0 {
		t.Fatalf("%+v %v %v", got, err, hits)
	}
	explicit := filepath.Join(dir, "elsewhere.json")
	writeClaude(t, explicit, "explicit-token", true)
	got, err = r.Resolve(t.Context(), ResolveOptions{Source: "claude-code", Login: &Login{CredentialsPath: explicit}})
	if err != nil || got.Token != "explicit-token" {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestClaudeKeychainFallback(t *testing.T) {
	if !isDarwin() {
		t.Skip("keychain fallback is macOS-only")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // no file there
	payload, _ := json.Marshal(map[string]any{"claudeAiOauth": map[string]any{
		"accessToken": "kc-token", "expiresAt": time.Now().UnixMilli() + 3_600_000,
	}})
	var calls [][]string
	kc := SecurityKeychain{Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		return append(payload, '\n'), nil
	}}
	r := &Resolver{Keychain: kc}
	got, err := r.Resolve(t.Context(), ResolveOptions{
		Source: "claude-code",
		Login:  &Login{KeychainService: "svc-work", KeychainAccount: "acct"},
	})
	if err != nil || got.Token != "kc-token" {
		t.Fatalf("%+v %v", got, err)
	}
	want := []string{"security", "find-generic-password", "-s", "svc-work", "-a", "acct", "-w"}
	if len(calls) != 1 || strings.Join(calls[0], " ") != strings.Join(want, " ") {
		t.Fatalf("calls %v", calls)
	}
}

// ---------------------------------------------------------------------------
// Sources + wire pairing
// ---------------------------------------------------------------------------

func TestSourceRegistry(t *testing.T) {
	for _, s := range []Source{"claude-code", "codex", "antigravity", "devin", "cursor", "workbuddy-ai", "static"} {
		if !IsSource(s) {
			t.Fatalf("missing source %q", s)
		}
	}
	if IsSource("nope") || ParseSource(42) != "" {
		t.Fatal("unknown source accepted")
	}
}

func TestWirePairing(t *testing.T) {
	if typ, err := WireType("devin", "devin", true); err != nil || typ != "devin" {
		t.Fatalf("%q %v", typ, err)
	}
	if typ, err := WireType("cursor", "openai", false); err != nil || typ != "cursor" {
		t.Fatalf("adopt forced wire: %q %v", typ, err)
	}
	if _, err := WireType("devin", "openai", true); err == nil ||
		err.Error() != "Devin credentials require --type devin." {
		t.Fatalf("err = %v", err)
	}
	if WireMismatch("devin", "api-key", "") !=
		"Devin wire requires --auth oauth --oauth-source devin (or static)." {
		t.Fatal("mismatch api-key")
	}
	if WireMismatch("devin", "oauth", "codex") !=
		"Devin wire requires --oauth-source devin (or static)." {
		t.Fatal("mismatch source")
	}
	if WireMismatch("devin", "oauth", "static") != "" ||
		WireMismatch("openai", "api-key", "") != "" {
		t.Fatal("false mismatch")
	}
}

func TestStaticSource(t *testing.T) {
	r := &Resolver{}
	if _, err := r.Resolve(t.Context(), ResolveOptions{Source: "static"}); err != ErrNoStaticToken {
		t.Fatalf("err = %v", err)
	}
	got, err := r.Resolve(t.Context(), ResolveOptions{Source: "static", StaticToken: "sk-x"})
	if err != nil || got.Token != "sk-x" {
		t.Fatalf("%+v %v", got, err)
	}
	if r.HasCredential("static", nil) {
		t.Fatal("static is not a credential")
	}
}

func TestWorkbuddySourceResolvesViaClient(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wb.json")
	t.Setenv("JEVONIAN_WORKBUDDY_AI_AUTH", path)
	if err := writeJSONFile(path, map[string]any{
		"uid":         "wb-uid",
		"accessToken": "wb-access",
		"expiresAt":   time.Now().UnixMilli() + 3_600_000,
		"domain":      "www.workbuddy.ai",
	}, true); err != nil {
		t.Fatal(err)
	}
	r := &Resolver{}
	got, err := r.Resolve(t.Context(), ResolveOptions{Source: "workbuddy-ai"})
	if err != nil || got.Token != "wb-access" || got.AccountID != "wb-uid" || got.Domain != "www.workbuddy.ai" {
		t.Fatalf("%+v %v", got, err)
	}
	if !r.HasCredential("workbuddy-ai", nil) {
		t.Fatal("hasCredential")
	}
	if CredentialLabel("workbuddy-ai") != "WorkBuddy AI credentials" {
		t.Fatal("label")
	}
}

func TestParseFlatTOML(t *testing.T) {
	got := ParseFlatTOML(strings.Join([]string{
		`# comment`,
		`a = "1"`,
		`b = 'lit'`,
		`c = "esc\"aped\\back\n"`,
		`[table]`,
		`nested.x = "y"   # inline comment`,
		`d = "broken`, // unterminated: ignored
		``,
	}, "\n"))
	if got["a"] != "1" || got["b"] != "lit" || got["nested.x"] != "y" {
		t.Fatalf("flat toml: %+v", got)
	}
	if got["c"] != "esc\"aped\\back\n" {
		t.Fatalf("escapes: %q", got["c"])
	}
	if _, ok := got["d"]; ok {
		t.Fatal("unterminated string parsed")
	}
}
