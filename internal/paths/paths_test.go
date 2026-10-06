package paths

import (
	"path/filepath"
	"testing"
)

func TestConfigPathEnvOverride(t *testing.T) {
	t.Setenv("JEVONIAN_CONFIG", "/tmp/custom-config.json")
	if got := ConfigPath(); got != "/tmp/custom-config.json" {
		t.Fatalf("ConfigPath() = %q, want /tmp/custom-config.json", got)
	}
}

func TestConfigPathXDG(t *testing.T) {
	t.Setenv("JEVONIAN_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", "/xdg/config")
	want := filepath.Join("/xdg/config", "jevonian", "config.json")
	if got := ConfigPath(); got != want {
		t.Fatalf("ConfigPath() = %q, want %q", got, want)
	}
}

func TestDataDirEnvOverride(t *testing.T) {
	t.Setenv("JEVONIAN_DATA_DIR", "/tmp/jevonian-data")
	if got := DataDir(); got != "/tmp/jevonian-data" {
		t.Fatalf("DataDir() = %q, want /tmp/jevonian-data", got)
	}
}

func TestDataDirXDG(t *testing.T) {
	t.Setenv("JEVONIAN_DATA_DIR", "")
	t.Setenv("XDG_DATA_HOME", "/xdg/data")
	want := filepath.Join("/xdg/data", "jevonian")
	if got := DataDir(); got != want {
		t.Fatalf("DataDir() = %q, want %q", got, want)
	}
}

func TestLedgerAndServePathsFollowDataDir(t *testing.T) {
	t.Setenv("JEVONIAN_DATA_DIR", "/data")
	t.Setenv("JEVONIAN_LEDGER", "")
	t.Setenv("JEVONIAN_SERVE_LOG", "")
	if got := LedgerPath(); got != filepath.Join("/data", "ledger.jsonl") {
		t.Fatalf("LedgerPath() = %q", got)
	}
	if got := ServeLogPath(); got != filepath.Join("/data", "serve.log") {
		t.Fatalf("ServeLogPath() = %q", got)
	}
}

func TestTunnelStateBesideConfig(t *testing.T) {
	t.Setenv("JEVONIAN_TUNNEL_STATE", "")
	t.Setenv("JEVONIAN_CONFIG", "/cfg/jevonian/config.json")
	want := filepath.Join("/cfg/jevonian", "tunnel-state.json")
	if got := TunnelStatePath(); got != want {
		t.Fatalf("TunnelStatePath() = %q, want %q", got, want)
	}
}
