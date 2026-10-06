package cli

import (
	"bytes"
	"context"
	"crypto/tls"
	"github.com/xinyao27/jevonian/internal/quota"
	"github.com/xinyao27/jevonian/internal/tunnel"
	"github.com/xinyao27/jevonian/internal/update"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/xinyao27/jevonian/internal/config"
)

func TestQuotaNoProvidersAndHelpListsCommands(t *testing.T) {
	sandbox(t)
	code, out, _ := invoke("quota")
	if code != 0 || !strings.Contains(out, "No providers configured") {
		t.Fatalf("%d %s", code, out)
	}
	_, _, errText := invoke("bogus")
	for _, want := range []string{"update", "refresh", "quota [--refresh]"} {
		if !strings.Contains(errText, want) {
			t.Fatalf("usage lacks %q", want)
		}
	}
}

func TestDoctorBrainsAndBaselineFallback(t *testing.T) {
	sandbox(t)
	if code, _, err := invoke("init"); code != 0 {
		t.Fatal(err)
	}
	cfg, _, _ := config.Load()
	cfg.Routing.BaselineModel = ""
	cfg.Routing.Brains = []config.BrainConfig{{Channel: "typesafe", TimeoutMs: 5000, MinConfidence: 0.6}}
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	_, out, _ := invoke("doctor")
	for _, want := range []string{"baseline: deepseek-v4-pro", "brains:  1 configured (tried in order)", "1. ", "key="} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %s", want, out)
		}
	}
}

func TestServeBanner(t *testing.T) {
	cfg := config.DefaultConfig()
	var buf bytes.Buffer
	printServeBanner(&buf, cfg, "127.0.0.1:8787")
	for _, want := range []string{"jevonian listening on http://127.0.0.1:8787/", "providers: (none)", "routing: auto (models: jevonian/auto, jevonian/plan", "pricing: "} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("missing %q in %s", want, buf.String())
		}
	}
}

func TestPortInUseMessageNamesRecovery(t *testing.T) {
	msg := portInUseMessage(8787, nil)
	if !strings.Contains(msg, "port 8787 is already in use") || !strings.Contains(msg, "jevonian stop") {
		t.Fatal(msg)
	}
}

func TestModelsSyncReportsPerProvider(t *testing.T) {
	sandbox(t)
	if code, _, err := invoke("init"); code != 0 {
		t.Fatal(err)
	}
	_, out, _ := invoke("models", "--sync")
	if !strings.Contains(out, "deepseek: skipped (API/reseller; set syncModels: true to enable)") {
		t.Fatal(out)
	}
}

