package devin

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// roundTripper routes requests to a canned handler.
type roundTripper func(req *http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestParseModels(t *testing.T) {
	row := func(label string, price float32) []byte {
		return BytesField(32, Concat(StringField(1, label), floatField(2, price)))
	}
	model := Concat(
		StringField(1, "Test Model"),
		VarintField(4, 1),
		VarintField(10, 3),
		StringField(22, "test-model"),
		BytesField(23, Concat(VarintField(4, 200000), VarintField(13, 32000))),
		row("Input", 1.2), row("Cached input", 0.2), row("Output", 5.5),
	)
	models := ParseModels(Concat(BytesField(1, model), BytesField(1, StringField(1, "Missing selector"))))
	if len(models) != 1 {
		t.Fatalf("models %v", models)
	}
	m := models[0]
	if m.ID != "test-model" || m.Label != "Test Model" || m.Vendor != "anthropic" || !m.Disabled {
		t.Fatalf("model %+v", m)
	}
	if m.ContextWindow != 200000 || m.MaxOutput != 32000 {
		t.Fatalf("windows %+v", m)
	}
	if m.Price == nil || m.Price.Input != 1.2 || m.Price.Output != 5.5 || m.Price.CacheRead == nil || *m.Price.CacheRead != 0.2 {
		t.Fatalf("price %+v", m.Price)
	}
}

func TestParseUserStatus(t *testing.T) {
	plan := Concat(
		BytesField(1, StringField(2, "Free")),
		VarintField(14, 55), VarintField(15, 20),
		VarintField(17, 1700000000), VarintField(18, 1700003600),
	)
	status := ParseUserStatus(BytesField(1, BytesField(13, plan)))
	if status.Plan != "Free" || *status.DailyRemainingPercent != 55 || *status.WeeklyRemainingPercent != 20 {
		t.Fatalf("status %+v", status)
	}
	if status.DailyResetsAt.Format(time.RFC3339) != "2023-11-14T22:13:20Z" ||
		status.WeeklyResetsAt.Format(time.RFC3339) != "2023-11-14T23:13:20Z" {
		t.Fatalf("resets %v %v", status.DailyResetsAt, status.WeeklyResetsAt)
	}
	if ParseUserStatus(nil) != (UserStatus{}) {
		t.Fatal("empty")
	}
	windows := status.Windows()
	if len(windows) != 2 || windows[0].UsedPercent != 45 || windows[0].ID != "devin-daily" ||
		windows[1].UsedPercent != 80 || windows[1].ID != "devin-weekly" {
		t.Fatalf("windows %+v", windows)
	}
}

func TestUnaryCallsSendMetadataBody(t *testing.T) {
	model := BytesField(1, Concat(StringField(1, "Free"), StringField(22, "swe-1-6-slow")))
	status := BytesField(1, BytesField(13, BytesField(1, StringField(2, "Free"))))
	var calls []*http.Request
	var bodies [][]byte
	client := &http.Client{Transport: roundTripper(func(req *http.Request) (*http.Response, error) {
		calls = append(calls, req)
		body, _ := io.ReadAll(req.Body)
		bodies = append(bodies, body)
		raw := status
		if strings.Contains(req.URL.Path, "GetCliModelConfigs") {
			raw = model
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(bytesReader(raw))}, nil
	})}
	models, err := FetchModels(testCtx(), client, "my-token", "https://example.test/")
	if err != nil || models[0].ID != "swe-1-6-slow" {
		t.Fatalf("models %v %v", models, err)
	}
	got, err := FetchUserStatus(testCtx(), client, "my-token", "https://example.test/")
	if err != nil || got.Plan != "Free" {
		t.Fatalf("status %+v %v", got, err)
	}
	for i, call := range calls {
		if !strings.HasPrefix(call.URL.String(), "https://example.test/exa.") {
			t.Fatal(call.URL)
		}
		if call.Header.Get("authorization") != "Basic my-token-my-token" {
			t.Fatal("authorization")
		}
		if call.Header.Get("content-type") != "application/proto" {
			t.Fatal("content-type")
		}
		if len(tFields(t, bodies[i], 1)) != 1 || len(parse(t, bodies[i])) != 1 {
			t.Fatal("body shape")
		}
		if tStr(t, tSub(t, bodies[i], 1), 3) != "my-token" {
			t.Fatal("token in metadata")
		}
	}
}

func TestUnaryErrorsNeverEchoCredentials(t *testing.T) {
	token := "opaque$secret-123"
	body, _ := json.Marshal(map[string]any{
		"code":    "permission_denied",
		"message": "echo " + token + " Basic " + token + "-" + token,
	})
	client := &http.Client{Transport: roundTripper(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 403, Header: http.Header{}, Body: io.NopCloser(bytesReader(body))}, nil
	})}
	for _, call := range []func() error{
		func() error { _, err := FetchModels(testCtx(), client, token, ""); return err },
		func() error { _, err := FetchUserStatus(testCtx(), client, token, ""); return err },
	} {
		err := call()
		if err == nil {
			t.Fatal("expected failure")
		}
		if !strings.Contains(err.Error(), "Devin authentication failed") || strings.Contains(err.Error(), token) {
			t.Fatalf("error %v", err)
		}
	}
}

