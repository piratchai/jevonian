package upstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/oauth"
	"github.com/xinyao27/jevonian/internal/quota"
	"github.com/xinyao27/jevonian/internal/wire"
)

func TestDevinFreeModelLimitStaysModelScoped(t *testing.T) {
	var calls atomic.Int32
	const response = `{"error":{"code":"unavailable","message":"Reached free model rate limit. Upgrade to Max for higher limits, or switch to a different model. Your limit will reset in 45 seconds ..."}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(response))
	}))
	defer srv.Close()
	provider := config.Provider{Name: "devin-subscription", Type: config.ProviderTypeDevin, BaseURL: srv.URL, Auth: config.AuthOAuth, OAuthSource: config.OAuthDevin, Billing: config.BillingSubscription, Models: []config.ModelEntry{{ID: "free-model"}, {ID: "sibling-model"}}}
	tracker := quota.New(nil)
	runner := NewRunner(ForwardDeps{HTTP: srv.Client(), Auth: &oauth.Resolver{HTTP: srv.Client()}, Quota: tracker})
	req := AttemptRequest{ClientKind: KindOpenAI, ClientBody: wire.Body{"messages": []any{map[string]any{"role": "user", "content": "hi"}}}}
	first := runner.Try(context.Background(), req, PlanEntry{Provider: provider, Model: "free-model"})
	second := runner.Try(context.Background(), req, PlanEntry{Provider: provider, Model: "free-model"})
	if first.Outcome.Kind != OutcomeQuotaRefusal || second.Outcome.Kind != OutcomeQuotaRefusal || calls.Load() != 2 {
		t.Fatalf("first=%+v second=%+v calls=%d", first.Outcome, second.Outcome, calls.Load())
	}
	if health := tracker.ModelHealth(provider, "free-model"); health.ResetsAt.IsZero() || time.Until(health.ResetsAt) < 40*time.Second || time.Until(health.ResetsAt) > 45*time.Second {
		t.Fatalf("model cooldown reset = %v", health.ResetsAt)
	}
	if got := tracker.ModelHealth(provider, "free-model"); got.Status != quota.StatusExhausted || got.Reason != "Reached free model rate limit. Upgrade to Max for higher limits, or switch to a different model. Your limit will reset in 45 seconds ..." {
		t.Fatalf("free model health = %+v", got)
	}
	if got := tracker.ModelHealth(provider, "sibling-model"); got.Status == quota.StatusExhausted {
		t.Fatalf("sibling model was benched: %+v", got)
	}
}
