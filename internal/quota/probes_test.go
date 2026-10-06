package quota

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/oauth"
	"github.com/xinyao27/jevonian/internal/provider/devin"
)

type rewriteTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (t rewriteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	copy := r.Clone(r.Context())
	u := *r.URL
	u.Scheme = t.target.Scheme
	u.Host = t.target.Host
	copy.URL = &u
	return t.base.RoundTrip(copy)
}
func testHTTP(server *httptest.Server) *http.Client {
	u, _ := url.Parse(server.URL)
	return &http.Client{Transport: rewriteTransport{u, server.Client().Transport}}
}
func testOptions(client *http.Client) LiveOptions {
	return LiveOptions{HTTP: client, ResolveAuth: func(context.Context, config.Provider) (oauth.AuthResolution, error) {
		return oauth.AuthResolution{Headers: map[string]string{"authorization": "Bearer test-token", "x-user-id": "test-uid", "x-domain": "www.workbuddy.ai"}, Token: "test-token", Project: "test-project"}, nil
	}, AuthSource: func(config.Provider) string { return "inline" }}
}

func TestRealLiveProviderEndpoints(t *testing.T) {
	cases := []struct {
		name       string
		p          config.Provider
		path, body string
		used       []float64
		plan, note string
		balance    *float64
	}{
		{name: "codex", p: config.Provider{Auth: config.AuthOAuth, OAuthSource: config.OAuthCodex, BaseURL: "https://chatgpt.com/backend-api/codex"}, path: "/backend-api/wham/usage", body: `{"account":{"account_id":"account-1"},"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000,"reset_at":1893456000},"secondary_window":{"used_percent":75,"window_minutes":10080}},"rate_limit_reset_credits":{"available_count":2},"credits":{"balance":"4.2"},"plan_type":"plus"}`, used: []float64{1, 75}, plan: "plus", note: "credits 4.2"},

		{name: "cursor", p: config.Provider{Type: config.ProviderTypeCursor, Auth: config.AuthOAuth, OAuthSource: config.OAuthCursor, BaseURL: "https://api2.cursor.sh"}, path: "/aiserver.v1.DashboardService/GetCurrentPeriodUsage", body: `{"billingCycleEnd":"1893456000000","planUsage":{"autoPercentUsed":21,"apiPercentUsed":9,"totalPercentUsed":30}}`, used: []float64{21, 9, 30}},
		{name: "opencode", p: config.Provider{BaseURL: "https://opencode.ai/zen/go/v1"}, path: "/zen/go/v1/usage", body: `{"usage":{"rolling":{"percent":1,"resetsAt":1893456000},"weekly":{"percent":19},"monthly":{"percent":33}}}`, used: []float64{1, 19, 33}},
		{name: "commandcode", p: config.Provider{BaseURL: "https://api.commandcode.ai/v1"}, path: "/alpha/billing/credits", body: `{"windowLimits":{"fiveHour":{"used":2,"cap":20,"resetAt":1893456000000},"weekly":{"used":5,"cap":10,"exceeded":true}},"credits":{"monthlyCredits":60}}`, used: []float64{10, 50, 40}, plan: "PRO", note: "$60.00 credits left"},
		{name: "antigravity", p: config.Provider{Type: config.ProviderTypeGemini, BaseURL: "https://cloud.google.com/v1internal"}, path: "/v1internal:fetchAvailableModels", body: `{"models":{"a":{"modelProvider":"MODEL_PROVIDER_GOOGLE","quotaInfo":{"remainingFraction":0.8}},"b":{"modelProvider":"MODEL_PROVIDER_ANTHROPIC","quotaInfo":{"remainingFraction":0.1}},"c":{"modelProvider":"MODEL_PROVIDER_GOOGLE","quotaInfo":{"remainingFraction":0.7,"resetTime":"2030-01-01T00:00:00Z"}}}}`, used: []float64{30, 90}, note: "2 quota counters"},
		{name: "devin", p: config.Provider{Type: config.ProviderTypeDevin, BaseURL: "https://server.codeium.com"}, path: "/exa.seat_management_pb.SeatManagementService/GetUserStatus", used: []float64{45, 80}, plan: "Free"},
		{name: "workbuddy", p: config.Provider{Auth: config.AuthOAuth, OAuthSource: config.OAuthWorkbuddyAI, BaseURL: "https://www.workbuddy.ai/v2"}, path: "/billing/meter/get-user-resource-summary", body: `{"code":0,"data":{"Packages":[{"CycleTotalCapacity":"1000","CycleUsedCapacity":"250"},{"CycleTotalCapacity":1000,"CycleUsedCapacity":750}],"IsPaidUser":true}}`, used: []float64{50}, plan: "Pro", note: "1.0k / 2.0k"},
		{name: "deepseek", p: config.Provider{BaseURL: "https://api.deepseek.com/v1"}, path: "/user/balance", body: `{"balance_infos":[{"total_balance":"12.1234567","currency":"CNY"}]}`, balance: floatPtr(12.123457)},
		{name: "openrouter", p: config.Provider{BaseURL: "https://openrouter.ai/api/v1"}, path: "/api/v1/credits", body: `{"data":{"total_credits":20,"total_usage":25}}`, balance: floatPtr(0)},
		{name: "moonshot", p: config.Provider{BaseURL: "https://api.moonshot.ai/v1"}, path: "/v1/users/me/balance", body: `{"data":{"available_balance":7.5}}`, balance: floatPtr(7.5)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				switch r.URL.Path {
				case "/backend-api/wham/rate-limit-reset-credits":
					if r.Header.Get("Authorization") != "Bearer test-token" || r.Header.Get("chatgpt-account-id") != "account-1" {
						t.Error("missing token or account id on reset-credits request")
					}
					fmt.Fprint(w, `{"available_count":2,"credits":[{"id":"r1","status":"available","expires_at":"2030-01-01T00:00:00Z"},{"id":"r2","status":"available","expires_at":"2030-02-01T00:00:00Z"}]}`)
					return
				case "/alpha/usage/summary":
					fmt.Fprint(w, `{"totalMonthlyCredits":40}`)
					return
				case "/alpha/billing/subscriptions":
					fmt.Fprint(w, `{"data":{"planId":"individual-pro","currentPeriodEnd":"2030-01-01T00:00:00Z"}}`)
					return
				}
				if r.URL.Path != c.path {
					if c.name == "codex" && r.URL.Path == "/backend-api/wham/usage" {
						if r.Header.Get("chatgpt-account-id") != "account-1" {
							t.Error("Codex usage request missing account id")
						}
					} else {
						t.Errorf("path = %s want %s", r.URL.Path, c.path)
					}
				}
				if c.name == "devin" {
					if r.Header.Get("Authorization") != "Basic test-token-test-token" || r.Method != "POST" {
						t.Error("missing Devin auth/proto request")
					}
					raw, _ := io.ReadAll(r.Body)
					if !strings.Contains(string(raw), "test-token") {
						t.Error("no token in metadata")
					}
					plan := devin.Concat(devin.BytesField(1, devin.StringField(2, "Free")), devin.VarintField(14, 55), devin.VarintField(15, 20), devin.VarintField(17, 1893456000))
					w.Write(devin.BytesField(1, devin.BytesField(13, plan)))
					return
				}
				if r.Header.Get("Authorization") != "Bearer test-token" {
					t.Error("missing bearer")
				}
				if c.name == "workbuddy" && (r.Header.Get("x-user-id") != "test-uid" || r.Method != "POST") {
					t.Error("WorkBuddy auth/method")
				}
				if c.name == "antigravity" {
					var body map[string]any
					json.NewDecoder(r.Body).Decode(&body)
					if body["project"] != "test-project" || r.Method != "POST" {
						t.Errorf("body = %+v", body)
					}
				}
				fmt.Fprint(w, c.body)
			}))
			defer server.Close()
			tracker := New(nil)
			options := testOptions(testHTTP(server))
			service := NewService(tracker, options)
			p := c.p
			p.Name = c.name
			rows, err := service.ProviderQuotas(context.Background(), &config.Config{Providers: []config.Provider{p}}, true)
			if err != nil {
				t.Fatal(err)
			}
			q := rows[0]
			if q.Error != "" || q.Source != "live" {
				t.Fatalf("quota = %+v", q)
			}
			if q.Plan != c.plan || q.Note != c.note {
				t.Fatalf("plan/note = %q %q", q.Plan, q.Note)
			}
			if len(q.Windows) != len(c.used) {
				t.Fatalf("windows = %+v", q.Windows)
			}
			for i, pct := range c.used {
				if diff := q.Windows[i].UsedPercent - pct; diff > 1e-8 || diff < -1e-8 {
					t.Errorf("window %d used = %v want %v", i, q.Windows[i].UsedPercent, pct)
				}
			}
			if c.balance != nil && (q.Balance == nil || q.Balance.Amount != *c.balance) {
				t.Fatalf("balance = %+v", q.Balance)
			}
			if c.name == "codex" && (q.Resets == nil || q.Resets.Count != 2 || len(q.Resets.Each) != 2 || q.Resets.Until != "2030-01-01T00:00:00.000Z") {
				t.Fatalf("reset credits = %+v", q.Resets)
			}
			if c.name == "cursor" && (q.Windows[0].ResetsAt != "2030-01-01T00:00:00.000Z" || q.Windows[0].Label != "Cursor Models") {
				t.Fatalf("Cursor usage windows = %+v", q.Windows)
			}
			if c.name == "openrouter" && tracker.ProviderHealth(p, HealthOptions{}).Status != StatusExhausted {
				t.Fatal("zero credit not exhausted")
			}
			if c.name == "deepseek" && tracker.ProviderHealth(p, HealthOptions{}).Status != StatusOK {
				t.Fatal("positive credit not healthy")
			}
			_, err = service.ProviderQuotas(context.Background(), &config.Config{Providers: []config.Provider{p}}, true)
			if err != nil {
				t.Fatal(err)
			}
			if calls.Load() < 2 {
				t.Fatal("explicit refresh used fresh cache")
			}
		})
	}
}
func floatPtr(v float64) *float64 { return &v }

