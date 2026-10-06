package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestJSONValueAndSavePreserveProviderWireAndExclusions(t *testing.T) {
	cfg, err := ParseBytes([]byte(`{"providers":[{"name":"p","type":"both","baseUrl":"https://example.com/v1","models":[{"id":"a","wire":["anthropic","openai"]},"b"],"excludeModels":["c"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	value, err := JSONValue(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	copy, err := ParseConfig(value)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(copy.Providers, cfg.Providers) {
		t.Fatalf("in-memory round trip lost fields: %+v", copy.Providers)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Save(path, &copy); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := ParseBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored.Providers, cfg.Providers) {
		t.Fatalf("disk round trip lost fields: %s", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("config permissions=%o", info.Mode().Perm())
	}
}
