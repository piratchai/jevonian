package upstream

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/oauth"
	"github.com/xinyao27/jevonian/internal/provider/devin"
	"github.com/xinyao27/jevonian/internal/wire"
)

// A 401 from Devin re-reads credentials.toml and replays the Connect request once
// with the renewed token (src/devin-routing.test.ts "retries a 401 ...").
func TestDevin401RetriesWithRenewedToken(t *testing.T) {
	dir := t.TempDir()
	cred := filepath.Join(dir, "credentials.toml")
	if err := os.WriteFile(cred, []byte("windsurf_api_key = \"old-token\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JEVONIAN_DEVIN_CREDENTIALS", cred)
	t.Setenv("JEVONIAN_DATA_DIR", dir)
	var calls atomic.Int32
	var auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, r.Header.Get("Authorization"))
		if calls.Add(1) == 1 {
			_ = os.WriteFile(cred, []byte("windsurf_api_key = \"renewed-token\"\n"), 0o600)
			w.WriteHeader(401)
			io.WriteString(w, `{"code":"unauthenticated","message":"invalid token"}`)
			return
		}
		w.Header().Set("content-type", "application/connect+proto")
		_, _ = w.Write(devin.Concat(
			devin.EncodeFrame(devin.StringField(3, "pong"), 0),
			devin.EncodeFrame(devin.Concat(devin.VarintField(5, 2)), 0),
			devin.EncodeFrame([]byte("{}"), 2),
		))
	}))
	defer srv.Close()
	p := config.Provider{Name: "devin-subscription", Type: config.ProviderTypeDevin, BaseURL: srv.URL, Auth: config.AuthOAuth, OAuthSource: config.OAuthDevin, Billing: config.BillingSubscription, Models: []config.ModelEntry{{ID: "swe-1-6-slow"}}}
	r := NewRunner(ForwardDeps{HTTP: srv.Client(), Auth: &oauth.Resolver{HTTP: srv.Client()}, Sleep: func(time.Duration) {}})
	at := r.Try(context.Background(), AttemptRequest{ClientKind: KindOpenAI, ClientBody: wire.Body{"messages": []any{map[string]any{"role": "user", "content": "hi"}}}}, PlanEntry{Provider: p, Model: "swe-1-6-slow"})
	if at.Outcome.Kind != OutcomeSuccess || calls.Load() != 2 {
		t.Fatalf("outcome=%+v err=%v text=%q calls=%d", at.Outcome, at.Err, at.Text, calls.Load())
	}
	raw, _ := io.ReadAll(at.Response.Body)
	if !strings.Contains(string(raw), "pong") {
		t.Fatalf("body %s", raw)
	}
	if !strings.Contains(auths[0], "Basic ") || auths[0] == auths[1] {
		t.Fatalf("token not renewed between attempts: %v", auths)
	}
}
