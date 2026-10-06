package server_test

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/server"
	"github.com/xinyao27/jevonian/internal/server/admin"
)

func TestMainServerAdminPersistenceAndPublicIsolation(t *testing.T) {
	cfg, err := config.ParseBytes([]byte(`{"providers":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	deps := server.Deps{Config: &cfg, Admin: &admin.Deps{ConfigPath: path}}
	main := server.New("", deps)
	request := func(handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:1234"
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		return rr
	}
	rr := request(main.Handler(), "GET", "/api/state", "")
	if rr.Code != 200 {
		t.Fatalf("admin mount: %d %s", rr.Code, rr.Body.String())
	}
	rr = request(main.Handler(), "PUT", "/api/token-saver", `{"enabled":false,"timeoutMs":321}`)
	if rr.Code != 200 || main.Config().TokenSaver.TimeoutMs != 321 || main.Config().TokenSaver.Enabled {
		t.Fatalf("config reload: %d %s", rr.Code, rr.Body.String())
	}
	public := server.NewPublicServer("", deps)
	rr = request(public.Handler(), "GET", "/api/state", "")
	// The public surface authenticates every path first (401 without a key);
	// either way the admin API is not reachable.
	if rr.Code != 404 && rr.Code != 401 {
		t.Fatalf("public listener exposes admin: %d", rr.Code)
	}
	req := httptest.NewRequest("GET", "/api/state", nil)
	req.RemoteAddr = "192.168.1.10:1234"
	req.Header.Set("X-Forwarded-For", "127.0.0.1")
	rr = httptest.NewRecorder()
	main.Handler().ServeHTTP(rr, req)
	if rr.Code != 403 {
		t.Fatalf("non-loopback admin allowed: %d", rr.Code)
	}
}
