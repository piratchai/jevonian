package parity

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/provider/multiacct"
	"github.com/xinyao27/jevonian/internal/provider/workbuddy"
	"github.com/xinyao27/jevonian/internal/server/admin"
)

// rewrite sends every request to the fake WorkBuddy server.
type rewrite struct{ to *url.URL }

func (r rewrite) RoundTrip(req *http.Request) (*http.Response, error) {
	c := req.Clone(req.Context())
	c.URL.Scheme, c.URL.Host = r.to.Scheme, r.to.Host
	return http.DefaultTransport.RoundTrip(c)
}

// POST /api/oauth/workbuddy-ai/signin runs the browser sign-in and stores the
// session without a provider being saved (src/admin.ts).
func TestAdminWorkbuddySignInEndpointWithoutProviderSave(t *testing.T) {
	dir := t.TempDir()
	session := filepath.Join(dir, "wb.json")
	t.Setenv("JEVONIAN_WORKBUDDY_AI_AUTH", session)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok := func(data any) { _ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": data}) }
		switch r.URL.Path {
		case "/v2/plugin/auth/state":
			ok(map[string]any{"state": "s", "authUrl": "https://www.workbuddy.ai/login?state=s"})
		case "/v2/plugin/auth/token":
			ok(map[string]any{"accessToken": "tok", "refreshToken": "r", "expiresIn": 3600, "refreshExpiresIn": 7200, "domain": "www.workbuddy.ai"})
		case "/v2/plugin/login/account":
			ok(map[string]any{"uid": "u1", "nickname": "Nick"})
		default:
			w.WriteHeader(404)
		}
	}))
	defer fake.Close()
	target, _ := url.Parse(fake.URL)
	var opened []string
	cfgPath := filepath.Join(dir, "config.json")
	cfg := config.DefaultConfig()
	h := admin.New(admin.Deps{
		Config:      func() *config.Config { return &cfg },
		Reload:      func(*config.Config) { t.Error("sign-in must not save config") },
		ConfigPath:  cfgPath,
		Credentials: &multiacct.Store{Path: filepath.Join(dir, "credentials.json")},
		Workbuddy: &workbuddy.Client{
			HTTP:         &http.Client{Transport: rewrite{target}},
			OpenBrowser:  func(u string) { opened = append(opened, u) },
			PollInterval: time.Millisecond, PollTimeout: 5 * time.Second,
		},
	})
	req := httptest.NewRequest("POST", "/oauth/workbuddy-ai/signin", bytes.NewReader([]byte(`{}`)))
	req.RemoteAddr = "127.0.0.1:1"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req.WithContext(context.Background()))
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if rr.Code != 200 || out["ok"] != true || out["user"] != "Nick" {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	if len(opened) != 1 {
		t.Fatalf("browser opened %v", opened)
	}
	if _, err := os.Stat(session); err != nil {
		t.Fatalf("session file not written: %v", err)
	}
	if _, err := os.Stat(cfgPath); err == nil {
		t.Fatal("config file was written by sign-in")
	}
}