func TestReportColumnsMatchTS(t *testing.T) {
	sandbox(t)
	if code, _, err := invoke("init"); code != 0 {
		t.Fatal(err)
	}
	jsonl := `{"id":"r1","ts":"2026-10-01T10:00:00.000Z","session":"s","path":"/v1/messages","provider":"deepseek","model":"deepseek-v4-pro","stream":true,"status":200,"latencyMs":1,"promptTokens":2000,"completionTokens":400,"cacheReadTokens":0,"cacheWriteTokens":0,"costUsd":0.01,"pricingKnown":true,"billing":"api","phase":"plan","savedTokens":1500}` + "\n"
	if err := os.WriteFile(os.Getenv("JEVONIAN_LEDGER"), []byte(jsonl), 0o600); err != nil {
		t.Fatal(err)
	}
	_, out, _ := invoke("report")
	for _, want := range []string{
		"model                      reqs     prompt     output   cache read      saved       cost",
		"deepseek-v4-pro               1       2000        400            0      ~1500    $0.0100",
		"plan              1 reqs      $0.0100",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
}

func TestDoctorPricingHintAndStatusLoadedWord(t *testing.T) {
	sandbox(t)
	if code, _, err := invoke("init"); code != 0 {
		t.Fatal(err)
	}
	_, out, _ := invoke("doctor")
	if !strings.Contains(out, "bundled-fallback (12 models) — run `jevonian pricing --refresh`") {
		t.Fatal(out)
	}
	_, out, _ = invoke("status")
	if !strings.Contains(out, "loaded:  no") && !strings.Contains(out, "loaded:  yes") {
		t.Fatal(out)
	}
}

func TestAddSummaryMatchesTS(t *testing.T) {
	sandbox(t)
	code, out, errText := invoke("add", "deepseek", "--key", "sk-x", "--models", "a,b")
	if code != 0 {
		t.Fatal(errText)
	}
	for _, want := range []string{
		"Get a key at https://platform.deepseek.com/api_keys",
		"enabling 2 models: a, b",
		`Added provider "deepseek" (both) with 2 models`,
		"  auth: stored in ",
		"  billing: api",
		"  plan: ",
		"Next: jevonian serve",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
	_, out, _ = invoke("add", "claude-subscription", "--name", "cw", "--login-home", "/tmp/x", "--login-label", "work", "--models", "m")
	if !strings.Contains(out, "  auth: oauth (claude-code)") || !strings.Contains(out, "  login: work — /tmp/x") {
		t.Fatal(out)
	}
}

func TestForceHTTP1DropsH2FromALPN(t *testing.T) {
	tr := &http.Transport{TLSClientConfig: &tls.Config{NextProtos: []string{"h2", "http/1.1"}}}
	forceHTTP1(tr)
	if len(tr.TLSClientConfig.NextProtos) != 1 || tr.TLSClientConfig.NextProtos[0] != "http/1.1" {
		t.Fatalf("%v", tr.TLSClientConfig.NextProtos)
	}
	forceHTTP1(nil)
	bare := &http.Transport{}
	forceHTTP1(bare)
	if bare.TLSClientConfig == nil || bare.TLSClientConfig.NextProtos[0] != "http/1.1" {
		t.Fatal("nil TLS config not initialized")
	}
}

func TestPricingSourceLabelMatchesTS(t *testing.T) {
	if got := cliPricingSource(pricingSnapshot{Source: "https://models.dev/api.json"}); got != "models.dev" {
		t.Fatal(got)
	}
	if got := cliPricingSource(pricingSnapshot{Source: "bundled-fallback"}); got != "bundled-fallback" {
		t.Fatal(got)
	}
}

func TestAnnounceTunnelPrintsURLAndErrors(t *testing.T) {
	var buf bytes.Buffer
	announceTunnel(context.Background(), &buf, func() tunnel.State {
		return tunnel.State{Status: tunnel.StatusOn, URL: "https://x.example", Provider: "cloudflare"}
	}, time.Millisecond, time.Second)
	if buf.String() != "tunnel: https://x.example/v1 (cloudflare)\n" {
		t.Fatal(buf.String())
	}
	buf.Reset()
	announceTunnel(context.Background(), &buf, func() tunnel.State {
		return tunnel.State{Status: tunnel.StatusError, Error: "boom"}
	}, time.Millisecond, time.Second)
	if buf.String() != "tunnel: boom\n" {
		t.Fatal(buf.String())
	}
}

func TestSpentProvidersListsFullWindows(t *testing.T) {
	got := spentProviders([]quota.ProviderQuota{
		{Provider: "a", Windows: []quota.Window{{UsedPercent: 40}}},
		{Provider: "b", Windows: []quota.Window{{UsedPercent: 20}, {UsedPercent: 100}}},
	})
	if len(got) != 1 || got[0] != "b" {
		t.Fatal(got)
	}
}

func TestUpdateCommandRestartsBackgroundServiceWithFakes(t *testing.T) {
	pkg, entry, packagePath, asset := func() (string, string, string, string) {
		// local copy of npmPackageFixture to avoid export churn across packages
		root := t.TempDir()
		pkg := filepath.Join(root, "lib", "node_modules", "jevonian")
		entry := filepath.Join(pkg, "bin", "jevonian.js")
		packagePath := filepath.Join(pkg, "package.json")
		os.MkdirAll(filepath.Dir(entry), 0o755)
		os.WriteFile(entry, []byte("ok\n"), 0o644)
		os.WriteFile(packagePath, []byte(`{"version":"0.6.0"}`+"\n"), 0o644)
		asset, err := update.AssetName(runtime.GOOS, runtime.GOARCH)
		if err != nil {
			t.Fatal(err)
		}
		return pkg, entry, packagePath, asset
	}()
	installed := "0.5.4"
	m := update.New(update.Options{
		Current: "0.5.4",
		Installation: &update.Installation{
			Channel: update.NPM, Bin: "/fake/npm", Entry: entry, PackagePath: packagePath,
		},
		Detection:   update.DetectionOptions{NodeExecutable: "node", Env: map[string]string{}},
		FetchLatest: func(context.Context) (string, error) { return "0.6.0", nil },
		Run: func(_ context.Context, bin string, args ...string) (string, error) {
			if bin == "/fake/npm" {
				installed = "0.6.0"
				return "", nil
			}
			if len(args) > 0 && args[len(args)-1] == "--download-only" {
				os.MkdirAll(filepath.Join(pkg, "native"), 0o755)
				os.WriteFile(filepath.Join(pkg, "native", asset), []byte("bin"), 0o755)
			}
			return "", nil
		},
		ReadInstalledVersion: func() string { return installed },
	})
	var out, errOut bytes.Buffer
	restarted := false
	err := m.Command(context.Background(), &out, &errOut, update.CommandOptions{
		RestartBackground: func(context.Context) bool { restarted = true; return true },
	})
	if err != nil || !restarted || !strings.Contains(out.String(), "updated to 0.6.0 and restarted the background service.") {
		t.Fatalf("%v restarted=%v %s", err, restarted, out.String())
	}
}
