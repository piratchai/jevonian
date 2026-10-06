// Package multiacct holds the multi-account seams: several providers may read
// several local sign-ins of the same agent (a second Claude Code, Codex,
// Devin, Cursor, or WorkBuddy account on one machine). The login a provider
// names is what keys its token cache, its renewal lock, and its display label.
//
// Port of the multi-account parts of src/oauth.ts (loginKey), src/cli.ts
// (loginFromFlags, the `account=` label), and src/cursor.ts (loginKey).
//
// This package is a leaf (it imports only internal/config) so oauth, the
// provider wires, the server, and the CLI can all depend on it without cycles.
package multiacct

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/xinyao27/jevonian/internal/config"
)

// Login is a provider's alternate local sign-in. Structurally the TS LoginSpec /
// ProviderLogin; nil means "the agent's own default sign-in".
type Login = config.ProviderLogin

// IsEmpty reports whether login names nothing that changes where a sign-in is
// read from (Label alone does not).
func IsEmpty(login *Login) bool {
	return login == nil ||
		(login.Home == "" && login.CredentialsPath == "" &&
			login.KeychainService == "" && login.KeychainAccount == "")
}

// Key identifies a login for the token cache. The source alone is not enough:
// two providers may read two accounts of the same source, and they must not
// share one cached token. A login naming nothing keys as the bare source.
func Key(source string, login *Login) string {
	if IsEmpty(login) {
		return source
	}
	return source + "\x00" + strings.Join([]string{
		login.Home, login.CredentialsPath, login.KeychainService, login.KeychainAccount,
	}, "\x00")
}

// KeyPrefix is the prefix every non-default login key of source starts with,
// so a bare invalidate can clear every account of the source.
func KeyPrefix(source string) string {
	return source + "\x00"
}

// RenewalKey identifies a sign-in for a per-login renewal lock (Cursor's
// `cursor-agent status`). Unlike Key it is "" for the default login and does
// not include the source. Port of src/cursor.ts loginKey.
func RenewalKey(login *Login) string {
	if login == nil {
		return ""
	}
	return strings.Join([]string{
		login.Home, login.CredentialsPath, login.KeychainService, login.KeychainAccount,
	}, "\x00")
}

// Label is the `account=` text `jevonian providers` shows for a provider with a
// login, or "" when it has none.
func Label(login *Login) string {
	if login == nil {
		return ""
	}
	for _, candidate := range []string{login.Label, login.CredentialsPath, login.Home, login.KeychainService} {
		if candidate != "" {
			return candidate
		}
	}
	return "custom"
}

// ErrLoginNeedsOAuth is returned when --login-* flags are given to a provider
// that does not authenticate with a local sign-in.
var ErrLoginNeedsOAuth = errors.New(
	"--login-home / --login-file / --login-keychain describe a local sign-in, which only an OAuth provider reads. Add --auth oauth, or drop them and use --key.",
)

// FromFlags builds a provider login from the CLI flags `--login-home`,
// `--login-file`, `--login-keychain SERVICE[:ACCOUNT]`, and `--login-label`:
//
//	jevonian add claude-subscription --name claude-work --login-home ~/.claude-work
//
// A login only means something for an OAuth source — an API key is already per
// provider — so naming one without `--auth oauth` is refused rather than
// silently ignored. Returns (nil, nil) when no login flag was given.
func FromFlags(flags map[string]string, auth config.ProviderAuth) (*Login, error) {
	home := strings.TrimSpace(flags["login-home"])
	credentialsPath := strings.TrimSpace(flags["login-file"])
	keychain := strings.TrimSpace(flags["login-keychain"])
	label := strings.TrimSpace(flags["login-label"])
	if home == "" && credentialsPath == "" && keychain == "" && label == "" {
		return nil, nil
	}
	if auth != config.AuthOAuth {
		return nil, ErrLoginNeedsOAuth
	}
	login := &Login{Label: label}
	if keychain != "" {
		if service, account, ok := strings.Cut(keychain, ":"); ok {
			login.KeychainService = strings.TrimSpace(service)
			login.KeychainAccount = strings.TrimSpace(account)
		} else {
			login.KeychainService = keychain
		}
	}
	// Each source keeps its sign-in somewhere specific; a `login.file` is the file
	// to read, a `login.home` the directory it usually lives in.
	if home != "" {
		login.Home = ExpandHome(home)
	}
	if credentialsPath != "" {
		login.CredentialsPath = ExpandHome(credentialsPath)
	}
	return login, nil
}

// ExpandHome resolves `~` and `~/…` against the user's home, so a shell-quoted
// flag does not depend on the shell.
func ExpandHome(path string) string {
	home, _ := os.UserHomeDir()
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:])
	}
	return path
}
