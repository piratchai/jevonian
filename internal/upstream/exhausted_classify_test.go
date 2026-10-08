package upstream

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/oauth"
	"github.com/xinyao27/jevonian/internal/quota"
	"github.com/xinyao27/jevonian/internal/wire"
)

// An already-exhausted provider that returns a request-shaped 400 must stay a
// client error. Rewriting it to "quota" hid Unsupported parameter: temperature
// behind fake quota-failover labels.
func TestExhaustedProviderKeepsClientError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(400)
		io.WriteString(w, `{"detail":"Unsupported parameter: temperature"}`)
	}))
	t.Cleanup(srv.Close)

	tracker := quota.New(nil)
	p := config.Provider{Name: "chatgpt", Type: "responses", BaseURL: srv.URL + "/v1", Auth: "api-key", APIKey: "k"}
	tracker.MarkSpent(p.Name, quota.MarkSpentOptions{Label: "limit", ResetsAt: time.Now().Add(time.Hour)})

	runner := NewRunner(ForwardDeps{
		HTTP:  srv.Client(),
		Auth:  &oauth.Resolver{HTTP: srv.Client()},
		Quota: tracker,
		Sleep: func(time.Duration) {},
	})
	at := runner.Try(context.Background(), AttemptRequest{
		ClientKind: KindOpenAI,
		ClientBody: wire.Body{"messages": []any{map[string]any{"role": "user", "content": "hi"}}, "temperature": 0.2},
	}, PlanEntry{Provider: p, Model: "gpt-5"})
	if at.Response != nil {
		_, _ = io.Copy(io.Discard, at.Response.Body)
		at.Response.Body.Close()
	}
	if at.Outcome.Kind != OutcomeClientError {
		t.Fatalf("outcome=%v want client error; status=%d text=%q", at.Outcome, at.Status, at.Text)
	}
}

// Provider refusals on an already-exhausted account still upgrade to quota so
// the turn failovers without waiting on a measured spend token in the body.
func TestExhaustedProviderUpgradesProviderRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(401)
		io.WriteString(w, `{"error":{"message":"invalid_api_key"}}`)
	}))
	t.Cleanup(srv.Close)

	tracker := quota.New(nil)
	p := config.Provider{Name: "chatgpt", Type: "openai", BaseURL: srv.URL + "/v1", Auth: "api-key", APIKey: "k"}
	tracker.MarkSpent(p.Name, quota.MarkSpentOptions{Label: "limit", ResetsAt: time.Now().Add(time.Hour)})

	runner := NewRunner(ForwardDeps{
		HTTP:  srv.Client(),
		Auth:  &oauth.Resolver{HTTP: srv.Client()},
		Quota: tracker,
		Sleep: func(time.Duration) {},
	})
	at := runner.Try(context.Background(), AttemptRequest{
		ClientKind: KindOpenAI,
		ClientBody: wire.Body{"messages": []any{map[string]any{"role": "user", "content": "hi"}}},
	}, PlanEntry{Provider: p, Model: "gpt-4o"})
	if at.Response != nil {
		_, _ = io.Copy(io.Discard, at.Response.Body)
		at.Response.Body.Close()
	}
	if at.Outcome.Kind != OutcomeQuotaRefusal {
		t.Fatalf("outcome=%v want quota refusal; status=%d", at.Outcome, at.Status)
	}
}
