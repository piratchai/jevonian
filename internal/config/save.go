package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/xinyao27/jevonian/internal/paths"
)

// JSONValue returns the disk representation, including compact model entries.
// ModelEntry deliberately has no JSON tags; never marshal it directly to disk.
func JSONValue(cfg *Config) (map[string]any, error) {
	data, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	providers := make([]any, 0, len(cfg.Providers))
	for _, p := range cfg.Providers {
		data, err := json.Marshal(p)
		if err != nil {
			return nil, err
		}
		var value map[string]any
		if err := json.Unmarshal(data, &value); err != nil {
			return nil, err
		}
		models := make([]any, 0, len(p.Models))
		for _, model := range p.Models {
			if len(model.Wire) == 0 {
				models = append(models, model.ID)
			} else {
				wires := make([]any, len(model.Wire))
				for i, w := range model.Wire {
					wires[i] = string(w)
				}
				models = append(models, map[string]any{"id": model.ID, "wire": wires})
			}
		}
		value["models"] = models
		providers = append(providers, value)
	}
	out["providers"] = providers
	return out, nil
}

// Save atomically persists cfg with mode 0600. An empty path uses ConfigPath.
// The caller is responsible for serializing read/modify/write transactions.
func Save(path string, cfg *Config) error {
	if cfg == nil {
		return fmt.Errorf("cannot save a nil config")
	}
	if path == "" {
		path = paths.ConfigPath()
	}
	value, err := JSONValue(cfg)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".config-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