func TestOAuthRefreshAndAccountHeaders(t *testing.T) {
	dir := t.TempDir()
	credential := filepath.Join(dir, "auth.json")
	os.WriteFile(credential, []byte(`{"tokens":{"access_token":"","refresh_token":"fake-refresh","account_id":"account-2"}}`), 0600)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["refresh_token"] != "fake-refresh" {
				t.Error("wrong refresh token")
			}
			fmt.Fprint(w, `{"access_token":"renewed-test-token","refresh_token":"renewed-refresh"}`)
		case "/backend-api/wham/usage":
			if r.Header.Get("chatgpt-account-id") != "account-2" || r.Header.Get("Authorization") != "Bearer renewed-test-token" {
				t.Error("account/token missing")
			}
			fmt.Fprint(w, `{"rate_limit":{"primary_window":{"used_percent":2}}}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	client := testHTTP(server)
	resolver := &oauth.Resolver{HTTP: client}
	p := config.Provider{Name: "second", Auth: config.AuthOAuth, OAuthSource: config.OAuthCodex, BaseURL: "https://chatgpt.com/backend-api/codex", Login: &config.ProviderLogin{CredentialsPath: credential}}
	s := NewService(New(nil), LiveOptions{HTTP: client, OAuth: resolver})
	rows, err := s.ProviderQuotas(context.Background(), &config.Config{Providers: []config.Provider{p}}, true)
	if err != nil || rows[0].Error != "" || rows[0].Auth != "oauth:codex" {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	raw, _ := os.ReadFile(credential)
	if !strings.Contains(string(raw), "renewed-test-token") {
		t.Fatal("refresh was not written back")
	}
}

func TestSnapshotRefreshBoundedAndCancelled(t *testing.T) {
	started := make(chan struct{}, 20)
	release := make(chan struct{})
	var active, maxActive, calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		n := active.Add(1)
		defer active.Add(-1)
		for {
			old := maxActive.Load()
			if n <= old || maxActive.CompareAndSwap(old, n) {
				break
			}
		}
		started <- struct{}{}
		select {
		case <-release:
			fmt.Fprint(w, `{"five_hour":{"utilization":25}}`)
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tracker := New(nil)
	tracker.CaptureRateLimitHeaders("p0", http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": []string{"0.2"}})
	opts := testOptions(server.Client())
	opts.BackgroundContext = ctx
	opts.Concurrency = 2
	s := NewService(tracker, opts)
	cfg := &config.Config{}
	for i := 0; i < 6; i++ {
		cfg.Providers = append(cfg.Providers, config.Provider{Name: fmt.Sprintf("p%d", i), BaseURL: server.URL, Auth: config.AuthOAuth, OAuthSource: config.OAuthClaudeCode})
	}
	rows, err := s.ProviderQuotas(context.Background(), cfg, false)
	if err != nil || rows[0].Source != "headers" || rows[0].Windows[0].UsedPercent != 20 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	<-started
	<-started
	// Snapshot reads and tracker marks must not wait for the network.
	done := make(chan struct{})
	go func() {
		tracker.MarkProviderSpent("p0", MarkSpentOptions{Model: "Fable"})
		s.ProviderQuotas(context.Background(), cfg, false)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("network held a lock")
	}
	if calls.Load() != 2 {
		t.Fatal("duplicate probes or unbounded concurrency")
	}
	cancel()
	close(release)
	// Explicit waiting on the same calls drains all context-cancelled jobs.
	_, err = s.ProviderQuotas(context.Background(), cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	if maxActive.Load() > 2 {
		t.Fatalf("max concurrency = %d", maxActive.Load())
	}
	if !tracker.ProviderModelExhausted(cfg.Providers[0], "Fable", HealthOptions{}) {
		t.Fatal("cancelled probe cleared model refusal")
	}
}

func TestProbeTimeoutFallbackAndLateHeaderWins(t *testing.T) {
	entered := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { entered <- struct{}{}; <-r.Context().Done() }))
	defer server.Close()
	tracker := New(nil)
	tracker.MarkProviderSpent("p", MarkSpentOptions{ResetsAt: time.Now().Add(time.Hour), Model: "Fable"})
	opts := testOptions(server.Client())
	opts.Timeout = 30 * time.Millisecond
	s := NewService(tracker, opts)
	p := config.Provider{Name: "p", BaseURL: server.URL, Auth: config.AuthOAuth, OAuthSource: config.OAuthClaudeCode}
	rows, err := s.ProviderQuotas(context.Background(), &config.Config{Providers: []config.Provider{p}}, true)
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Source != "headers" || !strings.Contains(rows[0].Error, "timed out") || len(rows[0].Windows) != 1 {
		t.Fatalf("fallback = %+v", rows[0])
	}
	if !tracker.ProviderModelExhausted(p, "Fable", HealthOptions{}) {
		t.Fatal("failure cleared scoped reset")
	}
}

func TestSuccessfulProbePreservesScopedMarksAndNewerRefusals(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		fmt.Fprint(w, `{"five_hour":{"utilization":10}}`)
	}))
	defer server.Close()
	tracker := New(nil)
	tracker.MarkProviderSpent("p", MarkSpentOptions{Model: "Fable", ResetsAt: time.Now().Add(time.Hour)})
	s := NewService(tracker, testOptions(server.Client()))
	p := config.Provider{Name: "p", BaseURL: server.URL, Auth: config.AuthOAuth, OAuthSource: config.OAuthClaudeCode}
	cfg := &config.Config{Providers: []config.Provider{p}}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := s.ProviderQuotas(context.Background(), cfg, true)
		if err != nil {
			t.Error(err)
		}
	}()
	<-entered
	tracker.MarkProviderSpent("p", MarkSpentOptions{ResetsAt: time.Now().Add(time.Hour)})
	close(release)
	wg.Wait()
	if tracker.ProviderHealth(p, HealthOptions{}).Status != StatusExhausted {
		t.Fatal("late probe erased newer refusal")
	}
	rows, err := s.ProviderQuotas(context.Background(), cfg, true)
	if err != nil || rows[0].Source != "live" {
		t.Fatalf("successful refresh = %+v, %v", rows, err)
	}
	if tracker.ProviderHealth(p, HealthOptions{}).Status != StatusOK {
		t.Fatal("successful probe did not clear account refusal")
	}
	tracker.MarkProviderSpent("p", MarkSpentOptions{Model: "Fable", ResetsAt: time.Now().Add(time.Hour)})
	rows, err = s.ProviderQuotas(context.Background(), cfg, true)
	if err != nil || len(rows[0].Windows) != 2 {
		t.Fatalf("scoped merge = %+v %v", rows, err)
	}
	if !tracker.ProviderModelExhausted(p, "Fable", HealthOptions{}) {
		t.Fatal("successful account probe cleared model rejection")
	}
}
