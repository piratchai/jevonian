package config

import (
	"encoding/json"
	"testing"
)

func TestStripJSONCCommentsAndTrailingCommas(t *testing.T) {
	src := []byte(`{
  // listen block
  "listen": { "host": "127.0.0.1", "port": 8787, },
  /* providers */
  "providers": [
    {
      "name": "deepseek",
      "baseUrl": "https://api.deepseek.com/v1",
    },
  ],
}`)
	cleaned, err := StripJSONC(src)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(cleaned, &raw); err != nil {
		t.Fatalf("strict JSON after strip: %v\n%s", err, cleaned)
	}
	if _, ok := raw["listen"]; !ok {
		t.Fatal("missing listen")
	}
}

func TestStripJSONCPreservesSlashesInStrings(t *testing.T) {
	src := []byte(`{"url": "https://example.com/path", "note": "not a // comment"}`)
	cleaned, err := StripJSONC(src)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(cleaned, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["url"] != "https://example.com/path" {
		t.Fatalf("url = %v", raw["url"])
	}
	if raw["note"] != "not a // comment" {
		t.Fatalf("note = %v", raw["note"])
	}
}
