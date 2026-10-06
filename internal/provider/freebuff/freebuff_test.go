package freebuff

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xinyao27/jevonian/internal/provider/multiacct"
	"github.com/xinyao27/jevonian/internal/wire"
)

func TestResolveModelSpellings(t *testing.T) {
	r := NewResolver(nil)
	for _, name := range []string{
		"deepseek/deepseek-v4-flash", "deepseek-v4-flash", "freebuff/deepseek-v4-flash",
		"freebuff/deepseek/deepseek-v4-flash", "DeepSeek-V4-Flash",
	} {
		m, ok := r.Resolve(name)
		if !ok || m.ID != "deepseek/deepseek-v4-flash" || m.Agent != "base2-free-deepseek-flash" {
			t.Fatalf("%q → %+v ok=%v", name, m, ok)
		}
	}
	// The dotted and dashed spellings of one model are the same model.
	for _, name := range []string{"deepseek-v4.1-flash", "deepseek-v4-1-flash"} {
		if m, ok := r.Resolve(name); !ok || m.ID != "deepseek/deepseek-v4.1-flash" {
			t.Fatalf("%q → %+v ok=%v", name, m, ok)
		}
	}
	if _, ok := r.Resolve("no-such-model"); ok {
		t.Fatal("unknown model resolved")
	}
	// Config overrides win and add.
	r = NewResolver(map[string]string{"deepseek/deepseek-v4-flash": "custom-agent", "acme/new-model": "base2-free-new"})
	if m, _ := r.Resolve("deepseek-v4-flash"); m.Agent != "custom-agent" {
		t.Fatalf("override ignored: %+v", m)
	}
	if m, ok := r.Resolve("freebuff/new-model"); !ok || m.Agent != "base2-free-new" {
		t.Fatalf("extra ignored: %+v ok=%v", m, ok)
	}
}

func TestModelIDsAreBareAndSorted(t *testing.T) {
	ids := ModelIDs()
	if len(ids) == 0 {
		t.Fatal("no models")
	}
	for i, id := range ids {
		if strings.Contains(id, "/") {
			t.Fatalf("%q is not bare", id)
		}
		if i > 0 && ids[i-1] > id {
			t.Fatalf("unsorted at %d", i)
		}
	}
}

func TestPrepareBuildsEnvelope(t *testing.T) {
	model := Model{ID: "deepseek/deepseek-v4-flash", Agent: "base2-free-deepseek-flash"}
	body := wire.Body{
		"model":    "freebuff/deepseek-v4-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"stream":   false,
		"stop":     "END",
		"system":   "dropped: not a chat completions field",
		"tools":    []any{map[string]any{"type": "function", "function": map[string]any{"name": "bash"}}},
	}
	out := Prepare(body, model)
	if out["model"] != "deepseek/deepseek-v4-flash" || out["stream"] != true {
		t.Fatalf("model/stream: %+v", out)
	}
	if _, ok := out["system"]; ok {
		t.Fatal("non-whitelisted field leaked")
	}
	if got := out["provider"].(map[string]any)["data_collection"]; got != "deny" {
		t.Fatalf("data_collection = %v", got)
	}
	stop := out["stop"].([]any)
	if len(stop) != 2 || stop[0] != "END" || stop[1] != StopSentinel {
		t.Fatalf("stop = %v", stop)
	}
	msgs := out["messages"].([]any)
	first := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != BuffyMarker || len(msgs) != 2 {
		t.Fatalf("messages = %v", msgs)
	}
	tools := out["tools"].([]any)
	last := tools[len(tools)-1].(map[string]any)["function"].(map[string]any)["name"]
	if len(tools) != 2 || last != "end_turn" {
		t.Fatalf("toolset signature missing: %v", tools)
	}
	// The caller's body is never mutated.
	if body["stream"] != false || len(body["tools"].([]any)) != 1 {
		t.Fatal("input mutated")
	}
}

