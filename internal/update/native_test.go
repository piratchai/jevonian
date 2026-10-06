package update

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNativeAtomicReplacement(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX rename fixture")
	}
	for _, failure := range []string{"", "checksum", "version", "target-changed", "download", "manifest"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			dir, _ = filepath.EvalSymlinks(dir)
			target := filepath.Join(dir, "jevonian")
			link := filepath.Join(dir, "symlink")
			old := []byte("old executable")
			if err := os.WriteFile(target, old, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			binary := "new executable fixture"
			sum := fmt.Sprintf("%x", sha256.Sum256([]byte(binary)))
			if failure == "checksum" {
				sum = strings.Repeat("0", 64)
			}
			calls := 0
			client := transportFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != "GET" || !strings.HasPrefix(r.URL.String(), "https://releases.test/v0.0.2/") {
					t.Fatal(r)
				}
				if strings.HasSuffix(r.URL.Path, "checksums.txt") {
					if failure == "manifest" {
						return response(200, sum+"  wrong-asset\n"), nil
					}
					return response(200, sum+"  jevonian-linux-amd64\n"), nil
				}
				if failure == "download" {
					return response(404, "not found"), nil
				}
				return response(200, binary), nil
			})
			installer := NativeInstaller{HTTP: client, ReleaseBase: "https://releases.test", Platform: "linux", Arch: "amd64", Run: func(ctx context.Context, bin string, args ...string) (string, error) {
				if filepath.Dir(bin) != dir || len(args) != 1 || args[0] != "version" {
					t.Fatal(bin, args)
				}
				b, _ := os.ReadFile(bin)
				if string(b) != binary {
					t.Fatal(string(b))
				}
				st, _ := os.Stat(bin)
				if st.Mode().Perm() != 0o755 {
					t.Fatal(st.Mode())
				}
				if failure == "target-changed" {
					os.WriteFile(target, []byte("external update"), 0o755)
				}
				if failure == "version" {
					return "jevonian 0.0.1\n", nil
				}
				return "jevonian 0.0.2\n", nil
			}}
			err := installer.Install(context.Background(), link, "0.0.2")
			if (err != nil) != (failure != "") {
				t.Fatal(failure, err)
			}
			got, _ := os.ReadFile(target)
			expected := string(old)
			if failure == "" {
				expected = binary
			}
			if failure == "target-changed" {
				expected = "external update"
			}
			if string(got) != expected {
				t.Fatal(string(got), expected)
			}
			if _, err := os.Readlink(link); err != nil {
				t.Fatal("symlink was replaced", err)
			}
			files, _ := os.ReadDir(dir)
			if len(files) != 2 {
				t.Fatal("temporary file leaked", files)
			}
			if calls == 0 {
				t.Fatal("no injected HTTP request")
			}
		})
	}
}
func TestNativeFailClosed(t *testing.T) {
	installer := NativeInstaller{HTTP: transportFunc(func(*http.Request) (*http.Response, error) { t.Fatal("unexpected HTTP"); return nil, nil }), Platform: "windows", Arch: "amd64"}
	if err := installer.Install(context.Background(), "unused", "0.0.2"); err == nil {
		t.Fatal("Windows running binary replacement allowed")
	}
	if _, err := AssetName("linux", "386"); err == nil {
		t.Fatal("unsupported architecture")
	}
	if _, err := AssetName("freebsd", "amd64"); err == nil {
		t.Fatal("unsupported platform")
	}
	for _, manifest := range []string{"", "bad  jevonian-linux-amd64", strings.Repeat("a", 64) + "  jevonian-linux-amd64\n" + strings.Repeat("a", 64) + "  jevonian-linux-amd64"} {
		if _, err := ParseChecksum(manifest, "jevonian-linux-amd64"); err == nil {
			t.Fatal(manifest)
		}
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module fixture"), 0o600)
	target := filepath.Join(dir, "jevonian")
	os.WriteFile(target, []byte("old"), 0o755)
	installer.Platform = "linux"
	if err := installer.Install(context.Background(), target, "0.0.2"); err == nil {
		t.Fatal("source binary replacement allowed")
	}
}
