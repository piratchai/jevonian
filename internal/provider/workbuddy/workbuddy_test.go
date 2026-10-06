package workbuddy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xinyao27/jevonian/internal/provider/multiacct"
)

func encrypted(v string) map[string]any {
	return map[string]any{"$wbEncrypted": 1, "envelope": v}
}

func TestPlainStringRejectsEncryptedEnvelopes(t *testing.T) {
	if PlainString("tok") != "tok" {
		t.Fatal("plain string")
	}
	if PlainString(encrypted("x")) != "" {
		t.Fatal("encrypted envelope should read empty")
	}
	got := ParseSession(map[string]any{
		"account": map[string]any{"uid": "u1", "nickname": encrypted("n")},
		"auth": map[string]any{
			"accessToken":  encrypted("a"),
			"refreshToken": encrypted("r"),
			"domain":       "www.workbuddy.ai",
		},
	}, time.Now())
	if got != nil {
		t.Fatalf("encrypted desktop session parsed: %+v", got)
	}
}

func TestParseDesktopAndFlatSessions(t *testing.T) {
	desktop := ParseSession(map[string]any{
		"account": map[string]any{"uid": "uid-1", "nickname": "Ada"},
		"auth": map[string]any{
			"accessToken":  "access",
			"refreshToken": "refresh",
			"expiresAt":    float64(9_000_000_000_000),
			"domain":       "www.workbuddy.ai",
		},
	}, time.Now())
	if desktop == nil || desktop.UID != "uid-1" || desktop.AccessToken != "access" ||
		desktop.RefreshToken != "refresh" || desktop.Nickname != "Ada" ||
		desktop.Domain != "www.workbuddy.ai" {
		t.Fatalf("desktop: %+v", desktop)
	}

	flat := ParseSession(map[string]any{
		"uid":          "uid-2",
		"accessToken":  "a2",
		"refreshToken": "r2",
		"expiresAt":    float64(1),
		"domain":       "www.codebuddy.ai",
	}, time.Now())
	if flat == nil || flat.UID != "uid-2" || flat.AccessToken != "a2" || flat.Domain != "www.codebuddy.ai" {
		t.Fatalf("flat: %+v", flat)
	}
}

func TestDesktopExpiresInBecomesAbsolute(t *testing.T) {
	now := time.UnixMilli(1_000_000)
	got := ParseSession(map[string]any{
		"account": map[string]any{"uid": "u"},
		"auth":    map[string]any{"accessToken": "a", "expiresIn": float64(60), "refreshExpiresIn": float64(120)},
	}, now)
	if got.ExpiresAt != 1_060_000 || got.RefreshExpiresAt != 1_120_000 {
		t.Fatalf("expiry: %+v", got)
	}
	if got.Domain != DefaultDomain {
		t.Fatalf("domain default: %q", got.Domain)
	}
}

func TestAuthAndClientHeaders(t *testing.T) {
	auth := AuthHeaders(Creds{UID: "u", AccessToken: "tok", Domain: "www.workbuddy.ai"})
	want := map[string]string{
		"authorization": "Bearer tok",
		"x-user-id":     "u",
		"x-domain":      "www.workbuddy.ai",
		"x-product":     "SaaS",
		"x-ide-type":    "WorkBuddy",
	}
	for k, v := range want {
		if auth[k] != v {
			t.Fatalf("%s = %q, want %q", k, auth[k], v)
		}
	}
	if !strings.HasPrefix(auth["user-agent"], "WorkBuddy/") {
		t.Fatalf("user-agent %q", auth["user-agent"])
	}

	client := ClientHeaders("abc123")
	if client["x-requested-with"] != "XMLHttpRequest" || client["x-agent-intent"] != "craft" ||
		client["x-ide-name"] != "WorkBuddy" || client["x-conversation-id"] != "abc123" {
		t.Fatalf("client headers: %+v", client)
	}
}

