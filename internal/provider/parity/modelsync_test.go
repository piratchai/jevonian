package parity

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/modelsync"
)

func yes() *bool { b := true; return &b }
func no() *bool  { b := false; return &b }

// providerSyncsModels in src/config.ts: explicit syncModels wins; otherwise only
// the OAuth sources in MODEL_SYNC_DEFAULT_SOURCES sync by default. A keyless
// local, a subscription-billed API-key reseller or a `static` token does not.
func TestSyncableMatchesTS(t *testing.T) {
	cases := []struct {
		name string
		p    config.Provider
		want bool
	}{
		{"explicit true", config.Provider{SyncModels: yes()}, true},
		{"explicit false beats oauth", config.Provider{SyncModels: no(), Auth: config.AuthOAuth, OAuthSource: config.OAuthCodex}, false},
		{"codex", config.Provider{Auth: config.AuthOAuth, OAuthSource: config.OAuthCodex}, true},
		{"claude-code", config.Provider{Auth: config.AuthOAuth, OAuthSource: config.OAuthClaudeCode}, true},
		{"antigravity", config.Provider{Auth: config.AuthOAuth, OAuthSource: config.OAuthAntigravity}, true},
		{"devin", config.Provider{Auth: config.AuthOAuth, OAuthSource: config.OAuthDevin}, true},
		{"cursor", config.Provider{Auth: config.AuthOAuth, OAuthSource: config.OAuthCursor}, true},
		{"workbuddy", config.Provider{Auth: config.AuthOAuth, OAuthSource: config.OAuthWorkbuddyAI}, true},
		{"api key", config.Provider{Auth: config.AuthAPIKey}, false},
		{"chatgpt-web", config.Provider{Type: config.ProviderTypeChatGPTWeb}, true},
	}
	for _, c := range cases {
		if got := modelsync.Syncable(c.p); got != c.want {
			t.Errorf("%s: got %v, TS says %v", c.name, got, c.want)
		}
	}
}

// TS syncs by default only the OAuth sources in MODEL_SYNC_DEFAULT_SOURCES (plus chatgpt-web).
func TestSyncableDefaultRule(t *testing.T) {
	no := func(p config.Provider) {
		t.Helper()
		if modelsync.Syncable(p) {
			t.Fatalf("%+v must not sync by default", p)
		}
	}
	no(config.Provider{NoKey: true})
	no(config.Provider{Billing: config.BillingSubscription})
	no(config.Provider{Auth: config.AuthOAuth, OAuthSource: config.OAuthStatic})
	if !modelsync.Syncable(config.Provider{Type: config.ProviderTypeChatGPTWeb}) {
		t.Fatal("chatgpt-web must sync by default")
	}
	for _, src := range []config.OAuthSource{config.OAuthCodex, config.OAuthClaudeCode, config.OAuthAntigravity, config.OAuthDevin, config.OAuthCursor, config.OAuthWorkbuddyAI} {
		if !modelsync.Syncable(config.Provider{Auth: config.AuthOAuth, OAuthSource: src}) {
			t.Fatalf("%s must sync by default", src)
		}
	}
	yes, off := true, false
	if !modelsync.Syncable(config.Provider{NoKey: true, SyncModels: &yes}) {
		t.Fatal("explicit true must sync")
	}
	if modelsync.Syncable(config.Provider{Auth: config.AuthOAuth, OAuthSource: config.OAuthCodex, SyncModels: &off}) {
		t.Fatal("explicit false must not sync")
	}
}

func TestModelSyncRunIsAppendOnlyAndRecordsState(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{Providers: []config.Provider{
		{Name: "codex", Auth: config.AuthOAuth, OAuthSource: config.OAuthCodex, Models: []config.ModelEntry{{ID: "a", Wire: []config.UpstreamWire{"anthropic"}}}, ExcludeModels: []string{"banned"}},
		{Name: "deepseek", Auth: config.AuthAPIKey, SyncModels: no(), Models: []config.ModelEntry{{ID: "x"}}},
		{Name: "broken", Auth: config.AuthOAuth, OAuthSource: config.OAuthDevin, Models: []config.ModelEntry{{ID: "keep"}}},
	}}
	var saved *config.Config
	d := &modelsync.Deps{
		Config:     func() *config.Config { return cfg },
		SaveConfig: func(c *config.Config) error { saved = c; return nil },
		Reload:     func(c *config.Config) { cfg = c },
		Discover: func(p config.Provider) modelsync.Discovered {
			switch p.Name {
			case "codex":
				return modelsync.Discovered{Models: []string{"b", "a", "banned", "c"}}
			case "broken":
				return modelsync.Discovered{Error: "HTTP 500"}
			}
			return modelsync.Discovered{Models: []string{"zzz"}}
		},
		StatePath: filepath.Join(dir, "model-sync.json"),
		Now:       func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) },
	}
	res, err := d.Run(context.Background())
	if err != nil || res == nil {
		t.Fatalf("%v %v", res, err)
	}
	if !res.Changed || res.Added != 2 {
		t.Fatalf("result %+v", res)
	}
	var ids []string
	for _, m := range saved.Providers[0].Models {
		ids = append(ids, m.ID)
	}
	if !reflect.DeepEqual(ids, []string{"a", "b", "c"}) || saved.Providers[0].Models[0].Wire[0] != "anthropic" {
		t.Fatalf("models %v (pins kept, excluded id skipped, order preserved)", saved.Providers[0].Models)
	}
	if len(saved.Providers[1].Models) != 1 || len(saved.Providers[2].Models) != 1 {
		t.Fatal("opted-out or failing provider list changed")
	}
	byName := map[string]modelsync.ProviderSyncResult{}
	for _, p := range res.Providers {
		byName[p.Provider] = p
	}
	if byName["deepseek"].Skipped != "opted-out" || byName["broken"].Error != "HTTP 500" || len(byName["broken"].Added) != 0 {
		t.Fatalf("per-provider %+v", byName)
	}
	// State is persisted and a fresh pass is skipped inside the interval.
	if st := d.LoadState(); st == nil || st.Added != 2 {
		t.Fatalf("state %+v", st)
	}
	if !d.Fresh(60) {
		t.Fatal("pass inside interval should be fresh")
	}
	d.Now = func() time.Time { return time.Date(2026, 1, 1, 2, 0, 0, 0, time.UTC) }
	if d.Fresh(60) {
		t.Fatal("pass older than the interval should be stale")
	}
	// The minimum interval is 15 minutes.
	d.Now = func() time.Time { return time.Date(2026, 1, 1, 0, 10, 0, 0, time.UTC) }
	if !d.Fresh(1) {
		t.Fatal("intervals below 15m must clamp to 15m")
	}
}
