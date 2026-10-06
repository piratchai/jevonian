package devin

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/xinyao27/jevonian/internal/config"
)

// CredentialsPath is where the Devin CLI (`devin auth login`) keeps its session
// token. JEVONIAN_DEVIN_CREDENTIALS wins; then the login's own file or home;
// otherwise %APPDATA%\devin on Windows and $XDG_DATA_HOME/devin (default
// ~/.local/share/devin) elsewhere. Port of devinCredentialsPath in src/oauth.ts.
func CredentialsPath(login *config.ProviderLogin) string {
	if override := strings.TrimSpace(os.Getenv("JEVONIAN_DEVIN_CREDENTIALS")); override != "" {
		return override
	}
	if login != nil && login.CredentialsPath != "" {
		return login.CredentialsPath
	}
	home, _ := os.UserHomeDir()
	if runtime.GOOS == "windows" {
		if login != nil && login.Home != "" {
			return filepath.Join(login.Home, "credentials.toml")
		}
		appData := os.Getenv("APPDATA")
		if appData == "" {
			appData = filepath.Join(home, "AppData", "Roaming")
		}
		return filepath.Join(appData, "devin", "credentials.toml")
	}
	if login != nil && login.Home != "" {
		return filepath.Join(login.Home, "credentials.toml")
	}
	base := strings.TrimSpace(os.Getenv("XDG_DATA_HOME"))
	if base == "" {
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "devin", "credentials.toml")
}

var (
	tomlBasic   = regexp.MustCompile(`^([A-Za-z0-9_.-]+)\s*=\s*"((?:[^"\\]|\\.)*)"\s*(?:#.*)?$`)
	tomlLiteral = regexp.MustCompile(`^([A-Za-z0-9_.-]+)\s*=\s*'([^']*)'\s*(?:#.*)?$`)
	tomlEscape  = regexp.MustCompile(`\\(["\\nrt])`)
)

// ParseFlatToml reads the flat `key = "value"` lines of Devin's
// credentials.toml. Tables, arrays and multi-line strings are ignored.
func ParseFlatToml(text string) map[string]string {
	result := map[string]string{}
	for _, raw := range regexp.MustCompile(`\r?\n`).Split(text, -1) {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "[") {
			continue
		}
		if m := tomlBasic.FindStringSubmatch(line); m != nil {
			result[m[1]] = tomlEscape.ReplaceAllStringFunc(m[2], func(s string) string {
				switch s[1] {
				case 'n':
					return "\n"
				case 'r':
					return "\r"
				case 't':
					return "\t"
				}
				return s[1:]
			})
			continue
		}
		if m := tomlLiteral.FindStringSubmatch(line); m != nil {
			result[m[1]] = m[2]
		}
	}
	return result
}

func readCredential(login *config.ProviderLogin) (map[string]string, bool) {
	data, err := os.ReadFile(CredentialsPath(login))
	if err != nil {
		return nil, false
	}
	return ParseFlatToml(string(data)), true
}

// ReadToken returns the Devin session token (`windsurf_api_key`). Devin tokens
// do not expire and have no refresh flow: when the server rejects one, the user
// signs in again and the next read picks the new file up.
func ReadToken(login *config.ProviderLogin) (string, error) {
	data, ok := readCredential(login)
	if !ok {
		return "", errors.New("Devin credentials not found. Sign in with `devin auth login`, or set JEVONIAN_DEVIN_CREDENTIALS.")
	}
	token := strings.TrimSpace(data["windsurf_api_key"])
	if token == "" {
		return "", errors.New("Devin credential has an unexpected shape (no windsurf_api_key). Run `devin auth login` again.")
	}
	return token, nil
}

// HasCredential reports whether a Devin token can be read.
func HasCredential(login *config.ProviderLogin) bool {
	data, ok := readCredential(login)
	return ok && strings.TrimSpace(data["windsurf_api_key"]) != ""
}

// ServerURL is the API server from credentials.toml (`api_server_url`), or the
// public default.
func ServerURL(login *config.ProviderLogin) string {
	if data, ok := readCredential(login); ok {
		if v := strings.TrimSpace(data["api_server_url"]); v != "" {
			return strings.TrimRight(v, "/")
		}
	}
	return DefaultBaseURL
}
