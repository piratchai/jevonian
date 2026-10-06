package update

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const DefaultReleaseBase = "https://github.com/xinyao27/jevonian/releases/download"
const MaxBinaryBytes = 128 << 20

// AssetName is the release contract shared with bin/jevonian.js: raw,
// CGO-free executable assets plus checksums.txt in the same vVERSION release.
func AssetName(platform, arch string) (string, error) {
	switch platform {
	case "darwin", "linux", "windows":
	default:
		return "", fmt.Errorf("unsupported platform %s", platform)
	}
	switch arch {
	case "amd64", "arm64":
	default:
		return "", fmt.Errorf("unsupported architecture %s", arch)
	}
	suffix := ""
	if platform == "windows" {
		suffix = ".exe"
	}
	return "jevonian-" + platform + "-" + arch + suffix, nil
}

type NativeInstaller struct {
	HTTP           HTTPClient
	ReleaseBase    string
	Platform, Arch string
	Run            Process // Checks the staged binary's version before any replacement.
}

// Install stages on the target filesystem, verifies SHA256 and executable
// version, fsyncs, then renames over the resolved target atomically. The old
// executable survives all pre-commit failures. Windows running executables
// cannot be replaced by rename; fail closed rather than remove/copy in place.
func (n NativeInstaller) Install(ctx context.Context, executable, version string) error {
	platform, arch := n.Platform, n.Arch
	if platform == "" {
		platform = runtime.GOOS
	}
	if arch == "" {
		arch = runtime.GOARCH
	}
	if platform == "windows" {
		return fmt.Errorf("direct binary self-update is unavailable on Windows; download the release and replace it after stopping Jevonian")
	}
	if !validVersion(version) {
		return fmt.Errorf("invalid release version")
	}
	asset, err := AssetName(platform, arch)
	if err != nil {
		return err
	}
	if executable == "" {
		return fmt.Errorf("update executable is unavailable")
	}
	target, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return err
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return err
	}
	if sourceCheckout(target) {
		return fmt.Errorf("refusing to replace a source checkout executable")
	}
	old, err := os.Stat(target)
	if err != nil {
		return err
	}
	if !old.Mode().IsRegular() {
		return fmt.Errorf("update target is not a regular file")
	}
	base := n.ReleaseBase
	if base == "" {
		base = DefaultReleaseBase
	}
	release := strings.TrimRight(base, "/") + "/v" + strings.TrimPrefix(version, "v") + "/"
	checks, err := request(ctx, n.HTTP, "GET", release+"checksums.txt", 15*time.Second)
	if err != nil {
		return err
	}
	if checks.StatusCode != 200 {
		checks.Body.Close()
		return fmt.Errorf("release checksums returned %d", checks.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(checks.Body, (1<<20)+1))
	checks.Body.Close()
	if err != nil {
		return err
	}
	if len(data) > 1<<20 {
		return fmt.Errorf("release checksums are too large")
	}
	expected, err := ParseChecksum(string(data), asset)
	if err != nil {
		return err
	}
	response, err := request(ctx, n.HTTP, "GET", release+asset, 2*time.Minute)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return fmt.Errorf("release binary returned %d", response.StatusCode)
	}
	if response.ContentLength > MaxBinaryBytes {
		return fmt.Errorf("release binary is too large")
	}
	staged, err := os.CreateTemp(filepath.Dir(target), ".jevonian-binary-*")
	if err != nil {
		return err
	}
	defer os.Remove(staged.Name())
	defer staged.Close()
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(staged, hash), io.LimitReader(response.Body, MaxBinaryBytes+1))
	if err != nil {
		return err
	}
	if size == 0 || size > MaxBinaryBytes {
		return fmt.Errorf("invalid release binary size")
	}
	if subtle.ConstantTimeCompare(hash.Sum(nil), expected) != 1 {
		return fmt.Errorf("release binary checksum mismatch")
	}
	if err := staged.Chmod(0o755); err != nil {
		return err
	}
	if err := staged.Sync(); err != nil {
		return err
	}
	if err := staged.Close(); err != nil {
		return err
	}
	run := n.Run
	if run == nil {
		run = RunProcess
	}
	verifyCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output, err := run(verifyCtx, staged.Name(), "version")
	if err != nil {
		return fmt.Errorf("release executable verification failed: %w", err)
	}
	got := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(output), "jevonian "))
	if got != strings.TrimPrefix(version, "v") {
		return fmt.Errorf("release executable version %q does not match %s", got, version)
	}
	// Do not overwrite an update installed by another process while downloading.
	current, err := os.Lstat(target)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(old, current) || current.Size() != old.Size() || !current.ModTime().Equal(old.ModTime()) {
		return fmt.Errorf("update target changed while downloading; retry the check")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(staged.Name(), target); err != nil {
		return fmt.Errorf("atomic binary replacement failed: %w", err)
	}
	// The rename is the commit point. Directory fsync is best effort: unsupported
	// filesystems must not turn a completed replacement into a reported rollback.
	if dir, err := os.Open(filepath.Dir(target)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

func ParseChecksum(manifest, asset string) ([]byte, error) {
	var result []byte
	for _, line := range strings.Split(manifest, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != asset {
			continue
		}
		if result != nil {
			return nil, fmt.Errorf("duplicate release checksum for %s", asset)
		}
		sum, err := hex.DecodeString(fields[0])
		if err != nil || len(sum) != sha256.Size {
			return nil, fmt.Errorf("invalid release checksum for %s", asset)
		}
		result = sum
	}
	if result == nil {
		return nil, fmt.Errorf("release checksum missing for %s", asset)
	}
	return result, nil
}
