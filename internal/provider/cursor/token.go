package cursor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/xinyao27/jevonian/internal/config"
)

const (
	defaultClientVersion = "2026.09.23-86fc751"
	refreshSkew          = 5 * time.Minute
	statusTimeout        = 30 * time.Second
)

var versionPattern = regexp.MustCompile(`^\d{4}\.\d{2}\.\d{2}-[0-9a-f]+$`)

// Exec runs a command and returns its stdout. Swappable so tests can stand in
// for the cursor-agent CLI and the macOS `security` tool.
type Exec func(ctx context.Context, name string, args ...string) (string, error)

func defaultExec(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return string(out), err
}

// execCommand is the process-wide runner; tests replace it.
var execCommand Exec = defaultExec

// ─── CLI discovery ──────────────────────────────────────────────────────────

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func isDirectory(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func whichSync(name string) string {
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		if candidate := filepath.Join(dir, name); isFile(candidate) {
			return candidate
		}
	}
	return ""
}

func isCursorAgent(path string) bool {
	real, err := filepath.EvalSymlinks(path)
	return err == nil && strings.Contains(real, "cursor-agent")
}

// Executable is the location of the `cursor-agent` CLI, or "" when it is not
// installed. A bare `agent` on PATH counts only when it resolves into a
// cursor-agent install.
func Executable() string {
	for _, name := range []string{"cursor-agent", "agent"} {
		found := whichSync(name)
		if found == "" {
			continue
		}
		if name == "cursor-agent" || isCursorAgent(found) {
			return found
		}
	}
	home, _ := os.UserHomeDir()
	for _, path := range []string{
		filepath.Join(home, ".local", "bin", "cursor-agent"),
		"/usr/local/bin/cursor-agent",
		"/opt/homebrew/bin/cursor-agent",
	} {
		if _, err := os.Stat(path); err == nil && !isDirectory(path) {
			return path
		}
	}
	return ""
}

// AuthPath is where `cursor-agent` keeps its sign-in away from a Mac's
// keychain. JEVONIAN_CURSOR_AUTH wins, then the login's own file or home.
func AuthPath(login *config.ProviderLogin) string {
	if override := strings.TrimSpace(os.Getenv("JEVONIAN_CURSOR_AUTH")); override != "" {
		return override
	}
	if login != nil && login.CredentialsPath != "" {
		return login.CredentialsPath
	}
	home, _ := os.UserHomeDir()
	loginHome := ""
	if login != nil {
		loginHome = login.Home
	}
	switch runtime.GOOS {
	case "windows":
		dir := loginHome
		if dir == "" {
			appData := os.Getenv("APPDATA")
			if appData == "" {
				appData = filepath.Join(home, "AppData", "Roaming")
			}
			dir = filepath.Join(appData, "Cursor")
		}
		return filepath.Join(dir, "auth.json")
	case "darwin":
		dir := loginHome
		if dir == "" {
			dir = filepath.Join(home, ".cursor")
		}
		return filepath.Join(dir, "auth.json")
	}
	dir := loginHome
	if dir == "" {
		base := os.Getenv("XDG_CONFIG_HOME")
		if base == "" {
			base = filepath.Join(home, ".config")
		}
		dir = filepath.Join(base, "cursor")
	}
	return filepath.Join(dir, "auth.json")
}

// HasCredential reports whether there is anything to read a token from. On a
// Mac the keychain may hold one even without auth.json.
func HasCredential(login *config.ProviderLogin) bool {
	if override := strings.TrimSpace(os.Getenv("JEVONIAN_CURSOR_AUTH")); override != "" {
		return isFile(override)
	}
	if isFile(AuthPath(login)) {
		return true
	}
	return runtime.GOOS == "darwin"
}

func readKeychainToken(ctx context.Context, login *config.ProviderLogin) string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	service, account := "cursor-access-token", "cursor-user"
	if login != nil && login.KeychainService != "" {
		service = login.KeychainService
	}
	if login != nil && login.KeychainAccount != "" {
		account = login.KeychainAccount
	}
	out, err := execCommand(ctx, "security", "find-generic-password", "-s", service, "-a", account, "-w")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func readAuthFileToken(login *config.ProviderLogin) string {
	data, err := os.ReadFile(AuthPath(login))
	if err != nil {
		return ""
	}
	var raw struct {
		AccessToken any `json:"accessToken"`
	}
	if json.Unmarshal(data, &raw) != nil {
		return ""
	}
	token, _ := raw.AccessToken.(string)
	return token
}

func decodeJWTClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 || parts[1] == "" {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil
	}
	var claims map[string]any
	if json.Unmarshal(payload, &claims) != nil {
		return nil
	}
	return claims
}

// ReadToken is the access token `cursor-agent` signed in with, or "". The
// macOS keychain is tried before auth.json.
func ReadToken(ctx context.Context, login *config.ProviderLogin) string {
	if runtime.GOOS == "darwin" {
		if token := readKeychainToken(ctx, login); token != "" {
			return token
		}
	}
	return readAuthFileToken(login)
}