func TestUnaryReadableMessageOnJSONError(t *testing.T) {
	client := &http.Client{Transport: roundTripper(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 403, Header: http.Header{},
			Body: io.NopCloser(strings.NewReader(`{"code":"permission_denied","message":"Please sign in"}`)),
		}, nil
	})}
	_, err := FetchModels(testCtx(), client, "bad", "")
	if err == nil || !strings.Contains(err.Error(), "GetCliModelConfigs failed (HTTP 403, auth): Please sign in") {
		t.Fatalf("err %v", err)
	}
}

// ─── catalog file (devin-catalog.test.ts) ───────────────────────────────────

func TestIsRoutable(t *testing.T) {
	ids := []string{"swe-1-6-slow", "claude-opus-4-8-medium", "MODEL_PRIVATE_11", "fusion-claude-opus-4-8-medium-gpt-6-sol-medium", "adaptive", "adaptive-fast", "arena-blind"}
	var routable []string
	for _, id := range ids {
		if IsRoutable(Model{ID: id}) {
			routable = append(routable, id)
		}
	}
	if len(routable) != 3 || routable[0] != "swe-1-6-slow" || routable[1] != "claude-opus-4-8-medium" || routable[2] != "MODEL_PRIVATE_11" {
		t.Fatalf("routable %v", routable)
	}
	if IsRoutable(Model{ID: "swe-1-6-slow", Disabled: true}) {
		t.Fatal("disabled routable")
	}
}

func TestModelMetaRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("JEVONIAN_DATA_DIR", dir)
	ResetModelMeta()
	cacheRead := 0.2
	SaveModelMeta([]Model{
		{ID: "swe-1-6-slow", Label: "SWE-1.6 Slow", ContextWindow: 200_000, MaxOutput: 32_000, Price: &Price{Input: 2, Output: 8, CacheRead: &cacheRead}},
		{ID: "MODEL_PRIVATE_11", Label: "Private"},
	})
	ResetModelMeta()
	meta, ok := LoadModelMeta()["swe-1-6-slow"]
	if !ok || meta.Label != "SWE-1.6 Slow" || meta.ContextWindow != 200_000 || meta.MaxOutput != 32_000 ||
		meta.Price == nil || meta.Price.Input != 2 || meta.Price.Output != 8 || *meta.Price.CacheRead != 0.2 {
		t.Fatalf("meta %+v", meta)
	}
	if m, ok := LookupModelMeta("devin-subscription/swe-1-6-slow"); !ok || m.ContextWindow != 200_000 {
		t.Fatalf("lookup %+v", m)
	}
	if _, ok := LookupModelMeta("MODEL_PRIVATE_11"); !ok {
		t.Fatal("missing private model")
	}
}

func TestModelMetaRefreshAndCorrupt(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("JEVONIAN_DATA_DIR", dir)
	ResetModelMeta()
	SaveModelMeta([]Model{{ID: "a", Label: "a", Price: &Price{Input: 1, Output: 2}}})
	if _, ok := LookupModelMeta("b"); ok {
		t.Fatal("stale")
	}
	SaveModelMeta([]Model{{ID: "b", Label: "b", Price: &Price{Input: 3, Output: 4}}})
	if m, _ := LookupModelMeta("b"); m.Price == nil || m.Price.Input != 3 {
		t.Fatalf("refresh %+v", m)
	}
	// Corrupt cache reads as empty.
	ResetModelMeta()
	if err := writeFileAtomic(ModelsPath(), []byte("{ nope")); err != nil {
		t.Fatal(err)
	}
	ResetModelMeta()
	if len(LoadModelMeta()) != 0 {
		t.Fatal("corrupt cache not empty")
	}
}

// ─── error classification ──────────────────────────────────────────────────