func TestNormalizeMessagesKeepsMarkerAndMapsDeveloper(t *testing.T) {
	// A system message already opening with the marker is untouched.
	got := normalizeMessages([]any{map[string]any{"role": "system", "content": BuffyMarker + " extra"}})
	if got[0].(map[string]any)["content"] != BuffyMarker+" extra" || len(got) != 1 {
		t.Fatalf("%v", got)
	}
	// developer becomes system and gets the marker prefix.
	got = normalizeMessages([]any{
		map[string]any{"role": "developer", "content": "be brief"},
		map[string]any{"role": "user", "content": "x"},
	})
	first := got[0].(map[string]any)
	if first["role"] != "system" || first["content"] != BuffyMarker+"\n\nbe brief" || len(got) != 2 {
		t.Fatalf("%v", got)
	}
	// Text-part content is prefixed on its first text part.
	got = normalizeMessages([]any{map[string]any{"role": "system", "content": []any{
		map[string]any{"type": "text", "text": "rules"},
	}}})
	parts := got[0].(map[string]any)["content"].([]any)
	if parts[0].(map[string]any)["text"] != BuffyMarker+"\n\nrules" {
		t.Fatalf("%v", parts)
	}
	// A user-first conversation gets a leading marker system message.
	got = normalizeMessages([]any{map[string]any{"role": "user", "content": "hi"}})
	if got[0].(map[string]any)["role"] != "system" || len(got) != 2 {
		t.Fatalf("%v", got)
	}
}

func TestWithMetadataFreshClientID(t *testing.T) {
	base := wire.Body{"model": "m"}
	a := WithMetadata(base, "run-1", "inst-1")
	b := WithMetadata(base, "run-1", "inst-1")
	ma := a["codebuff_metadata"].(map[string]any)
	mb := b["codebuff_metadata"].(map[string]any)
	if ma["run_id"] != "run-1" || ma["freebuff_instance_id"] != "inst-1" || ma["cost_mode"] != "free" {
		t.Fatalf("%v", ma)
	}
	ca, cb := ma["client_id"].(string), mb["client_id"].(string)
	if len(ca) != 13 || ca == cb || strings.Trim(ca, base36) != "" {
		t.Fatalf("client ids %q %q", ca, cb)
	}
	if _, ok := base["codebuff_metadata"]; ok {
		t.Fatal("input mutated")
	}
	if _, ok := WithMetadata(base, "r", "")["codebuff_metadata"].(map[string]any)["freebuff_instance_id"]; ok {
		t.Fatal("empty instance id must be omitted")
	}
}

func TestClassify(t *testing.T) {
	now := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		status int
		body   string
		hdr    http.Header
		kind   Kind
	}{
		{"auth", 401, `{"error":"bad token"}`, nil, KindAuth},
		{"banned", 403, `{"status":"banned","resumes_at":"2026-10-06T00:00:00Z"}`, nil, KindBanned},
		{"rate limit", 429, `{"status":"rate_limited","retryAfterMs":60000}`, nil, KindRateLimit},
		{"waiting room 503", 503, `busy`, nil, KindWaitingRoom},
		{"gate 428", 428, `{"error":"waiting_room_required"}`, nil, KindSession},
		{"superseded", 409, `{"error":"session_superseded"}`, nil, KindSession},
		{"mismatch", 409, `{"error":"session_model_mismatch"}`, nil, KindSession},
		{"expired", 410, `{"error":"session_expired"}`, nil, KindSession},
		{"run gone", 400, `runId not found`, nil, KindRun},
		{"model gone", 400, `{"error":"model_unavailable"}`, nil, KindModel},
		{"other", 500, `boom`, nil, KindOther},
	}
	for _, tc := range cases {
		e := Classify(tc.status, tc.body, tc.hdr, now)
		if e.Kind != tc.kind {
			t.Errorf("%s: kind %s want %s", tc.name, e.Kind, tc.kind)
		}
	}
	rl := Classify(429, `{"retryAfterMs":90000}`, nil, now)
	if !rl.ResetsAt.Equal(now.Add(90 * time.Second)) {
		t.Fatalf("relative reset = %v", rl.ResetsAt)
	}
	rl = Classify(429, `{"resetAt":"2026-10-05T08:00:00Z"}`, nil, now)
	if !rl.ResetsAt.Equal(time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)) {
		t.Fatalf("absolute reset = %v", rl.ResetsAt)
	}
	h := http.Header{"Retry-After": {"120"}}
	if e := Classify(429, `slow down`, h, now); !e.ResetsAt.Equal(now.Add(2 * time.Minute)) {
		t.Fatalf("header reset = %v", e.ResetsAt)
	}
	ban := Classify(403, `{"status":"banned","resumes_at":"2026-10-06T00:00:00Z"}`, nil, now)
	if !ban.ResetsAt.Equal(time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("ban resume = %v", ban.ResetsAt)
	}
	if !Classify(409, `session_superseded`, nil, now).Retryable() || Classify(401, ``, nil, now).Retryable() {
		t.Fatal("retryable wrong")
	}
}