func TestClientHeadersSanitizeAndRandomize(t *testing.T) {
	long := strings.Repeat("a-b_", 20)
	h := ClientHeaders(long)
	if h["x-conversation-id"] != strings.Repeat("ab", 16) {
		t.Fatalf("sanitized: %q", h["x-conversation-id"])
	}
	random := ClientHeaders("")
	if len(random["x-conversation-id"]) != 32 || random["x-request-id"] == random["x-conversation-id"] {
		t.Fatalf("random ids: %+v", random)
	}
}

func TestApplyHeadersLiveTokenWins(t *testing.T) {
	headers := map[string]string{
		"authorization": "Bearer stale",
		"x-user-id":     "old",
		"x-product":     "Custom",
	}
	ApplyHeaders(headers, Creds{UID: "u", AccessToken: "live"}, "sess")
	if headers["authorization"] != "Bearer live" || headers["x-user-id"] != "u" ||
		headers["x-domain"] != DefaultDomain {
		t.Fatalf("live creds lost: %+v", headers)
	}
	if headers["x-product"] != "Custom" {
		t.Fatalf("preset override lost: %+v", headers)
	}
	if headers["x-conversation-id"] != "sess" {
		t.Fatalf("session id: %+v", headers)
	}
}

func TestEnsureSystem(t *testing.T) {
	withSystem := EnsureSystem(map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": "keep"},
		map[string]any{"role": "user", "content": "hi"},
	}})
	if !reflect.DeepEqual(withSystem["messages"], []any{
		map[string]any{"role": "system", "content": "keep"},
		map[string]any{"role": "user", "content": "hi"},
	}) {
		t.Fatalf("withSystem: %+v", withSystem["messages"])
	}

	without := EnsureSystem(map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": "hi"},
	}})
	if !reflect.DeepEqual(without["messages"], []any{
		map[string]any{"role": "system", "content": SystemPrompt},
		map[string]any{"role": "user", "content": "hi"},
	}) {
		t.Fatalf("without: %+v", without["messages"])
	}
}

func TestReadWriteSessionFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "workbuddy-ai.json")
	t.Setenv("JEVONIAN_WORKBUDDY_AI_AUTH", path)
	if err := SaveSession(Creds{
		UID: "u", AccessToken: "a", RefreshToken: "r",
		ExpiresAt: 9_000_000_000_000, RefreshExpiresAt: 9_000_000_000_000,
		Domain: "www.workbuddy.ai", Nickname: "Ada",
	}, nil); err != nil {
		t.Fatal(err)
	}
	got := ReadSession(nil)
	if got == nil || got.UID != "u" || got.AccessToken != "a" || got.Nickname != "Ada" {
		t.Fatalf("read back: %+v", got)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode: %v %v", info.Mode(), err)
	}
}

