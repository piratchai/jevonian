package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The release build stamps Version with -ldflags -X from package.json. The
// in-source default must match too, so a plain `go build` (dev, smoke, a
// forgotten flag) never reports a stale version and loops the update check.
func TestVersionMatchesPackageJSON(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	var pkg struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &pkg); err != nil {
		t.Fatal(err)
	}
	if Version != pkg.Version {
		t.Fatalf("cli.Version = %q, package.json = %q; bump internal/cli/cli.go with the release", Version, pkg.Version)
	}
}
