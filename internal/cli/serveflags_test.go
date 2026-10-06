package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/xinyao27/jevonian/internal/config"
)

func TestServeLANFlagsPersistButTunnelAndPortDoNot(t *testing.T) {
	cfg, err := config.ParseBytes([]byte(`{"listen":{"port":8787}}`))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	a, err := parseArgs([]string{"--lan", "--lan-host", "127.0.0.1", "--lan-port", "9002", "--tunnel", "--port", "9000"})
	if err != nil {
		t.Fatal(err)
	}
	if err := applyServeFlags(&cfg, a, path); err != nil {
		t.Fatal(err)
	}
	if !cfg.Lan.Enabled || !cfg.Tunnel.Enabled || cfg.Listen.Port != 9000 {
		t.Fatalf("one-shot config=%+v", cfg)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	disk, err := config.ParseBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	if !disk.Lan.Enabled || disk.Lan.Port == nil || *disk.Lan.Port != 9002 || disk.Listen.Port != 8787 || disk.Tunnel.Enabled {
		t.Fatalf("persistence leaked runtime flags: %s", data)
	}
}
func TestServeFlagsRejectConflictingAndInvalidInputs(t *testing.T) {
	for _, args := range [][]string{{"--lan", "--no-lan"}, {"--tunnel", "--no-tunnel"}, {"--lan-port", "70000"}, {"--port", "oops"}} {
		a, err := parseArgs(args)
		if err != nil {
			t.Fatal(err)
		}
		cfg := config.Config{}
		if err := applyServeFlags(&cfg, a, filepath.Join(t.TempDir(), "config.json")); err == nil {
			t.Fatalf("accepted invalid flags %v", args)
		}
	}
}
