// Package parity holds cross-package parity tests against TypeScript reference dumps.
package parity

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/xinyao27/jevonian/internal/cli"
	"github.com/xinyao27/jevonian/internal/server/admin"
)

// testdata/presets_ts.json is `JSON.stringify(PRESETS)` from src/providers.ts.
func TestPresetsMatchTS(t *testing.T) {
	raw, err := os.ReadFile("testdata/presets_ts.json")
	if err != nil {
		t.Fatal(err)
	}
	var ts []map[string]any
	if err := json.Unmarshal(raw, &ts); err != nil {
		t.Fatal(err)
	}
	// Round-trip Go views through JSON so value types match.
	b, _ := json.Marshal(cli.PresetViews())
	var gv []map[string]any
	if err := json.Unmarshal(b, &gv); err != nil {
		t.Fatal(err)
	}
	if len(ts) != len(gv) {
		t.Fatalf("preset count: ts=%d go=%d", len(ts), len(gv))
	}
	for i := range ts {
		if !reflect.DeepEqual(ts[i], gv[i]) {
			t.Errorf("preset[%d] %v differs\n ts=%v\n go=%v", i, ts[i]["id"], ts[i], gv[i])
		}
	}
}

// The dashboard's GET /api/state presets come from a second table in the admin
// package; it must also equal the TS table.
func TestAdminPresetsMatchTS(t *testing.T) {
	raw, err := os.ReadFile("testdata/presets_ts.json")
	if err != nil {
		t.Fatal(err)
	}
	var ts []map[string]any
	if err := json.Unmarshal(raw, &ts); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(admin.DefaultPresets())
	var gv []map[string]any
	if err := json.Unmarshal(b, &gv); err != nil {
		t.Fatal(err)
	}
	if len(ts) != len(gv) {
		t.Fatalf("preset count: ts=%d go=%d", len(ts), len(gv))
	}
	for i := range ts {
		if !reflect.DeepEqual(ts[i], gv[i]) {
			t.Errorf("admin preset[%d] %v differs\n ts=%v\n go=%v", i, ts[i]["id"], ts[i], gv[i])
		}
	}
}