// TokenExpiry is a JWT's `exp`, or the zero time when it does not say.
func TokenExpiry(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) < 2 || parts[1] == "" {
		return time.Time{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		Exp *float64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp == nil {
		return time.Time{}
	}
	return time.UnixMilli(int64(*claims.Exp * 1000))
}

// tokenClock is what tokenFresh and Token read as "now"; tests pin it.
var tokenClock = time.Now

func tokenFresh(token string, now time.Time) bool {
	if token == "" {
		return false
	}
	expiry := TokenExpiry(token)
	return expiry.IsZero() || expiry.Sub(now) > refreshSkew
}

// loginKey identifies a sign-in for the renewal map. Two Cursor accounts must
// not share one `cursor-agent status`: it renews whichever account the CLI is
// signed into, and the re-read that follows would hand one account the
// other's token.
func loginKey(login *config.ProviderLogin) string {
	if login == nil {
		return ""
	}
	return strings.Join([]string{login.Home, login.CredentialsPath, login.KeychainService, login.KeychainAccount}, "\x00")
}

type renewal struct {
	done  chan struct{}
	token string
}

// renewals are in-flight renewals, one per sign-in, so concurrent callers share
// rather than race.
var renewals = struct {
	sync.Mutex
	m map[string]*renewal
}{m: map[string]*renewal{}}

// Token is the token to call Cursor's API with. A token about to run out is
// renewed by `cursor-agent status`, which renews whenever it runs. Concurrent
// callers for the same sign-in share one renewal — but only that sign-in.
func Token(ctx context.Context, login *config.ProviderLogin) (string, error) {
	token := ReadToken(ctx, login)
	if tokenFresh(token, tokenClock()) {
		return token, nil
	}
	if path := Executable(); path != "" {
		key := loginKey(login)
		renewals.Lock()
		r, running := renewals.m[key]
		if !running {
			r = &renewal{done: make(chan struct{})}
			renewals.m[key] = r
		}
		renewals.Unlock()
		if !running {
			go func() {
				statusCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), statusTimeout)
				defer cancel()
				// Re-read after claiming this account's renewal. A late caller
				// may have observed the old token before the prior renewal
				// completed. OS/keychain IO stays outside the shared map lock.
				latest := ReadToken(statusCtx, login)
				if !tokenFresh(latest, tokenClock()) {
					// A failed status still often renews; the re-read decides.
					_, _ = execCommand(statusCtx, path, "status")
					latest = ReadToken(statusCtx, login)
				}
				r.token = latest
				renewals.Lock()
				close(r.done)
				delete(renewals.m, key)
				renewals.Unlock()
			}()
		}
		select {
		case <-r.done:
			if r.token != "" {
				return r.token, nil
			}
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	latest := ReadToken(ctx, login)
	if latest == "" {
		return "", errors.New("Cursor isn't signed in; run `cursor-agent login`, or point JEVONIAN_CURSOR_AUTH at an auth.json.")
	}
	if expiry := TokenExpiry(latest); !expiry.IsZero() && !expiry.After(tokenClock()) {
		return "", errors.New("Cursor's sign-in has run out; run `cursor-agent login` to sign in again.")
	}
	return latest, nil
}

// ClientVersion is the CLI version the API is told it is talking to, as
// `cli-<version>`: the installed binary's version directory, else the newest
// one under ~/.local/share/cursor-agent/versions, else a pinned default.
func ClientVersion() string {
	version := ""
	if path := Executable(); path != "" {
		parts := strings.Split(filepath.ToSlash(path), "/")
		if len(parts) >= 2 && versionPattern.MatchString(parts[len(parts)-2]) {
			version = parts[len(parts)-2]
		}
	}
	if version == "" {
		home, _ := os.UserHomeDir()
		dirs := []string{filepath.Join(home, ".local", "share", "cursor-agent", "versions")}
		if runtime.GOOS == "windows" {
			if local := os.Getenv("LOCALAPPDATA"); local != "" {
				dirs = append(dirs, filepath.Join(local, "cursor-agent", "versions"))
			}
		}
		for _, dir := range dirs {
			entries, _ := os.ReadDir(dir)
			for _, entry := range entries {
				if entry.IsDir() && versionPattern.MatchString(entry.Name()) && entry.Name() > version {
					version = entry.Name()
				}
			}
		}
	}
	if version == "" {
		version = defaultClientVersion
	}
	return "cli-" + version
}

// ─── account status ─────────────────────────────────────────────────────────

// Account is who Cursor's CLI says is signed in.
type Account struct {
	User string
	Plan string
}

var ansiPattern = regexp.MustCompile("\x1b\\[[0-9;?]*[A-Za-z]")

func ansiStrip(text string) string { return ansiPattern.ReplaceAllString(text, "") }

// ParseAbout reads `cursor-agent about --format json`: the email and plan.
// An update notice printed before the JSON is skipped.
func ParseAbout(out string) (Account, bool) {
	text := strings.TrimSpace(ansiStrip(out))
	if brace := strings.Index(text, "{"); brace > 0 {
		text = text[brace:]
	}
	var about map[string]any
	if json.Unmarshal([]byte(text), &about) != nil {
		return Account{}, false
	}
	user, _ := about["userEmail"].(string)
	plan, _ := about["subscriptionTier"].(string)
	user, plan = strings.TrimSpace(user), strings.TrimSpace(plan)
	if user == "" {
		return Account{}, false
	}
	return Account{User: user, Plan: plan}, true
}

// FetchAccount asks the CLI who is signed in.
func FetchAccount(ctx context.Context) (Account, bool) {
	path := Executable()
	if path == "" {
		return Account{}, false
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := execCommand(ctx, path, "about", "--format", "json")
	if err != nil {
		return Account{}, false
	}
	return ParseAbout(out)
}
