package quota

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/oauth"
)

func TestLiveClaudePayloadAndScopedHealth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/oauth/usage" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("auth = %s", r.Header.Get("Authorization"))
		}
		fmt.Fprint(w, `{"five_hour":{"utilization":1},"seven_day":{"utilization":32},"limits":[{"scope":{"model":{"id":"fable","display_name":"Fable"}},"percent":100}]}`)
	}))
	defer server.Close()
	tracker := New(nil)
	service := NewService(tracker, LiveOptions{HTTP: server.Client(), ResolveAuth: func(context.Context, config.Provider) (oauth.AuthResolution, error) {
		return oauth.AuthResolution{Headers: map[string]string{"authorization": "Bearer test-token"}}, nil
	}, AuthSource: func(config.Provider) string { return "oauth:claude-code" }})
	provider := config.Provider{Name: "claude", BaseURL: server.URL + "/v1", Auth: config.AuthOAuth, OAuthSource: config.OAuthClaudeCode, Billing: config.BillingSubscription}
	rows, err := service.Quotas(context.Background(), &config.Config{Providers: []config.Provider{provider}}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0]["source"] != "live" || rows[0]["auth"] != "oauth:claude-code" {
		t.Fatalf("rows = %#v", rows)
	}
	windows := tracker.HeaderWindows(provider.Name)
	if len(windows) != 3 || windows[0].UsedPercent != 1 || windows[2].Model != "Fable" {
		t.Fatalf("windows = %+v", windows)
	}
	if tracker.ProviderHealth(provider, HealthOptions{}).Status != StatusOK {
		t.Fatal("scoped quota exhausted account")
	}
	if !tracker.ProviderModelExhausted(provider, "Fable", HealthOptions{}) {
		t.Fatal("scoped model not exhausted")
	}
}
