package upstream

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/ledger"
	"github.com/xinyao27/jevonian/internal/oauth"
	"github.com/xinyao27/jevonian/internal/quota"
	"github.com/xinyao27/jevonian/internal/wire"
)

// TestPrepareAppliesPromptPolicyToEveryWire pins the src/prepare.ts call site:
// every adapter's Prepare runs wire.RewritePromptBodies last, so a configured
// rewrite reaches Chat, Anthropic and Responses hosts alike.
func TestPrepareAppliesPromptPolicyToEveryWire(t *testing.T) {
	policy := config.PromptPolicyConfig{
		Builtins: true,
		Rewrites: []config.PromptRewriteRule{{Match: "PLACEHOLDER", Replace: "redacted"}},
	}
	const rival = "You operate in Cursor. Some PLACEHOLDER detail."
	cases := []struct {
		name     string
		provider config.Provider
		kind     ClientKind
		body     wire.Body
		read     func(t *testing.T, body wire.Body) string
	}{
		{
			name:     "chat system",
			provider: config.Provider{Name: "p", Type: config.ProviderTypeOpenAI, Auth: config.AuthAPIKey, APIKey: "k"},
			kind:     KindOpenAI,
			body: wire.Body{"messages": []any{
				map[string]any{"role": "system", "content": rival},
				map[string]any{"role": "user", "content": "PLACEHOLDER"},
			}},
			read: func(t *testing.T, body wire.Body) string {
				msgs := body["messages"].([]any)
				return msgs[0].(map[string]any)["content"].(string)
			},
		},
		{
			name:     "anthropic system",
			provider: config.Provider{Name: "p", Type: config.ProviderTypeAnthropic, Auth: config.AuthAPIKey, APIKey: "k"},
			kind:     KindAnthropic,
			body: wire.Body{"max_tokens": 16, "system": rival,
				"messages": []any{map[string]any{"role": "user", "content": "PLACEHOLDER"}}},
			read: func(t *testing.T, body wire.Body) string { return body["system"].(string) },
		},
		{
			name:     "responses instructions",
			provider: config.Provider{Name: "p", Type: config.ProviderTypeResponses, Auth: config.AuthAPIKey, APIKey: "k"},
			kind:     KindResponses,
			body:     wire.Body{"instructions": rival, "input": "PLACEHOLDER"},
			read:     func(t *testing.T, body wire.Body) string { return body["instructions"].(string) },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := PlanUpstreamWire(tc.provider, tc.kind, "m")
			if err != nil {
				t.Fatal(err)
			}
			adapter, err := AdapterFor(tc.provider, tc.kind, plan)
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := adapter.Prepare(PrepInput{
				PromptPolicy: policy, Provider: tc.provider, Model: "m",
				ClientKind: tc.kind, ClientBody: tc.body, UpstreamWire: plan.Wire, Bridge: plan.Bridge,
			})
			if err != nil {
				t.Fatal(err)
			}
			got := tc.read(t, prepared)
			if strings.Contains(got, "You operate in Cursor") || strings.Contains(got, "PLACEHOLDER") {
				t.Fatalf("prompt policy not applied: %q", got)
			}
			if !strings.Contains(got, "You work inside the user's code editor") || !strings.Contains(got, "redacted") {
				t.Fatalf("unexpected rewrite: %q", got)
			}
		})
	}
}

// TestStreamIdleGuardEndsWedgedStream proves the mid-stream idle clock is
// enforced: a host that sends its first byte then stops is closed with an idle
// timeout instead of hanging for the total budget.
func TestStreamIdleGuardEndsWedgedStream(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer ts.Close()

	p := config.Provider{Name: "p", Type: config.ProviderTypeOpenAI, BaseURL: ts.URL + "/v1", Auth: config.AuthAPIKey, APIKey: "k"}
	r := NewRunner(ForwardDeps{
		HTTP: ts.Client(),
		Timeouts: Timeouts{
			ConnectMS: 5_000, HeadersMS: 5_000, TotalMS: 30_000, FirstByteMS: 5_000, IdleMS: 60,
		},
		Auth: &oauth.Resolver{HTTP: ts.Client()},
	})
	at := r.Try(context.Background(), AttemptRequest{
		ClientKind: KindOpenAI, Stream: true,
		ClientBody: wire.Body{"messages": []any{map[string]any{"role": "user", "content": "hi"}}},
	}, PlanEntry{Provider: p, Model: "m"})
	if at.Outcome.Kind != OutcomeSuccess || at.Response == nil {
		t.Fatalf("outcome = %+v err=%v", at.Outcome, at.Err)
	}
	defer at.Response.Body.Close()

	// Drain the buffered frame, then the next read must report the idle cutoff.
	buf := make([]byte, 256)
	var err error
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err = at.Response.Body.Read(buf); err != nil {
			break
		}
	}
	if err == nil {
		t.Fatal("wedged stream was not cut off")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "idle") {
		t.Fatalf("err = %v", err)
	}
}

// TestHeaderExhaustionBecomesQuotaFailover covers the minor upstream.ts rule:
// after capturing rate-limit headers, an exhausted provider health state is a
// quota refusal even when the 429 body carries no spend token.
func TestHeaderExhaustionBecomesQuotaFailover(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("anthropic-ratelimit-unified-5h-utilization", "1")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"slow down"}}`)
	}))
	defer ts.Close()

	db, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tracker := quota.New(db)

	p := config.Provider{Name: "p", Type: config.ProviderTypeAnthropic, BaseURL: ts.URL + "/v1", Auth: config.AuthAPIKey, APIKey: "k"}
	r := NewRunner(ForwardDeps{HTTP: ts.Client(), Quota: tracker, Auth: &oauth.Resolver{HTTP: ts.Client()}})
	at := r.Try(context.Background(), AttemptRequest{
		ClientKind: KindAnthropic,
		ClientBody: wire.Body{"max_tokens": 8, "messages": []any{map[string]any{"role": "user", "content": "hi"}}},
	}, PlanEntry{Provider: p, Model: "m"})
	if at.Response != nil {
		_, _ = io.Copy(io.Discard, at.Response.Body)
		_ = at.Response.Body.Close()
	}
	if at.Outcome.Kind != OutcomeQuotaRefusal {
		t.Fatalf("outcome = %+v status=%d text=%q", at.Outcome, at.Status, at.Text)
	}
}
