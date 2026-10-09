package upstream

import (
	"context"
	"encoding/binary"
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

// decodeField16 reads protobuf field 16 (the session UUID) out of a Connect
// frame's payload, so the test sees what the upstream would key its prompt
// cache on.
func decodeField16(payload []byte) string {
	for len(payload) > 0 {
		key, n := binary.Uvarint(payload)
		if n <= 0 {
			return ""
		}
		payload = payload[n:]
		field, wire := int(key>>3), int(key&7)
		switch wire {
		case 0:
			_, n = binary.Uvarint(payload)
			payload = payload[n:]
		case 2:
			length, ln := binary.Uvarint(payload)
			if ln <= 0 || uint64(len(payload)-ln) < length {
				return ""
			}
			body := payload[ln : ln+int(length)]
			payload = payload[ln+int(length):]
			if field == 16 {
				return string(body)
			}
		default:
			return ""
		}
	}
	return ""
}

// Devin's upstream session field (protobuf #16) must carry the routing
// session, not a fresh random id: the same conversation has to present the
// same value so Devin's prompt cache keeps hitting across turns.
// src/upstream.ts buildDevinChatRequest `sessionId: decision.session`.
func TestDevinSendsStableSessionForPromptCache(t *testing.T) {
	dir := t.TempDir()
	cred := filepath.Join(dir, "credentials.toml")
	if err := os.WriteFile(cred, []byte("windsurf_api_key = \"tok\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JEVONIAN_DEVIN_CREDENTIALS", cred)
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, decodeField16(raw[5:]))
		w.Header().Set("content-type", "application/connect+proto")
		_, _ = w.Write(devin.Concat(
			devin.EncodeFrame(devin.StringField(3, "pong"), 0),
			devin.EncodeFrame(devin.Concat(devin.VarintField(5, 2)), 0),
			devin.EncodeFrame([]byte("{}"), 2),
		))
	}))
	defer srv.Close()
	p := config.Provider{Name: "devin-subscription", Type: config.ProviderTypeDevin, BaseURL: srv.URL, Auth: config.AuthOAuth, OAuthSource: config.OAuthDevin, Billing: config.BillingSubscription, Models: []config.ModelEntry{{ID: "swe-2-max"}}}
	r := NewRunner(ForwardDeps{HTTP: srv.Client(), Auth: &oauth.Resolver{HTTP: srv.Client()}, Sleep: func(time.Duration) {}})
	req := AttemptRequest{
		ClientKind:   KindOpenAI,
		ClientBody:   wire.Body{"messages": []any{map[string]any{"role": "user", "content": "hi"}}},
		ExtraHeaders: http.Header{"X-Jevonian-Session": {"github|user_01HZ9PBPY9ZCF3S4NPYPV1P44X"}},
	}
	for i := 0; i < 2; i++ {
		at := r.Try(context.Background(), req, PlanEntry{Provider: p, Model: "swe-2-max"})
		if at.Outcome.Kind != OutcomeSuccess {
			t.Fatalf("attempt %d: outcome=%+v err=%v", i, at.Outcome, at.Err)
		}
		_, _ = io.ReadAll(at.Response.Body)
	}
	if len(bodies) != 2 {
		t.Fatalf("captured %d bodies, want 2", len(bodies))
	}
	if bodies[0] == "" {
		t.Fatal("no session id (protobuf field 16) in the Devin request")
	}
	if bodies[0] != bodies[1] {
		t.Fatalf("session id changed between turns: %q then %q", bodies[0], bodies[1])
	}
}
