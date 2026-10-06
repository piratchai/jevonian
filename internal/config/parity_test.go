package config

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// loadSaved parses raw config bytes and returns the semantic JSON Save would write.
func loadSaved(t *testing.T, data []byte) map[string]any {
	t.Helper()
	cfg, err := ParseBytes(data)
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	value, err := JSONValue(&cfg)
	if err != nil {
		t.Fatalf("JSONValue: %v", err)
	}
	// Normalize through JSON text so numeric types match the golden decode.
	text, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(text, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return out
}

// The goldens (*.ts.json) are the output of src/config.ts parseConfig +
// serializeModelEntry for the matching input, generated with tsx.
func TestParseMatchesTypeScriptGolden(t *testing.T) {
	t.Setenv("JEVONIAN_PORT", "")
	for _, name := range []string{"parity_synthetic", "parity_legacy"} {
		t.Run(name, func(t *testing.T) {
			input, err := os.ReadFile("testdata/" + name + ".json")
			if err != nil {
				t.Fatal(err)
			}
			got := loadSaved(t, input)
			want := readJSON(t, "testdata/"+name+".ts.json")
			normalizeWirePins(got)
			normalizeWirePins(want)
			if !reflect.DeepEqual(got, want) {
				g, _ := json.MarshalIndent(got, "", " ")
				w, _ := json.MarshalIndent(want, "", " ")
				t.Fatalf("Go parse differs from TS golden\n--- go\n%s\n--- ts\n%s", g, w)
			}
		})
	}
}

// A load -> save round trip over a real-world config must not drop or alter any
// field (the config is the normalized fixed point of its own parse). Point
// JEVONIAN_PARITY_CONFIG_COPY at a READ-ONLY copy of a real config.json.
func TestRealConfigCopyRoundTrip(t *testing.T) {
	path := os.Getenv("JEVONIAN_PARITY_CONFIG_COPY")
	if path == "" {
		t.Skip("JEVONIAN_PARITY_CONFIG_COPY not set")
	}
	t.Setenv("JEVONIAN_PORT", "")
	input, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var original map[string]any
	if err := UnmarshalJSONC(input, &original); err != nil {
		t.Fatal(err)
	}
	saved := loadSaved(t, input)
	if !reflect.DeepEqual(saved, original) {
		g, _ := json.MarshalIndent(saved, "", " ")
		w, _ := json.MarshalIndent(original, "", " ")
		_ = os.WriteFile(os.TempDir()+"/parity-config-roundtrip.go.json", g, 0o600)
		_ = os.WriteFile(os.TempDir()+"/parity-config-roundtrip.orig.json", w, 0o600)
		t.Fatalf("round trip changed config; diff %s/parity-config-roundtrip.*.json", os.TempDir())
	}
	if again := loadSaved(t, mustMarshal(t, saved)); !reflect.DeepEqual(again, saved) {
		t.Fatal("second round trip not stable")
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRealConfigCopyMatchesTypeScript(t *testing.T) {
	path := os.Getenv("JEVONIAN_PARITY_CONFIG_COPY")
	golden := os.Getenv("JEVONIAN_PARITY_CONFIG_TS")
	if path == "" || golden == "" {
		t.Skip("JEVONIAN_PARITY_CONFIG_COPY / JEVONIAN_PARITY_CONFIG_TS not set")
	}
	t.Setenv("JEVONIAN_PORT", "")
	input, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := loadSaved(t, input), readJSON(t, golden); !reflect.DeepEqual(got, want) {
		t.Fatal("Go parse of real config differs from TS parseConfig output")
	}
}

// normalizeWirePins folds a scalar `"wire": "x"` into `["x"]`. TS keeps the
// scalar/array shape the file used; Go stores a slice and always writes an
// array. Both load identically in both runtimes, so the shape is cosmetic.
func normalizeWirePins(cfg map[string]any) {
	providers, _ := cfg["providers"].([]any)
	for _, p := range providers {
		models, _ := p.(map[string]any)["models"].([]any)
		for _, m := range models {
			entry, ok := m.(map[string]any)
			if !ok {
				continue
			}
			if w, ok := entry["wire"].(string); ok {
				entry["wire"] = []any{w}
			}
		}
	}
}

func TestJevonianPortEnvInvalidFallsBackToConfig(t *testing.T) {
	for _, env := range []string{"abc", "0", "-5", " "} {
		t.Setenv("JEVONIAN_PORT", env)
		cfg, err := ParseConfig(map[string]any{"listen": map[string]any{"port": float64(9123)}})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Listen.Port != 9123 {
			t.Fatalf("JEVONIAN_PORT=%q: port=%d, want listen.port 9123", env, cfg.Listen.Port)
		}
	}
	t.Setenv("JEVONIAN_PORT", "4242")
	cfg, _ := ParseConfig(map[string]any{"listen": map[string]any{"port": float64(9123)}})
	if cfg.Listen.Port != 4242 {
		t.Fatalf("valid env should win, got %d", cfg.Listen.Port)
	}
}