// mock is a Freebuff upstream that records the calls it saw.
type mock struct {
	mu        sync.Mutex
	sessions  atomic.Int32
	runs      atomic.Int32
	queuePoll int // polls answered "queued" before "active"
	polled    int
	sessionFn func(w http.ResponseWriter, r *http.Request) bool
	calls     []string
	headers   []http.Header
}

func (m *mock) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.calls = append(m.calls, r.Method+" "+r.URL.Path)
		m.headers = append(m.headers, r.Header.Clone())
		m.mu.Unlock()
		switch {
		case r.URL.Path == sessionPath && r.Method == http.MethodPost:
			if m.sessionFn != nil && m.sessionFn(w, r) {
				return
			}
			m.sessions.Add(1)
			time.Sleep(20 * time.Millisecond) // widen the single-flight window
			status := "active"
			if m.queuePoll > 0 {
				status = "queued"
			}
			json.NewEncoder(w).Encode(map[string]any{
				"status": status, "instanceId": r.Header.Get("x-freebuff-instance-id"),
				"model": r.Header.Get("x-freebuff-model"), "remainingMs": 3_600_000,
			})
		case r.URL.Path == sessionPath && r.Method == http.MethodGet:
			m.mu.Lock()
			m.polled++
			done := m.polled >= m.queuePoll
			m.mu.Unlock()
			status := "queued"
			if done {
				status = "active"
			}
			json.NewEncoder(w).Encode(map[string]any{
				"status": status, "instanceId": r.Header.Get("x-freebuff-instance-id"), "remainingMs": 3_600_000,
			})
		case r.URL.Path == runsPath:
			n := m.runs.Add(1)
			json.NewEncoder(w).Encode(map[string]any{"runId": "run-" + string(rune('a'+n-1))})
		default:
			http.NotFound(w, r)
		}
	})
}

func newManager(t *testing.T, m *mock) *Manager {
	t.Helper()
	srv := httptest.NewServer(m.handler())
	t.Cleanup(srv.Close)
	return NewManager(Options{HTTP: srv.Client(), BaseURL: srv.URL, Token: "tok", QueueGap: time.Millisecond})
}

func TestEnsureSessionIsSingleFlight(t *testing.T) {
	m := &mock{}
	mgr := newManager(t, m)
	var wg sync.WaitGroup
	ids := make([]string, 16)
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := mgr.Ensure(context.Background(), "deepseek/deepseek-v4-flash")
			if err != nil {
				t.Error(err)
				return
			}
			ids[i] = s.InstanceID
		}()
	}
	wg.Wait()
	if got := m.sessions.Load(); got != 1 {
		t.Fatalf("created %d sessions; a burst must share one", got)
	}
	for _, id := range ids {
		if id == "" || id != ids[0] {
			t.Fatalf("callers got different sessions: %v", ids)
		}
	}
	// A second call reuses the cache.
	if _, err := mgr.Ensure(context.Background(), "deepseek/deepseek-v4-flash"); err != nil || m.sessions.Load() != 1 {
		t.Fatalf("cache miss: err=%v sessions=%d", err, m.sessions.Load())
	}
	// Invalidate forces a new one.
	mgr.Invalidate("deepseek/deepseek-v4-flash")
	if _, err := mgr.Ensure(context.Background(), "deepseek/deepseek-v4-flash"); err != nil || m.sessions.Load() != 2 {
		t.Fatalf("invalidate: err=%v sessions=%d", err, m.sessions.Load())
	}
}

