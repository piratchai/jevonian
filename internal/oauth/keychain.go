package oauth

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"os/user"
	"runtime"
	"strings"
)

// ErrNoKeychain is returned on platforms without the macOS keychain.
var ErrNoKeychain = errors.New("keychain is only available on macOS")

// Runner executes a command and returns its stdout. Injectable so keychain
// access can be faked in tests.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// ExecRunner runs the command with os/exec.
func ExecRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	var stdout bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return stdout.Bytes(), nil
}

// SecurityKeychain reads and writes generic passwords with the macOS
// `security` CLI — CGO-free, same commands the TypeScript build ran.
type SecurityKeychain struct {
	// Run executes `security`; nil → ExecRunner.
	Run Runner
}

func (k SecurityKeychain) run() Runner {
	if k.Run != nil {
		return k.Run
	}
	return ExecRunner
}

// Find returns the password for service (and account when non-empty).
// A missing item reads as "" with no error.
func (k SecurityKeychain) Find(ctx context.Context, service, account string) (string, error) {
	if runtime.GOOS != "darwin" && k.Run == nil {
		return "", ErrNoKeychain
	}
	args := []string{"find-generic-password", "-s", service}
	if account != "" {
		args = append(args, "-a", account)
	}
	args = append(args, "-w")
	out, err := k.run()(ctx, "security", args...)
	if err != nil {
		return "", nil
	}
	return strings.TrimSpace(string(out)), nil
}

// Add upserts a generic password. An empty account means the current user.
func (k SecurityKeychain) Add(ctx context.Context, service, account, value string) error {
	if runtime.GOOS != "darwin" && k.Run == nil {
		return ErrNoKeychain
	}
	if account == "" {
		if u, err := user.Current(); err == nil {
			account = u.Username
		}
	}
	_, err := k.run()(ctx, "security",
		"add-generic-password", "-U", "-s", service, "-a", account, "-w", value)
	return err
}
