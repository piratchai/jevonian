package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/paths"
)

// configDocument avoids ModelEntry's untagged Go fields and preserves TS wire shape.
func configDocument(cfg config.Config) (map[string]any, error) {
	return config.JSONValue(&cfg)
}
func writeJSONAtomic(path string, value any, mode os.FileMode) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".jevonian-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
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
	return os.Rename(tmp, path)
}
func configuredTiers(routings []config.RoutingEntry) config.RoutingTiers {
	tiers := config.RoutingTiers{Plan: []string{}, Execute: []string{}, Utility: []string{}, Chat: []string{}}
	for _, entry := range routings {
		models := append([]string{}, entry.Models...)
		switch entry.ID {
		case "plan":
			tiers.Plan = models
		case "execute":
			tiers.Execute = models
		case "utility":
			tiers.Utility = models
		case "chat":
			tiers.Chat = models
		}
	}
	return tiers
}
func saveConfig(cfg config.Config) error {
	doc, err := configDocument(cfg)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(doc)
	if _, err := config.ParseBytes(b); err != nil {
		return fmt.Errorf("refusing invalid config: %w", err)
	}
	return config.Save(paths.ConfigPath(), &cfg)
}