func TestEnsureSessionSendsModelAndInstanceHeaders(t *testing.T) {
	m := &mock{}
	mgr := newManager(t, m)
	if _, err := mgr.Ensure(context.Background(), "mimo/mimo-v2.5"); err != nil {
		t.Fatal(err)
	}
	h := m.headers[0]
	if h.Get("x-freebuff-model") != "mimo/mimo-v2.5" || h.Get("x-freebuff-instance-id") == "" ||
		h.Get("Authorization") != "Bearer tok" {
		t.Fatalf("headers %v", h)
	}
}

func TestEnsureSessionWaitsOutTheQueue(t *testing.T) {
	m := &mock{queuePoll: 2}
	mgr := newManager(t, m)
	s, err := mgr.Ensure(context.Background(), "deepseek/deepseek-v4-flash")
	if err != nil || s.InstanceID == "" {
		t.Fatalf("queued session: %+v err=%v", s, err)
	}
	if m.polled != 2 {
		t.Fatalf("polled %d times", m.polled)
	}
}

func TestEnsureSessionQueueGivesUp(t *testing.T) {
	m := &mock{queuePoll: 1000}
	mgr := newManager(t, m)
	_, err := mgr.Ensure(context.Background(), "deepseek/deepseek-v4-flash")
	var fe *Error
	if !errors.As(err, &fe) || fe.Kind != KindWaitingRoom {
		t.Fatalf("err = %v", err)
	}
}

