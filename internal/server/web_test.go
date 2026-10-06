package server_test

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/server"
)

func TestDashboardMountedOnlyOnMainAndNeverShadowsProtocols(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(`<!doctype html><div id="root">real-dashboard</div>`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JEVONIAN_WEB_DIR", dir)
	t.Setenv("JEVONIAN_WEB_DEV", "")
	cfg := config.Config{}
	for _, public := range []bool{false, true} {
		deps := server.Deps{Config: &cfg, Public: public}
		handler := server.New("", deps).Handler()
		for _, path := range []string{"/", "/providers", "/routing", "/logs/request-123"} {
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
			if public {
				// Public surface authenticates every path first (401 without a
				// key, as in createPublicApp); it must never serve the SPA.
				if (rr.Code != 404 && rr.Code != 401) || strings.Contains(rr.Body.String(), "real-dashboard") {
					t.Fatalf("public %s exposes dashboard: %d", path, rr.Code)
				}
			} else if rr.Code != 200 || !strings.Contains(rr.Body.String(), "real-dashboard") {
				t.Fatalf("dashboard mount %s: %d %s", path, rr.Code, rr.Body.String())
			}
		}
		for _, path := range []string{"/api/unknown", "/v1/unknown", "/assets/missing.js"} {
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
			if strings.Contains(rr.Body.String(), "real-dashboard") || rr.Code == 200 {
				t.Fatalf("SPA shadows %s: %d %s", path, rr.Code, rr.Body.String())
			}
		}
	}
}