func TestClassifyRedaction(t *testing.T) {
	token := `arbitrary"secret\value`
	echoed, _ := json.Marshal(map[string]any{
		"message": "local " + token + ", " + Headers(token, HeadersUnary).Get("authorization"),
	})
	local := ClassifyError(500, string(echoed), token)
	for _, leak := range []string{"arbitrary", "secret", "Basic"} {
		if strings.Contains(local.Message, leak) {
			t.Fatalf("leaked %q in %q", leak, local.Message)
		}
	}
	for _, text := range []string{
		"failure devin-session-token$abc.def:ghi and more",
		"failure Basic some-unknown-secret-with-hyphens and more",
		`{"message":"failure Basic unknown\"embedded-token"}`,
		"failure Basic a-b-a-b",
	} {
		if got := ClassifyError(500, text, "").Message; got != "Devin upstream error" {
			t.Fatalf("text %q → %q", text, got)
		}
	}
	if got := ClassifyError(500, "ordinary failure", "").Message; got != "ordinary failure" {
		t.Fatal(got)
	}
}

func TestFreeModelProseReset(t *testing.T) {
	err := ClassifyError(0, `{"error":{"code":"unavailable","message":"Reached free model rate limit. Upgrade to Max for higher limits, or switch to a different model. Your limit will reset in 2 hours 37 minutes. (trace ID: 5c0d)"}}`, "")
	if err.Kind != KindRateLimit {
		t.Fatalf("kind %v", err.Kind)
	}
	ms := time.Until(err.ResetsAt)
	if ms <= (2*60+36)*time.Minute || ms > (2*60+37)*time.Minute {
		t.Fatalf("resetsAt %v", ms)
	}
}

func TestResetHints(t *testing.T) {
	cases := []struct {
		text string
		want time.Duration
	}{
		{"Your limit will reset in 45 minutes.", 45 * time.Minute},
		{"Your limit will reset in 45 seconds.", 45 * time.Second},
		{"limit resets in 1 hour and 5 minutes", 65 * time.Minute},
		{"resets in 2 days", 2 * 24 * time.Hour},
		{"Rate limit reached. Resets in: 3h0m0s", 3 * time.Hour},
	}
	for _, c := range cases {
		err := ClassifyError(429, c.text, "")
		got := time.Until(err.ResetsAt)
		if got <= c.want-5*time.Second || got > c.want {
			t.Fatalf("%q → %v (want %v)", c.text, got, c.want)
		}
	}
}

func TestFreeModelLimitTrailerIsModelScopedRateLimit(t *testing.T) {
	err := ClassifyError(0, `{"error":{"code":"unavailable","message":"Reached free model rate limit. Your limit will reset in 45 seconds."}}`, "")
	if err.Kind != KindRateLimit || !ModelScoped(err) {
		t.Fatalf("trailer classification = %+v", err)
	}
	if remaining := time.Until(err.ResetsAt); remaining <= 40*time.Second || remaining > 45*time.Second {
		t.Fatalf("reset delay = %v, want about 45s", remaining)
	}
}

func TestClassifyKinds(t *testing.T) {
	cases := []struct {
		status   int
		text     string
		kind     ErrorKind
		surfaced int
	}{
		{401, "Reached message rate limit for this model. Please try again later. Resets in: 3h0m0s", KindRateLimit, 429},
		{429, `{"code":"resource_exhausted","message":"try again later"}`, KindRateLimit, 429},
		{401, `{"code":"unavailable","message":"high demand, try again later"}`, KindCapacity, 503},
		{403, "an internal error occurred (trace ID: abc)", KindInternal, 502},
		{403, `{"code":"permission_denied","message":"blocked by our content policy"}`, KindContentPolicy, 400},
		{403, "insufficient credit / quota exceeded", KindQuota, 429},
		{500, "Your daily usage quota has been exhausted. Visit https://app.devin.ai/settings/usage to purchase on-demand usage or turn on auto-reload. (trace ID: bbdd07ca49202cec286b54dbb6f602a8)", KindQuota, 429},
		{500, `{"code":"internal","message":"Your daily usage quota has been exhausted"}`, KindQuota, 429},
		{500, "monthly usage limit reached", KindQuota, 429},
		{401, "Please visit /upgrade to access this model", KindModelBlocked, 403},
		{403, "Upgrade to Pro; this model requires paid access", KindModelBlocked, 403},
		{403, `{"code":"permission_denied","message":"not allowed"}`, KindAuth, 401},
		{401, `{"code":"unauthenticated","message":"invalid token"}`, KindAuth, 401},
		{500, "something went wrong", KindOther, 502},
	}
	for _, c := range cases {
		err := ClassifyError(c.status, c.text, "")
		if err.Kind != c.kind || err.Status != c.surfaced {
			t.Fatalf("status %d %q → %v/%d (want %v/%d)", c.status, c.text, err.Kind, err.Status, c.kind, c.surfaced)
		}
	}
}
