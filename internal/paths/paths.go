// Package paths resolves Jevonian data, config, and ledger file locations.
// Mirrors src/paths.ts: env overrides win, then XDG (or home) defaults.
package paths

import (
	"os"
	"path/filepath"
)

func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h
	}
	return ""
}

// ConfigPath is the main config.json location.
// JEVONIAN_CONFIG overrides; else $XDG_CONFIG_HOME/jevonian/config.json
// (or ~/.config/jevonian/config.json).
func ConfigPath() string {
	if v := os.Getenv("JEVONIAN_CONFIG"); v != "" {
		return v
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		base = filepath.Join(homeDir(), ".config")
	}
	return filepath.Join(base, "jevonian", "config.json")
}

// DataDir is the shared data directory (ledger, logs, and other runtime data).
// JEVONIAN_DATA_DIR overrides; else $XDG_DATA_HOME/jevonian
// (or ~/.local/share/jevonian).
func DataDir() string {
	if v := os.Getenv("JEVONIAN_DATA_DIR"); v != "" {
		return v
	}
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		base = filepath.Join(homeDir(), ".local", "share")
	}
	return filepath.Join(base, "jevonian")
}

// TunnelStatePath is the tunnel state JSON beside the config file.
func TunnelStatePath() string {
	if v := os.Getenv("JEVONIAN_TUNNEL_STATE"); v != "" {
		return v
	}
	return filepath.Join(filepath.Dir(ConfigPath()), "tunnel-state.json")
}

// TunnelLogPath is the tunnel log under the data directory.
func TunnelLogPath() string {
	if v := os.Getenv("JEVONIAN_TUNNEL_LOG"); v != "" {
		return v
	}
	return filepath.Join(DataDir(), "tunnel.log")
}

// BrowserStatePath is the browser state JSON beside the config file.
func BrowserStatePath() string {
	if v := os.Getenv("JEVONIAN_BROWSER_STATE"); v != "" {
		return v
	}
	return filepath.Join(filepath.Dir(ConfigPath()), "browser-state.json")
}

// UpdateStatePath is the update state JSON under the data directory.
func UpdateStatePath() string {
	if v := os.Getenv("JEVONIAN_UPDATE_STATE"); v != "" {
		return v
	}
	return filepath.Join(DataDir(), "update.json")
}

// LedgerPath is the legacy JSONL spend ledger under the data directory.
func LedgerPath() string {
	if v := os.Getenv("JEVONIAN_LEDGER"); v != "" {
		return v
	}
	return filepath.Join(DataDir(), "ledger.jsonl")
}

// LedgerDBPath is the SQLite ledger used by the Go runtime.
// JEVONIAN_LEDGER_DB overrides; else <dataDir>/ledger.sqlite.
func LedgerDBPath() string {
	if v := os.Getenv("JEVONIAN_LEDGER_DB"); v != "" {
		return v
	}
	return filepath.Join(DataDir(), "ledger.sqlite")
}

// ServeLogPath is the serve log under the data directory.
func ServeLogPath() string {
	if v := os.Getenv("JEVONIAN_SERVE_LOG"); v != "" {
		return v
	}
	return filepath.Join(DataDir(), "serve.log")
}

// LeaderboardPath is the leaderboard JSON under the data directory.
func LeaderboardPath() string {
	if v := os.Getenv("JEVONIAN_LEADERBOARD"); v != "" {
		return v
	}
	return filepath.Join(DataDir(), "leaderboard.json")
}

// ModelSyncStatePath is the model-sync state JSON under the data directory.
func ModelSyncStatePath() string {
	if v := os.Getenv("JEVONIAN_MODEL_SYNC_STATE"); v != "" {
		return v
	}
	return filepath.Join(DataDir(), "model-sync.json")
}
