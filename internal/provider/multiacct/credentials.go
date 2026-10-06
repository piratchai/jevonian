package multiacct

// Credential store: ~/.config/jevonian/credentials.json, one API key per
// provider. Port of src/credentials.ts. In multiacct because the file is only
// ever keyed by provider — keys and logins are the two ways a provider's local
// identity is stored.

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Store is the credentials file. Path defaults to JEVONIAN_CREDENTIALS or
// $XDG_CONFIG_HOME/jevonian/credentials.json; override Path in tests.
type Store struct {
	Path string
}

// DefaultStore resolves the standard credentials path each call, so env
// changes between calls behave like the TypeScript module functions.
func DefaultStore() *Store { return &Store{} }

func (s *Store) path() string {
	if s != nil && s.Path != "" {
		return s.Path
	}
	if v := os.Getenv("JEVONIAN_CREDENTIALS"); v != "" {
		return v
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "jevonian", "credentials.json")
}

// Credentials maps provider name → { apiKey }.
type Credentials map[string]struct {
	APIKey string `json:"apiKey"`
}

// Load reads the whole store. A missing or malformed file reads as empty.
func (s *Store) Load() Credentials {
	data, err := os.ReadFile(s.path())
	if err != nil {
		return Credentials{}
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return Credentials{}
	}
	out := Credentials{}
	for provider, entry := range raw {
		var record struct {
			APIKey string `json:"apiKey"`
		}
		if err := json.Unmarshal(entry, &record); err != nil {
			continue
		}
		if record.APIKey != "" {
			out[provider] = struct {
				APIKey string `json:"apiKey"`
			}{APIKey: record.APIKey}
		}
	}
	return out
}

func (s *Store) write(creds Credentials) error {
	path := s.path()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// Set stores a provider API key.
func (s *Store) Set(provider, apiKey string) error {
	creds := s.Load()
	creds[provider] = struct {
		APIKey string `json:"apiKey"`
	}{APIKey: apiKey}
	return s.write(creds)
}

// Remove drops a provider's key; absent is a no-op.
func (s *Store) Remove(provider string) error {
	creds := s.Load()
	if _, ok := creds[provider]; !ok {
		return nil
	}
	delete(creds, provider)
	return s.write(creds)
}

// Get is the stored key for provider, or "".
func (s *Store) Get(provider string) string {
	if entry, ok := s.Load()[provider]; ok {
		return entry.APIKey
	}
	return ""
}

// MaskKey renders a key for display.
func MaskKey(apiKey string) string {
	if len(apiKey) <= 8 {
		return "****"
	}
	return apiKey[:4] + "…" + apiKey[len(apiKey)-4:]
}