func TestEnsureSessionSurfacesClassifiedErrors(t *testing.T) {
	m := &mock{sessionFn: func(w http.ResponseWriter, r *http.Request) bool {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"error":"session_model_mismatch"}`))
		return true
	}}
	mgr := newManager(t, m)
	_, err := mgr.Ensure(context.Background(), "mimo/mimo-v2.5")
	var fe *Error
	if !errors.As(err, &fe) || fe.Kind != KindSession || fe.Status != 409 {
		t.Fatalf("err = %v", err)
	}
	// A failed creation is not cached: the next call tries again.
	m.sessionFn = nil
	if _, err := mgr.Ensure(context.Background(), "mimo/mimo-v2.5"); err != nil {
		t.Fatalf("retry after failure: %v", err)
	}
}

func TestRunIsReusedAndStartsPruner(t *testing.T) {
	m := &mock{}
	mgr := newManager(t, m)
	a, err := mgr.Run(context.Background(), "deepseek/deepseek-v4-flash", "base2-free-deepseek-flash")
	if err != nil || a == "" {
		t.Fatal(a, err)
	}
	b, _ := mgr.Run(context.Background(), "deepseek/deepseek-v4-flash", "base2-free-deepseek-flash")
	if a != b {
		t.Fatalf("run not reused: %s vs %s", a, b)
	}
	if got := m.runs.Load(); got != 2 {
		t.Fatalf("expected root + context-pruner = 2 START calls, got %d", got)
	}
	mgr.InvalidateRun("deepseek/deepseek-v4-flash", "base2-free-deepseek-flash")
	c, _ := mgr.Run(context.Background(), "deepseek/deepseek-v4-flash", "base2-free-deepseek-flash")
	if c == a {
		t.Fatal("InvalidateRun kept the run")
	}
}

func TestCredentialsResolution(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("JEVONIAN_FREEBUFF_AUTH", "")
	t.Setenv("FREEBUFF_AUTH_TOKEN", "")

	if _, err := ReadToken(nil); !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("missing file err = %v", err)
	}
	if HasCredential(nil) {
		t.Fatal("HasCredential true with nothing")
	}

	path := filepath.Join(dir, "jevonian", "freebuff.json")
	if err := WriteCredentials(path, Credentials{AuthToken: "file-token", Email: "a@b.c"}); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("credentials mode %v", st.Mode().Perm())
	}
	if tok, err := ReadToken(nil); err != nil || tok != "file-token" {
		t.Fatalf("file token = %q err=%v", tok, err)
	}

	t.Setenv("FREEBUFF_AUTH_TOKEN", "env-token")
	if tok, _ := ReadToken(nil); tok != "env-token" {
		t.Fatalf("env must beat the default file, got %q", tok)
	}

	// An explicit file wins over the env token: it names one account.
	other := filepath.Join(dir, "other.json")
	if err := WriteCredentials(other, Credentials{AuthToken: "other-token"}); err != nil {
		t.Fatal(err)
	}
	login := &multiacct.Login{CredentialsPath: other}
	if tok, _ := ReadToken(login); tok != "other-token" {
		t.Fatalf("login file token = %q", tok)
	}
	t.Setenv("JEVONIAN_FREEBUFF_AUTH", path)
	if tok, _ := ReadToken(nil); tok != "file-token" {
		t.Fatalf("JEVONIAN_FREEBUFF_AUTH token = %q", tok)
	}

	bad := filepath.Join(dir, "bad.json")
	os.WriteFile(bad, []byte(`{"authToken":""}`), 0o600)
	if _, err := ReadCredentials(bad); err == nil || !strings.Contains(err.Error(), "no authToken") {
		t.Fatalf("empty token err = %v", err)
	}
}

func TestEndpointFromBaseURL(t *testing.T) {
	for in, want := range map[string]string{
		"":                                 BaseURL,
		"https://www.codebuff.com":         "https://www.codebuff.com",
		"https://www.codebuff.com/api/v1":  "https://www.codebuff.com",
		"https://www.codebuff.com/api/v1/": "https://www.codebuff.com",
		"http://127.0.0.1:9000/v1":         "http://127.0.0.1:9000",
		"http://127.0.0.1:9000":            "http://127.0.0.1:9000",
	} {
		if got := EndpointFromBaseURL(in); got != want {
			t.Errorf("%q → %q want %q", in, got, want)
		}
	}
	if got := ChatURL(""); got != "https://www.codebuff.com/api/v1/chat/completions" {
		t.Fatalf("ChatURL = %q", got)
	}
}

func TestSignInDeviceCodeFlow(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("JEVONIAN_FREEBUFF_AUTH", filepath.Join(dir, "freebuff.json"))
	var polls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/cli/code":
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			if body["fingerprintId"] == "" {
				t.Error("no fingerprintId")
			}
			json.NewEncoder(w).Encode(map[string]any{
				"loginUrl": "https://example.test/login", "fingerprintHash": "h", "expiresAt": 1791193342038, "expiresInMs": 60000,
			})
		case "/api/auth/cli/status":
			q := r.URL.Query()
			if q.Get("fingerprintHash") != "h" || q.Get("fingerprintId") == "" || q.Get("expiresAt") != "1791193342038" {
				t.Errorf("poll query %v", q)
			}
			polls++
			if polls < 3 {
				w.WriteHeader(http.StatusUnauthorized) // not signed in yet
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"default": map[string]any{
				"authToken": "secret", "id": "u1", "email": "me@example.test", "name": "Me",
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	var opened string
	c := &Client{HTTP: srv.Client(), OpenBrowser: func(u string) { opened = u }, Sleep: func(time.Duration) {}}
	creds, err := c.SignIn(context.Background(), srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if creds.AuthToken != "secret" || creds.Email != "me@example.test" || opened != "https://example.test/login" || polls != 3 {
		t.Fatalf("creds=%+v opened=%q polls=%d", creds, opened, polls)
	}
	if tok, _ := ReadToken(nil); tok != "secret" {
		t.Fatalf("not stored: %q", tok)
	}
}