func TestSignInAgainstMockServer(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("JEVONIAN_WORKBUDDY_AI_AUTH", filepath.Join(dir, "session.json"))

	var tokenPolls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok := func(data any) {
			w.Header().Set("content-type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": data})
		}
		switch r.URL.Path {
		case "/v2/plugin/auth/state":
			if r.URL.Query().Get("platform") != "workbuddy-ai" {
				t.Errorf("platform %q", r.URL.Query().Get("platform"))
			}
			ok(map[string]any{
				"state":   "s1",
				"authUrl": "https://www.workbuddy.ai/login?platform=workbuddy-ai&state=s1",
			})
		case "/v2/plugin/auth/token":
			// First poll: not ready yet (retry code), then the token.
			if tokenPolls.Add(1) == 1 {
				_ = json.NewEncoder(w).Encode(map[string]any{"code": retryCodeToken, "msg": "pending"})
				return
			}
			ok(map[string]any{
				"accessToken":      "access-1",
				"refreshToken":     "refresh-1",
				"expiresIn":        3600,
				"refreshExpiresIn": 7200,
				"domain":           "www.workbuddy.ai",
			})
		case "/v2/plugin/login/account":
			if r.Header.Get("authorization") != "Bearer access-1" {
				t.Errorf("account auth %q", r.Header.Get("authorization"))
			}
			ok(map[string]any{"uid": "uid-9", "nickname": "Tester"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	var opened []string
	client := &Client{
		HTTP:         server.Client(),
		OpenBrowser:  func(u string) { opened = append(opened, u) },
		PollInterval: 5 * time.Millisecond,
		PollTimeout:  5 * time.Second,
	}
	result, err := client.SignIn(context.Background(), server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(opened) != 1 || !strings.Contains(opened[0], "https://www.workbuddy.ai/login") ||
		!strings.Contains(opened[0], "version="+AppVersion) || !strings.Contains(opened[0], "loginSessionId=") {
		t.Fatalf("opened: %v", opened)
	}
	if result.User != "Tester" {
		t.Fatalf("user %q", result.User)
	}
	c := result.Creds
	if c.UID != "uid-9" || c.AccessToken != "access-1" || c.RefreshToken != "refresh-1" || c.Domain != "www.workbuddy.ai" {
		t.Fatalf("creds: %+v", c)
	}
	if got := ReadSession(nil); got == nil || got.AccessToken != "access-1" {
		t.Fatalf("persisted: %+v", got)
	}
	if tokenPolls.Load() != 2 {
		t.Fatalf("expected one retry-code poll, got %d polls", tokenPolls.Load())
	}
}

func TestSignInRejectsNonHTTPSAuthURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{
			"state": "s", "authUrl": "http://evil.example/login",
		}})
	}))
	defer server.Close()
	client := &Client{HTTP: server.Client(), OpenBrowser: func(string) { t.Fatal("must not open") }}
	if _, err := client.SignIn(context.Background(), server.URL, nil); err == nil ||
		!strings.Contains(err.Error(), "no sign-in page") {
		t.Fatalf("err = %v", err)
	}
}

func TestIgnoresEncryptedDesktopFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "workbuddy-desktop-ai.info")
	data, _ := json.Marshal(map[string]any{
		"account": map[string]any{"uid": "u"},
		"auth": map[string]any{
			"accessToken":  encrypted("x"),
			"refreshToken": encrypted("y"),
		},
	})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JEVONIAN_WORKBUDDY_AI_AUTH", path)
	if got := ReadSession(nil); got != nil {
		t.Fatalf("encrypted file read: %+v", got)
	}
}

func TestNoDesktopFallbackWithExplicitPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.json")
	t.Setenv("JEVONIAN_WORKBUDDY_AI_AUTH", missing)
	if ReadSession(nil) != nil {
		t.Fatal("env path must not fall back to desktop")
	}
	t.Setenv("JEVONIAN_WORKBUDDY_AI_AUTH", "")
	if ReadSession(&multiacct.Login{CredentialsPath: missing}) != nil {
		t.Fatal("login path must not fall back to desktop")
	}
}

func TestModelsFromConfig(t *testing.T) {
	models := ModelsFromConfig(map[string]any{
		"agents": []any{map[string]any{
			"name":   "cli",
			"models": []any{"default-model", "deepseek-v4.1-flash", "gpt-5.5", "kimi-k3"},
		}},
		"models": []any{map[string]any{"id": "ignored-when-cli-present"}},
	})
	if !reflect.DeepEqual(models, []string{"default-model", "deepseek-v4.1-flash", "gpt-5.5", "kimi-k3"}) {
		t.Fatalf("cli models: %v", models)
	}
	catalog := ModelsFromConfig(map[string]any{
		"models": []any{map[string]any{"id": "gpt-5.5"}, map[string]any{"id": "deepseek-v4.1-flash"}},
	})
	if !reflect.DeepEqual(catalog, []string{"gpt-5.5", "deepseek-v4.1-flash"}) {
		t.Fatalf("catalog: %v", catalog)
	}
}

