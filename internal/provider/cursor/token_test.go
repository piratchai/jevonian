package cursor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/xinyao27/jevonian/internal/config"
)

// Port of src/cursor-token.test.ts — cursor-token renewal under concurrent
// accounts. The TS mocks child_process and counts `cursor-agent status` calls;
// the Go test swaps execCommand and the package's credential resolution the
// same way.

// expiredJWT is a JWT whose exp is in the past, so the token reads as stale.
func expiredJWT() string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1}`))
	return "header." + payload + ".signature"
}

func writeAuthFile(t *testing.T, dir, token string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(path, []byte(`{"accessToken":`+strconv_Quote(token)+`}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// strconv_Quote avoids importing strconv just for JSON string quoting.
func strconv_Quote(s string) string {
	data, _ := json.Marshal(s)
	return string(data)
}

// tokenFixture swaps the process-level knobs Token consults: PATH holds a
// fake cursor-agent, execCommand counts status calls and writes renewed
// tokens, tokenClock is real.
type tokenFixture struct {
	dir         string
	statusCalls atomic.Int64
	renewTo     map[string]string
	beforeRenew func(context.Context)
	restore     func()
}

func newTokenFixture(t *testing.T) *tokenFixture {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	// A plain file named cursor-agent is enough for Executable(), since exec
	// itself is swapped.
	if err := os.WriteFile(filepath.Join(bin, "cursor-agent"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	fx := &tokenFixture{dir: dir, renewTo: map[string]string{}}

	prevPath := os.Getenv("PATH")
	prevAuth := os.Getenv("JEVONIAN_CURSOR_AUTH")
	prevExec := execCommand
	if err := os.Setenv("PATH", bin); err != nil {
		t.Fatal(err)
	}
	_ = os.Unsetenv("JEVONIAN_CURSOR_AUTH")

	execCommand = func(ctx context.Context, name string, args ...string) (string, error) {
		// Keychain lookups find nothing, so the auth file is the credential that
		// is read.
		if len(args) > 0 && args[0] == "find-generic-password" {
			return "", nil
		}
		if len(args) > 0 && args[0] == "status" {
			if fx.beforeRenew != nil {
				fx.beforeRenew(ctx)
			}
			fx.statusCalls.Add(1)
			for path, token := range fx.renewTo {
				data, _ := json.Marshal(map[string]any{"accessToken": token})
				_ = os.WriteFile(path, data, 0o644)
			}
			return "", nil
		}
		return "", nil
	}

	fx.restore = func() {
		_ = os.Setenv("PATH", prevPath)
		if prevAuth == "" {
			_ = os.Unsetenv("JEVONIAN_CURSOR_AUTH")
		} else {
			_ = os.Setenv("JEVONIAN_CURSOR_AUTH", prevAuth)
		}
		execCommand = prevExec
	}
	return fx
}

func TestTokenRenewsEachAccountOnItsOwn(t *testing.T) {
	fx := newTokenFixture(t)
	defer fx.restore()

	workPath := writeAuthFile(t, filepath.Join(fx.dir, "work"), expiredJWT())
	homePath := writeAuthFile(t, filepath.Join(fx.dir, "home"), expiredJWT())
	fx.renewTo[workPath] = "fresh-work"
	fx.renewTo[homePath] = "fresh-home"
	// The fixture writes both accounts on status. Hold both status calls
	// until each account has entered renewal so scheduler order cannot make
	// one account look fresh before this test exercises its own renewal.
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	fx.beforeRenew = func(ctx context.Context) {
		arrived <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
	}
	go func() { <-arrived; <-arrived; close(release) }()

	ctx := context.Background()
	type result struct {
		token string
		err   error
	}
	work := make(chan result, 1)
	home := make(chan result, 1)
	go func() {
		token, err := Token(ctx, &config.ProviderLogin{CredentialsPath: workPath})
		work <- result{token, err}
	}()
	go func() {
		token, err := Token(ctx, &config.ProviderLogin{CredentialsPath: homePath})
		home <- result{token, err}
	}()
	w, h := <-work, <-home
	if w.err != nil || h.err != nil {
		t.Fatalf("errors: %v %v", w.err, h.err)
	}
	if w.token != "fresh-work" || h.token != "fresh-home" {
		t.Fatalf("tokens: %q %q", w.token, h.token)
	}
	// One renewal per account: sharing a single `cursor-agent status` is what
	// mixed them up.
	if got := fx.statusCalls.Load(); got != 2 {
		t.Fatalf("status calls: %d", got)
	}
}

func TestTokenSharesOneRenewalPerAccount(t *testing.T) {
	fx := newTokenFixture(t)
	defer fx.restore()

	path := writeAuthFile(t, filepath.Join(fx.dir, "solo"), expiredJWT())
	fx.renewTo[path] = "fresh-solo"

	ctx := context.Background()
	login := &config.ProviderLogin{CredentialsPath: path}
	type result struct {
		token string
		err   error
	}
	out := make(chan result, 2)
	for i := 0; i < 2; i++ {
		go func() {
			token, err := Token(ctx, login)
			out <- result{token, err}
		}()
	}
	a, b := <-out, <-out
	if a.err != nil || b.err != nil {
		t.Fatalf("errors: %v %v", a.err, b.err)
	}
	if a.token != "fresh-solo" || b.token != "fresh-solo" {
		t.Fatalf("tokens: %q %q", a.token, b.token)
	}
	if got := fx.statusCalls.Load(); got != 1 {
		t.Fatalf("status calls: %d", got)
	}
}

func TestTokenReturnsFreshWithoutRenewing(t *testing.T) {
	fx := newTokenFixture(t)
	defer fx.restore()

	path := writeAuthFile(t, filepath.Join(fx.dir, "fresh"), "still-good")
	token, err := Token(context.Background(), &config.ProviderLogin{CredentialsPath: path})
	if err != nil {
		t.Fatal(err)
	}
	if token != "still-good" {
		t.Fatalf("token: %q", token)
	}
	if got := fx.statusCalls.Load(); got != 0 {
		t.Fatalf("status calls: %d", got)
	}
}

func TestTokenErrorsWhenNothingToRead(t *testing.T) {
	fx := newTokenFixture(t)
	defer fx.restore()

	// No auth.json anywhere the login points, and no keychain hit.
	_, err := Token(context.Background(), &config.ProviderLogin{
		CredentialsPath: filepath.Join(fx.dir, "missing", "auth.json"),
	})
	if err == nil {
		t.Fatal("expected an error")
	}
}

func TestTokenExpiry(t *testing.T) {
	if got := TokenExpiry("no-dots"); !got.IsZero() {
		t.Fatalf("got %v", got)
	}
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1700000000}`))
	got := TokenExpiry("h." + payload + ".s")
	if got.Unix() != 1700000000 {
		t.Fatalf("got %v", got)
	}
}

func TestAuthPathOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "custom.json")
	t.Setenv("JEVONIAN_CURSOR_AUTH", path)
	if got := AuthPath(&config.ProviderLogin{CredentialsPath: "/other"}); got != path {
		t.Fatalf("got %q", got)
	}
}
