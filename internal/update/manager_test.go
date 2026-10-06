package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }
func response(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body))}
}

func TestRegistryTarballProbe(t *testing.T) {
	for _, code := range []int{200, 404} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			calls := 0
			client := transportFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Header.Get("User-Agent") != "jevonian-update-check" {
					t.Fatal("user agent missing")
				}
				if _, ok := r.Context().Deadline(); !ok {
					t.Fatal("missing timeout")
				}
				if calls == 1 {
					if r.Method != "GET" || r.URL.String() != "https://registry.test/jevonian/latest" {
						t.Fatal(r)
					}
					return response(200, `{"version":"0.4.1","dist":{"tarball":"https://registry.test/package.tgz"}}`), nil
				}
				if r.Method != "HEAD" || r.URL.String() != "https://registry.test/package.tgz" {
					t.Fatal(r)
				}
				return response(code, ""), nil
			})
			version, err := (Registry{client, "https://registry.test/jevonian/latest"}).FetchLatest(context.Background())
			if calls != 2 {
				t.Fatal(calls)
			}
			if code == 200 && (err != nil || version != "0.4.1") {
				t.Fatal(version, err)
			}
			if code == 404 && (err == nil || !strings.Contains(err.Error(), "tarball is not available yet (404)")) {
				t.Fatal(err)
			}
		})
	}
}
func TestRegistryInvalidData(t *testing.T) {
	for _, doc := range []string{`{}`, `{"version":"../../oops"}`, `{"version":123}`, `not json`} {
		_, err := (Registry{HTTP: transportFunc(func(r *http.Request) (*http.Response, error) { return response(200, doc), nil })}).FetchLatest(context.Background())
		if err == nil {
			t.Fatal(doc)
		}
	}
	_, err := (Registry{URL: "file:///etc/passwd"}).FetchLatest(context.Background())
	if err == nil {
		t.Fatal("file URL allowed")
	}
}
func TestVersionComparison(t *testing.T) {
	for _, c := range []struct {
		a, b  string
		newer bool
	}{{"v1.2.3", "1.2.2", true}, {"1.2.3-beta", "1.2.3", false}, {"0.10.0", "0.9.100", true}, {"junk", "0.0.0", false}, {"1.0.0", "unknown", false}} {
		if IsNewerVersion(c.a, c.b) != c.newer {
			t.Fatal(c)
		}
	}
}
func TestCacheAndFailures(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	calls := 0
	opts := Options{Current: "0.0.1", CachePath: filepath.Join(t.TempDir(), "updates.json"), Installation: &Installation{Channel: NPM}, Now: func() time.Time { return now }, ReadInstalledVersion: func() string { return "0.0.1" }, FetchLatest: func(context.Context) (string, error) { calls++; return "0.0.2", nil }}
	first := New(opts)
	status := first.Check(context.Background(), false)
	if !status.UpdateAvailable || status.CheckedAt != "2026-01-01T00:00:00.000Z" {
		t.Fatal(status)
	}
	st, _ := os.Stat(opts.CachePath)
	if st.Mode().Perm() != 0o600 {
		t.Fatal(st.Mode())
	}
	second := New(opts)
	if second.Check(context.Background(), false).Latest != "0.0.2" || calls != 1 {
		t.Fatal(calls)
	}
	now = now.Add(UpdateInterval + time.Second)
	second.Check(context.Background(), false)
	if calls != 2 {
		t.Fatal(calls)
	}
	// A failed check preserves a useful prior successful snapshot, but reports error.
	opts.FetchLatest = func(context.Context) (string, error) { return "", errors.New("offline") }
	failed := New(opts).Check(context.Background(), true)
	if failed.Error != "offline" || failed.Latest != "0.0.2" {
		t.Fatal(failed)
	}
	opts.Installation = &Installation{Channel: PNPM}
	if New(opts).Status().Latest != "" {
		t.Fatal("cache leaked across channel")
	}
	if err := os.WriteFile(opts.CachePath, []byte("corrupted"), 0o600); err != nil {
		t.Fatal(err)
	}
	if New(opts).Status().Latest != "" {
		t.Fatal("corrupted cache accepted")
	}
}
func TestInstallPinsVersionAndVerifiesDisk(t *testing.T) {
	for _, channel := range []Channel{NPM, PNPM} {
		t.Run(string(channel), func(t *testing.T) {
			var disk atomic.Value
			disk.Store("0.0.1")
			calls := 0
			manager := New(Options{Current: "0.0.1", Installation: &Installation{Channel: channel, Bin: "/space path/npm"},
				FetchLatest: func(context.Context) (string, error) { return "0.0.2", nil }, ReadInstalledVersion: func() string { return disk.Load().(string) },
				Run: func(ctx context.Context, bin string, args ...string) (string, error) {
					calls++
					if bin != "/space path/npm" || strings.Join(args, " ") != strings.Join(InstallArgs(channel, "0.0.2"), " ") {
						t.Fatal(bin, args)
					}
					disk.Store("0.0.2")
					return "", nil
				}})
			status, err := manager.Install(context.Background())
			if err != nil || status.Current != "0.0.1" || status.Installed != "0.0.2" || !status.RestartRequired || calls != 1 {
				t.Fatal(status, err, calls)
			}
			if _, err := manager.Install(context.Background()); err != nil || calls != 1 {
				t.Fatal(err, calls)
			}
		})
	}
	unchanged := New(Options{Current: "0.0.1", Installation: &Installation{Channel: NPM}, FetchLatest: func(context.Context) (string, error) { return "0.0.2", nil }, ReadInstalledVersion: func() string { return "0.0.1" }, Run: func(context.Context, string, ...string) (string, error) { return "", nil }})
	if _, err := unchanged.Install(context.Background()); err == nil || !strings.Contains(err.Error(), "still 0.0.1") {
		t.Fatal(err)
	}
}
func TestSourceAndUnknownNeverUpdate(t *testing.T) {
	for _, channel := range []Channel{Source, Unknown} {
		manager := New(Options{Installation: &Installation{Channel: channel}, FetchLatest: func(context.Context) (string, error) { t.Fatal("source made network request"); return "", nil }, Run: func(context.Context, string, ...string) (string, error) {
			t.Fatal("source spawned process")
			return "", nil
		}})
		if s := manager.Check(context.Background(), true); s.Latest != "" || s.InstallCommand != "" {
			t.Fatal(s)
		}
		manager.Install(context.Background())
	}
}
func TestDetectionConservativeAndInjected(t *testing.T) {
	dir := t.TempDir()
	checkout := filepath.Join(dir, "checkout")
	if err := os.Mkdir(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(checkout, "go.mod"), []byte("module example"), 0o600)
	for _, path := range []string{filepath.Join(checkout, "jevonian"), filepath.Join(checkout, "src", "cli.ts"), filepath.Join(checkout, "node_modules", "jevonian", "npm", "bin", "jevonian.js")} {
		got := DetectInstallation(DetectionOptions{Executable: path, Env: map[string]string{"JEVONIAN_INSTALL_CHANNEL": "npm", "JEVONIAN_INSTALL_GLOBAL": "1"}})
		if got.Channel != Source {
			t.Fatal(got)
		}
	}
	node := filepath.Join(dir, "bin", "node")
	os.MkdirAll(filepath.Dir(node), 0o755)
	npm := filepath.Join(filepath.Dir(node), "npm")
	os.WriteFile(npm, []byte("fixture"), 0o755)
	entry := filepath.Join(dir, "lib", "node_modules", "jevonian", "bin", "jevonian.js")
	got := DetectInstallation(DetectionOptions{Executable: filepath.Join(dir, "jevonian"), Entry: entry, NodeExecutable: node, Platform: "linux", Env: map[string]string{}})
	if got.Channel != NPM || got.Bin != npm || got.PackagePath != filepath.Join(dir, "lib", "node_modules", "jevonian", "package.json") {
		t.Fatal(got)
	}
	local := DetectInstallation(DetectionOptions{Executable: filepath.Join(dir, "jevonian"), Entry: filepath.Join(dir, "project", "node_modules", "jevonian", "bin", "jevonian.js"), Env: map[string]string{}})
	if local.Channel != Unknown {
		t.Fatal(local)
	}
	direct := DetectInstallation(DetectionOptions{Executable: filepath.Join(dir, "jevonian"), Env: map[string]string{"JEVONIAN_INSTALL_CHANNEL": "binary"}})
	if direct.Channel != Binary {
		t.Fatal(direct)
	}
}
func TestJSONContract(t *testing.T) {
	b, err := json.Marshal(New(Options{Current: "0.0.1", Installation: &Installation{Channel: Unknown}}).Status())
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	json.Unmarshal(b, &value)
	for _, key := range []string{"current", "installed", "channel", "updateAvailable", "restartRequired"} {
		if _, ok := value[key]; !ok {
			t.Fatal(string(b))
		}
	}
	if _, ok := value["error"]; ok {
		t.Fatal("optional error must be omitted")
	}
}