func TestFetchModelsAndQuota(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("authorization") != "Bearer tok" || r.Header.Get("x-user-id") != "u" {
			t.Errorf("%s auth headers: %v", r.URL.Path, r.Header)
		}
		switch r.URL.Path {
		case "/v3/config":
			if !strings.HasPrefix(r.Header.Get("user-agent"), "CLI/") {
				t.Errorf("config UA %q", r.Header.Get("user-agent"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{
				"agents": []any{map[string]any{"name": "cli", "models": []any{"m1", "m2"}}},
			}})
		case "/billing/meter/get-user-resource-summary":
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{
				"Packages": []any{
					map[string]any{"CycleTotalCapacity": 1500, "CycleUsedCapacity": 300},
					map[string]any{"CycleTotalCapacity": "500", "CycleUsedCapacity": 200},
				},
				"IsPaidUser": true,
			}})
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	client := &Client{HTTP: server.Client()}
	creds := Creds{UID: "u", AccessToken: "tok"}
	models, err := client.FetchModels(context.Background(), creds, server.URL)
	if err != nil || !reflect.DeepEqual(models, []string{"m1", "m2"}) {
		t.Fatalf("models %v %v", models, err)
	}
	quota, err := client.FetchQuota(context.Background(), creds, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	// Numeric strings count, like TS Number("500").
	if quota.Plan != "Pro" || len(quota.Windows) != 1 {
		t.Fatalf("quota %+v", quota)
	}
	w := quota.Windows[0]
	if w.ID != "credits" || w.Label != "Credits" || w.UsedPercent != 25 || w.Note != "500 / 2.0k" {
		t.Fatalf("window %+v", w)
	}
}

func TestResolveRefreshesNearExpiry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wb.json")
	t.Setenv("JEVONIAN_WORKBUDDY_AI_AUTH", path)
	now := time.UnixMilli(10_000_000)
	if err := SaveSession(Creds{
		UID: "u", AccessToken: "old", RefreshToken: "r",
		ExpiresAt: now.UnixMilli() + 1_000, Domain: DefaultDomain,
	}, nil); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/plugin/auth/token/refresh" || r.Header.Get("x-refresh-token") != "r" {
			t.Errorf("unexpected refresh call %s %v", r.URL.Path, r.Header)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{
			"accessToken": "new", "expiresIn": 3600,
		}})
	}))
	defer server.Close()
	client := &Client{HTTP: server.Client(), Now: func() time.Time { return now }}
	got, err := client.Resolve(context.Background(), nil, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "new" || got.RefreshToken != "r" || got.ExpiresAt != now.UnixMilli()+3_600_000 {
		t.Fatalf("refreshed %+v", got)
	}
	if stored := ReadSession(nil); stored == nil || stored.AccessToken != "new" {
		t.Fatalf("not written back: %+v", stored)
	}
}

func TestResolveFallsBackToStaleTokenOnRefreshFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wb.json")
	t.Setenv("JEVONIAN_WORKBUDDY_AI_AUTH", path)
	_ = SaveSession(Creds{UID: "u", AccessToken: "old", RefreshToken: "r", ExpiresAt: 1}, nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 401, "msg": "nope"})
	}))
	defer server.Close()
	got, err := (&Client{HTTP: server.Client()}).Resolve(context.Background(), nil, server.URL)
	if err != nil || got.AccessToken != "old" {
		t.Fatalf("got %+v %v", got, err)
	}
}

func TestResolveMissing(t *testing.T) {
	t.Setenv("JEVONIAN_WORKBUDDY_AI_AUTH", filepath.Join(t.TempDir(), "none.json"))
	if _, err := (&Client{}).Resolve(context.Background(), nil, ""); err != ErrNoCredentials {
		t.Fatalf("err = %v", err)
	}
}

func TestEndpointFromBaseURL(t *testing.T) {
	cases := map[string]string{
		"https://www.workbuddy.ai/v2":  "https://www.workbuddy.ai",
		"https://www.workbuddy.ai/v2/": "https://www.workbuddy.ai",
		"http://127.0.0.1:9/v2":        "http://127.0.0.1:9",
		"":                             Endpoint,
	}
	for in, want := range cases {
		if got := EndpointFromBaseURL(in); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}
