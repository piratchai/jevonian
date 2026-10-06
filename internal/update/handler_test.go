package update

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fakeLifecycle struct {
	draining  atomic.Bool
	resumed   atomic.Bool
	restarted chan struct{}
}

func (l *fakeLifecycle) Draining() bool      { return l.draining.Load() }
func (l *fakeLifecycle) ActiveRequests() int { return 3 }
func (l *fakeLifecycle) Resume()             { l.draining.Store(false); l.resumed.Store(true) }
func (l *fakeLifecycle) RestartAfterDrain(ctx context.Context, restart func(context.Context) error) error {
	l.draining.Store(true)
	err := restart(ctx)
	close(l.restarted)
	return err
}
func call(h *Handler, method, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	r.RemoteAddr = "127.0.0.1:1234"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func TestHandlerInstallsWhileServingAndRejectsDuplicate(t *testing.T) {
	var disk atomic.Value
	disk.Store("0.0.1")
	pkg, entry, packagePath, asset := npmPackageFixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	manager := New(Options{
		Current: "0.0.1",
		Installation: &Installation{
			Channel: NPM, Bin: "npm", Entry: entry, PackagePath: packagePath,
		},
		Detection:   DetectionOptions{NodeExecutable: "node", Env: map[string]string{}},
		FetchLatest: func(context.Context) (string, error) { return "0.0.2", nil },
		ReadInstalledVersion: func() string { return disk.Load().(string) },
		Run: func(_ context.Context, bin string, args ...string) (string, error) {
			if bin == "npm" {
				close(started)
				<-release
				disk.Store("0.0.2")
				return "", nil
			}
			if len(args) > 0 && args[len(args)-1] == "--download-only" {
				os.MkdirAll(filepath.Join(pkg, "native"), 0o755)
				os.WriteFile(filepath.Join(pkg, "native", asset), []byte("bin"), 0o755)
			}
			return "", nil
		},
	})
	life := &fakeLifecycle{restarted: make(chan struct{})}
	h := NewHandler(HandlerOptions{Manager: manager, Lifecycle: life, Restart: func(context.Context) error { return nil }})
	if w := call(h, "POST", "/api/update/check"); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	w := call(h, "POST", "/api/update/install")
	if w.Code != 202 || !strings.Contains(w.Body.String(), `"accepted":true`) {
		t.Fatal(w.Code, w.Body)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("install not started")
	}
	if life.Draining() {
		t.Fatal("drained before installation finished")
	}
	if w := call(h, "POST", "/update/install"); w.Code != 409 {
		t.Fatal("duplicate install accepted", w.Code)
	}
	// Polling stays responsive during a slow package install.
	if w := call(h, "GET", "/update"); w.Code != 200 {
		t.Fatal(w.Code)
	}
	close(release)
	select {
	case <-life.restarted:
	case <-time.After(time.Second):
		t.Fatal("did not restart")
	}
	if !life.Draining() || !manager.Status().RestartRequired {
		t.Fatal("restart contract")
	}
}
func TestHandlerFailureAndRestartOnly(t *testing.T) {
	for _, restartOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "failure", true: "restart-only"}[restartOnly], func(t *testing.T) {
			disk := "0.0.1"
			if restartOnly {
				disk = "0.0.2"
			}
			ran := make(chan struct{})
			manager := New(Options{Current: "0.0.1", Installation: &Installation{Channel: NPM}, FetchLatest: func(context.Context) (string, error) { return "0.0.2", nil }, ReadInstalledVersion: func() string { return disk }, Run: func(context.Context, string, ...string) (string, error) {
				close(ran)
				return "", errors.New("registry unavailable")
			}})
			manager.Check(context.Background(), true)
			life := &fakeLifecycle{restarted: make(chan struct{})}
			h := NewHandler(HandlerOptions{Manager: manager, Lifecycle: life, Restart: func(context.Context) error { return nil }})
			if w := call(h, "POST", "/update/install"); w.Code != 202 {
				t.Fatal(w.Code, w.Body)
			}
			if restartOnly {
				select {
				case <-life.restarted:
				case <-time.After(time.Second):
					t.Fatal("restart not called")
				}
				select {
				case <-ran:
					t.Fatal("restart-only reinstalled")
				default:
				}
			} else {
				deadline := time.Now().Add(time.Second)
				for !life.resumed.Load() && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
				if !life.resumed.Load() {
					t.Fatal("did not resume")
				}
				// Resume can precede storing error by a few instructions; synchronize through h.mu.
				for time.Now().Before(deadline) {
					h.mu.Lock()
					last := h.lastError
					h.mu.Unlock()
					if last != "" {
						break
					}
					time.Sleep(time.Millisecond)
				}
				w := call(h, "GET", "/api/update")
				if !strings.Contains(w.Body.String(), "registry unavailable") {
					t.Fatal(w.Body)
				}
			}
		})
	}
}
func TestHandlerGatesAndJSON(t *testing.T) {
	h := NewHandler(HandlerOptions{})
	if w := call(h, "POST", "/update/check"); w.Code != 503 {
		t.Fatal(w.Code)
	}
	if w := call(h, "POST", "/update/install"); w.Code != 503 {
		t.Fatal(w.Code)
	}
	w := call(h, "GET", "/update")
	var payload map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil || payload["active"] != false || payload["activeRequests"] != float64(0) {
		t.Fatal(payload, err)
	}
	r := httptest.NewRequest("POST", "/api/update/install", nil)
	r.RemoteAddr = "192.0.2.1:12"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
	life := &fakeLifecycle{restarted: make(chan struct{})}
	manager := New(Options{Current: "0.0.1", Installation: &Installation{Channel: NPM}, FetchLatest: func(context.Context) (string, error) { return "0.0.1", nil }})
	h = NewHandler(HandlerOptions{Manager: manager, Lifecycle: life, Restart: func(context.Context) error { return nil }})
	if w := call(h, "POST", "/update/install"); w.Code != 409 || !strings.Contains(w.Body.String(), "No update check") {
		t.Fatal(w.Code, w.Body)
	}
	call(h, "POST", "/update/check")
	if w := call(h, "POST", "/update/install"); w.Code != 409 || !strings.Contains(w.Body.String(), "up to date") {
		t.Fatal(w.Code, w.Body)
	}
	if len(h.Extensions()) != 3 {
		t.Fatal("extension contract")
	}
}
