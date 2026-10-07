package chatgptweb

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	"github.com/google/uuid"
	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/wire"
)

func TestComposerReplacementUsesKeyboardEvents(t *testing.T) {
	source := string(mustReadChatSource(t))
	start := strings.Index(source, "func insertPromptAndVerify")
	if start < 0 {
		t.Fatal("composer replacement function was not found")
	}
	end := strings.Index(source[start:], "func clickSendButton")
	if end < 0 {
		t.Fatal("composer replacement function boundary was not found")
	}
	composer := source[start : start+end]
	if strings.Contains(composer, "replaceChildren()") {
		t.Fatal("composer replacement must not remove React-managed editor nodes")
	}
	for _, required := range []string{"KeyA", "input.DispatchKeyEventTypeKeyUp", "input.InsertText", "normalize('NFC')"} {
		if !strings.Contains(composer, required) {
			t.Fatalf("composer replacement must use %s", required)
		}
	}
}

func TestNavigatePageUsesAttachedTarget(t *testing.T) {
	if !strings.Contains(string(mustReadChatSource(t)), "cdp.Call(ctx, target, page.Navigate") {
		t.Fatal("Page.navigate must receive the attached chromedp target")
	}
}

func mustReadChatSource(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("chat.go")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestConversationSendURL(t *testing.T) {
	for _, tc := range []struct {
		url  string
		want bool
	}{
		{"https://chatgpt.com/backend-api/f/conversation", true},
		{"https://chatgpt.com/backend-api/conversation", true},
		{"https://chatgpt.com/backend-api/conversations", false},
		{"https://chatgpt.com/c/abc", false},
	} {
		if got := isConversationSendURL(tc.url); got != tc.want {
			t.Fatalf("isConversationSendURL(%q)=%t want %t", tc.url, got, tc.want)
		}
	}
}

func TestSendControlEnabled(t *testing.T) {
	for _, tc := range []struct {
		name            string
		found, disabled bool
		aria            string
		want            bool
	}{
		{name: "enabled", found: true, want: true},
		{name: "missing", found: false, want: false},
		{name: "disabled attribute", found: true, disabled: true, want: false},
		{name: "aria disabled", found: true, aria: "true", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sendControlEnabled(tc.found, tc.disabled, tc.aria); got != tc.want {
				t.Fatalf("send enabled=%t, want %t", got, tc.want)
			}
		})
	}
}

func TestChatWorkspaceDecision(t *testing.T) {
	if click := shouldClickChatWorkspace(true, false); click {
		t.Fatal("must not click Chat when the composer is already ready")
	}
	if click := shouldClickChatWorkspace(false, true); !click {
		t.Fatal("must click visible Chat control when composer is not ready")
	}
	if click := shouldClickChatWorkspace(false, false); click {
		t.Fatal("must not claim a click when neither composer nor control exists")
	}
}

func TestFailedWindowRetentionPolicy(t *testing.T) {
	cases := []struct {
		name      string
		keep      bool
		succeeded bool
		want      bool
	}{
		{name: "retain failed diagnostic window", keep: true, want: true},
		{name: "close successful window", keep: true, succeeded: true},
		{name: "normal failure closes window", keep: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldRetainFailedWindow(tc.keep, tc.succeeded); got != tc.want {
				t.Fatalf("retention=%t, want %t", got, tc.want)
			}
		})
	}
}

func TestBrowserActionContextRetainsChromedpTarget(t *testing.T) {
	browserCtx, cancelBrowser := chromedp.NewContext(context.Background())
	defer cancelBrowser()
	tabCtx, cancelTab := chromedp.NewContext(browserCtx)
	defer cancelTab()
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	defer cancelRequest()

	actionCtx, cancelAction, stopRequestCancel := browserActionContext(requestCtx, tabCtx, time.Second)
	defer cancelAction()
	defer stopRequestCancel()
	if chromedp.FromContext(actionCtx) == nil {
		t.Fatal("browser action context lost chromedp context values")
	}
	cancelRequest()
	select {
	case <-actionCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("request cancellation did not cancel browser action context")
	}
}

func TestPollBrowserStateReportsDetachedTarget(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := pollBrowserState(ctx)
	if err == nil || err.Kind != KindBrowser {
		t.Fatalf("expected explicit browser inspection error, got %v", err)
	}
	if !strings.Contains(err.Message, "failed to inspect ChatGPT conversation") {
		t.Fatalf("expected inspection failure detail, got %q", err.Message)
	}
}

func makeConversationRequest(model, text, messageID string) conversationRequest {
	return conversationRequest{
		Model:          model,
		Action:         "next",
		ConversationID: "conversation-id",
		Messages: []struct {
			ID     string `json:"id"`
			Author struct {
				Role string `json:"role"`
			} `json:"author"`
			Content struct {
				Parts []any `json:"parts"`
			} `json:"content"`
		}{{ID: messageID, Author: struct {
			Role string `json:"role"`
		}{Role: "user"}, Content: struct {
			Parts []any `json:"parts"`
		}{Parts: []any{text}}}},
	}
}

func TestSelectTurnAssistantTextRequiresTerminalDescendant(t *testing.T) {
	parent := "user-node"
	projection := backendProjection{Nodes: map[string]backendNode{
		"user-node": {ID: "user-node", Role: "user", Children: []string{"draft"}},
		"draft":     {ID: "draft", Role: "assistant", Parent: &parent, Children: []string{"final"}, EndTurn: false, ContentType: "text", Text: "in progress"},
		"final":     {ID: "final", Role: "assistant", Parent: &parent, EndTurn: true, ContentType: "text", Text: "verified answer", CreateTime: 2},
		"stale":     {ID: "stale", Role: "assistant", EndTurn: true, ContentType: "reasoning_recap", Text: ""},
	}}
	if text, ok := selectTurnAssistantText(projection, "user-node"); !ok || text != "verified answer" {
		t.Fatalf("terminal correlated text = %q, %t", text, ok)
	}
	if _, ok := selectTurnAssistantText(projection, "missing"); ok {
		t.Fatal("unknown user ID must not match")
	}
}

func TestTurnSelectionStopsAtLaterUserAndUsesParentGraph(t *testing.T) {
	user, later := "user", "later"
	projection := backendProjection{Nodes: map[string]backendNode{
		user:    {ID: user, Role: "user", Children: []string{later}},
		later:   {ID: later, Role: "user", Parent: &user, Children: []string{"wrong"}},
		"wrong": {Role: "assistant", Parent: &later, EndTurn: true, ContentType: "text", Text: "later answer", CreateTime: 3},
		"right": {Role: "assistant", Parent: &user, EndTurn: true, ContentType: "text", Text: "this answer", CreateTime: 2},
	}}
	if text, ok := selectTurnAssistantText(projection, user); !ok || text != "this answer" {
		t.Fatalf("selected %q, %t; must not cross another user", text, ok)
	}
}

func TestConversationEvidenceFiltersUnrelatedPostsAndCapturesTurnID(t *testing.T) {
	evidence := newConversationEvidence("question")
	request := network.RequestID("chat-turn")
	evidence.recordRequest(request, conversationRequest{Model: "gpt-5-6", Action: "next", ConversationID: "conversation-id", Messages: []struct {
		ID     string `json:"id"`
		Author struct {
			Role string `json:"role"`
		} `json:"author"`
		Content struct {
			Parts []any `json:"parts"`
		} `json:"content"`
	}{{ID: "wrong", Author: struct {
		Role string `json:"role"`
	}{Role: "user"}, Content: struct {
		Parts []any `json:"parts"`
	}{Parts: []any{"different prompt"}}}}})
	if seen, _, _, _, _ := evidence.snapshot(); seen {
		t.Fatal("unrelated prompt must not match the armed send")
	}
	evidence.recordRequest(request, makeConversationRequest("gpt-5-6", "question", "c6195f25-3d15-45d8-8ef4-78e7a25c9fbf"))
	if conversationID, userID := evidence.turnID(); conversationID != "conversation-id" || userID != "c6195f25-3d15-45d8-8ef4-78e7a25c9fbf" {
		t.Fatalf("captured turn identity = %q/%q", conversationID, userID)
	}
	_, capturedID := evidence.turnID()
	if _, err := uuid.Parse(capturedID); err != nil {
		t.Fatalf("captured ID must be a UUID: %v", err)
	}
}

func TestConversationEvidenceCorrelatesRequests(t *testing.T) {
	evidence := newConversationEvidence("question")
	request := network.RequestID("chat-turn")
	unrelated := network.RequestID("background")
	evidence.recordRequest(request, makeConversationRequest("gpt-6", "question", "25e4c874-5c09-46c9-a287-92e474b37765"))
	evidence.recordRequest(unrelated, conversationRequest{})
	evidence.recordResponse(unrelated, http.StatusOK)
	seen, model, responded, status, failure := evidence.snapshot()
	if !seen || model != "gpt-6" || responded || status != 0 || failure != "" {
		t.Fatalf("unrelated response must not confirm conversation request: %t %q %t %d %q", seen, model, responded, status, failure)
	}
	evidence.recordResponse(request, http.StatusAccepted)
	seen, model, responded, status, failure = evidence.snapshot()
	if !seen || model != "gpt-6" || !responded || status != http.StatusAccepted || failure != "" {
		t.Fatalf("conversation response was not correlated: %t %q %t %d %q", seen, model, responded, status, failure)
	}
	evidence.recordFailure(request, "net::ERR_BLOCKED_BY_CONTENT_BLOCKER")
	seen, model, responded, status, failure = evidence.snapshot()
	if !seen || model != "gpt-6" || !responded || status != http.StatusAccepted || failure != "net::ERR_BLOCKED_BY_CONTENT_BLOCKER" {
		t.Fatalf("conversation failure was not correlated: %t %q %t %d %q", seen, model, responded, status, failure)
	}
}

func TestSendConfirmationWaitsForRequestEvidence(t *testing.T) {
	evidence := newConversationEvidence("question")
	request := network.RequestID("late-request")
	evidence.recordResponse(request, http.StatusOK)
	evidence.recordFailure(request, "")
	if seen, _, _, _, _ := evidence.snapshot(); seen {
		t.Fatal("response alone must not confirm a conversation request")
	}
	evidence.recordRequest(request, makeConversationRequest("gpt-5-6", "question", "c6195f25-3d15-45d8-8ef4-78e7a25c9fbf"))
	seen, model, responded, status, _ := evidence.snapshot()
	if !seen || model != "gpt-5-6" || !responded || status != http.StatusOK {
		t.Fatalf("late request event did not complete evidence: %t %q %t %d", seen, model, responded, status)
	}
}

func TestConversationEvidenceHandlesResponseBeforeRequest(t *testing.T) {
	evidence := newConversationEvidence("")
	request := network.RequestID("reordered-events")
	evidence.recordResponse(request, http.StatusOK)
	evidence.recordFailure(request, "net::ERR_TEST")
	evidence.recordRequest(request, makeConversationRequest("gpt-5-6", "question", "c6195f25-3d15-45d8-8ef4-78e7a25c9fbf"))
	seen, model, responded, status, failure := evidence.snapshot()
	if !seen || model != "gpt-5-6" || !responded || status != http.StatusOK || failure != "net::ERR_TEST" {
		t.Fatalf("out-of-order conversation events were not correlated: %t %q %t %d %q", seen, model, responded, status, failure)
	}
}

func TestPublicContractAndDefaults(t *testing.T) {
	if DefaultBaseURL != "http://127.0.0.1:9222" {
		t.Fatalf("expected DefaultBaseURL %q, got %q", "http://127.0.0.1:9222", DefaultBaseURL)
	}

	p := NewProvider(config.Provider{}, nil)
	if p == nil {
		t.Fatal("NewProvider returned nil")
	}
	if p.BaseURL != DefaultBaseURL {
		t.Fatalf("expected BaseURL %q, got %q", DefaultBaseURL, p.BaseURL)
	}

	// Verify Error implements error
	var err error = &Error{
		Status:  http.StatusUnauthorized,
		Kind:    KindAuth,
		Message: "session expired",
	}
	if !strings.Contains(err.Error(), "[auth] session expired") {
		t.Fatalf("unexpected error string: %s", err.Error())
	}

	// Verify Kind constants
	kinds := []Kind{KindAuth, KindModel, KindRateLimit, KindInvalid, KindBrowser, KindTimeout}
	if len(kinds) != 6 {
		t.Fatalf("unexpected count of Kind constants: %d", len(kinds))
	}
}

func TestValidateAndNormalizeEndpoint(t *testing.T) {
	validCases := []struct {
		input    string
		expected string
	}{
		{"", "127.0.0.1:9222"},
		{"http://127.0.0.1:9222", "127.0.0.1:9222"},
		{"http://localhost:9222", "127.0.0.1:9222"},
		{"127.0.0.1:9222", "127.0.0.1:9222"},
		{"localhost:9222", "127.0.0.1:9222"},
		{"http://127.0.0.1:9223/", "127.0.0.1:9223"},
	}

	for _, tc := range validCases {
		got, err := ValidateAndNormalizeEndpoint(tc.input)
		if err != nil {
			t.Fatalf("input %q unexpected error: %v", tc.input, err)
		}
		if got != tc.expected {
			t.Fatalf("input %q: expected %q, got %q", tc.input, tc.expected, got)
		}
	}

	invalidCases := []string{
		"http://192.168.1.100:9222",
		"http://api.openai.com/v1",
		"http://example.com:9222",
		"http://10.0.0.1:9222",
	}

	for _, inv := range invalidCases {
		_, err := ValidateAndNormalizeEndpoint(inv)
		if err == nil {
			t.Fatalf("expected error for non-loopback endpoint %q, but got nil", inv)
		}
		if err.Kind != KindInvalid {
			t.Fatalf("expected KindInvalid for %q, got %s", inv, err.Kind)
		}
	}
}

func TestPreflightChrome(t *testing.T) {
	// 1. Success mock server
	tsValid := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/json/version" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(VersionInfo{
				Browser:              "Chrome/130.0.0.0",
				WebSocketDebuggerURL: "ws://127.0.0.1:9222/devtools/browser/abc",
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer tsValid.Close()

	uValid := strings.TrimPrefix(tsValid.URL, "http://")
	ver, err := PreflightChrome(context.Background(), tsValid.Client(), uValid)
	if err != nil {
		t.Fatalf("unexpected preflight error: %v", err)
	}
	if ver.Browser != "Chrome/130.0.0.0" {
		t.Fatalf("expected Chrome browser, got %s", ver.Browser)
	}

	// 2. Non-Chrome 404 server
	ts404 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer ts404.Close()

	u404 := strings.TrimPrefix(ts404.URL, "http://")
	_, err = PreflightChrome(context.Background(), ts404.Client(), u404)
	if err == nil {
		t.Fatal("expected preflight error for 404 endpoint, got nil")
	}
	if err.Kind != KindBrowser {
		t.Fatalf("expected KindBrowser, got %s", err.Kind)
	}

	// 3. Third-party non-Chrome endpoint (missing required fields)
	tsNonChrome := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer tsNonChrome.Close()

	uNonChrome := strings.TrimPrefix(tsNonChrome.URL, "http://")
	_, err = PreflightChrome(context.Background(), tsNonChrome.Client(), uNonChrome)
	if err == nil {
		t.Fatal("expected preflight error for non-Chrome endpoint, got nil")
	}
	if err.Kind != KindBrowser {
		t.Fatalf("expected KindBrowser, got %s", err.Kind)
	}
}

func TestEndpointGateSerialization(t *testing.T) {
	gate := newEndpointGate(4)

	// Acquire first lock
	ctx := context.Background()
	release1, err := gate.acquire(ctx)
	if err != nil {
		t.Fatalf("acquire 1 failed: %v", err)
	}

	acquiredSecond := make(chan bool)
	go func() {
		release2, err2 := gate.acquire(ctx)
		if err2 != nil {
			acquiredSecond <- false
			return
		}
		defer release2()
		acquiredSecond <- true
	}()

	// Verify second acquire blocks while first is held
	select {
	case <-acquiredSecond:
		t.Fatal("second acquire should have blocked")
	case <-time.After(50 * time.Millisecond):
		// Expected to block
	}

	// Release first lock
	release1()

	// Now second acquire should unblock
	select {
	case ok := <-acquiredSecond:
		if !ok {
			t.Fatal("second acquire failed")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("second acquire timed out after release")
	}
}

func TestEndpointGateQueueCapacity(t *testing.T) {
	gate := newEndpointGate(1) // only 1 waiter allowed

	ctx := context.Background()
	rel, err := gate.acquire(ctx)
	if err != nil {
		t.Fatalf("acquire failed: %v", err)
	}
	defer rel()

	// Start 1 waiter (takes the only waiting slot)
	go func() {
		r, _ := gate.acquire(ctx)
		if r != nil {
			r()
		}
	}()

	// Wait briefly for goroutine to increment waiting count
	time.Sleep(30 * time.Millisecond)

	// Second waiter exceeds capacity
	_, err = gate.acquire(ctx)
	if err == nil {
		t.Fatal("expected capacity error, got nil")
	}
	if err.Kind != KindRateLimit {
		t.Fatalf("expected KindRateLimit, got %s", err.Kind)
	}
}

func TestBuildTranscriptValidation(t *testing.T) {
	// 1. Tool-calling rejection
	bodyWithTools := wire.Body{
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
		"tools": []any{
			map[string]any{"type": "function"},
		},
	}
	_, err := BuildTranscript(bodyWithTools)
	if err == nil || err.Kind != KindInvalid {
		t.Fatalf("expected KindInvalid for tools, got %v", err)
	}

	// 2. Tool choice rejection
	bodyWithChoice := wire.Body{
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
		"tool_choice": "auto",
	}
	_, err = BuildTranscript(bodyWithChoice)
	if err == nil || err.Kind != KindInvalid {
		t.Fatalf("expected KindInvalid for tool_choice, got %v", err)
	}

	// 3. Multimodal image rejection
	bodyWithImage := wire.Body{
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": "look at this"},
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,..."}},
				},
			},
		},
	}
	_, err = BuildTranscript(bodyWithImage)
	if err == nil || err.Kind != KindInvalid {
		t.Fatalf("expected KindInvalid for image content, got %v", err)
	}

	// 4. Oversized payload rejection
	hugeText := strings.Repeat("A", MaxTranscriptBytes+10)
	bodyHuge := wire.Body{
		"messages": []any{
			map[string]any{"role": "user", "content": hugeText},
		},
	}
	_, err = BuildTranscript(bodyHuge)
	if err == nil || err.Kind != KindInvalid {
		t.Fatalf("expected KindInvalid for oversized payload, got %v", err)
	}

	// 5. Valid single user message
	bodySingle := wire.Body{
		"messages": []any{
			map[string]any{"role": "user", "content": "Hello world!"},
		},
	}
	transcript, err := BuildTranscript(bodySingle)
	if err != nil {
		t.Fatalf("unexpected error for single message: %v", err)
	}
	if transcript != "Hello world!" {
		t.Fatalf("expected %q, got %q", "Hello world!", transcript)
	}

	// 6. Valid multi-turn with system message
	bodyMulti := wire.Body{
		"messages": []any{
			map[string]any{"role": "system", "content": "You are a helpful coding assistant."},
			map[string]any{"role": "user", "content": "What is 2+2?"},
			map[string]any{"role": "assistant", "content": "4"},
			map[string]any{"role": "user", "content": "Add 3 to it."},
		},
	}
	transcript, err = BuildTranscript(bodyMulti)
	if err != nil {
		t.Fatalf("unexpected error for multi-turn: %v", err)
	}
	if !strings.Contains(transcript, "[System]: You are a helpful coding assistant.") {
		t.Fatalf("missing system header in transcript:\n%s", transcript)
	}
	if !strings.Contains(transcript, systemRoleWarning) {
		t.Fatalf("missing system role warning in transcript:\n%s", transcript)
	}
	if !strings.Contains(transcript, "[User]: What is 2+2?") || !strings.Contains(transcript, "[Assistant]: 4") {
		t.Fatalf("missing turns in transcript:\n%s", transcript)
	}
}

func TestModelSelectorUsesStrictExactConfirmation(t *testing.T) {
	if strings.Contains(selectModelJS, "confirmedText.includes") || strings.Contains(selectModelJS, "if (confirmedBtn)") {
		t.Fatal("model selector confirmation must require an existing button and exact match")
	}
	for _, required := range []string{"data-codex-intelligence-trigger", "data-model-picker-view-toggle", "data-model-picker-power-slider", "selectedVariant", "confirmedBySelectedItem"} {
		if !strings.Contains(selectModelJS, required) {
			t.Fatalf("model selector must handle current ChatGPT picker and verify %q", required)
		}
	}
	if !strings.Contains(selectModelJS, "confirmedByExactTitle") {
		t.Fatal("model selector must require exact model confirmation")
	}
}

func TestModelCatalogMatching(t *testing.T) {
	catalog := []Model{
		{ID: "gpt-5-4-thinking", Title: "GPT-5.4 Thinking"},
		{ID: "gpt-5-mini", Title: "GPT-5 Mini"},
		{ID: "auto", Title: "Auto"},
	}

	// Exact ID match
	m := FindModelInCatalog(catalog, "gpt-5-4-thinking")
	if m == nil || m.ID != "gpt-5-4-thinking" {
		t.Fatalf("expected gpt-5-4-thinking, got %v", m)
	}

	// Exact Title match
	m = FindModelInCatalog(catalog, "GPT-5 Mini")
	if m == nil || m.ID != "gpt-5-mini" {
		t.Fatalf("expected gpt-5-mini by title, got %v", m)
	}

	// Substring / fuzzy match must NOT match
	m = FindModelInCatalog(catalog, "thinking")
	if m != nil {
		t.Fatalf("fuzzy match should return nil, got %v", m)
	}

	m = FindModelInCatalog(catalog, "gpt-5")
	if m != nil {
		t.Fatalf("partial prefix match should return nil, got %v", m)
	}
}

// mockDriver implements BrowserDriver for tests without real Chrome instance.
type mockDriver struct {
	mu           sync.Mutex
	models       []Model
	modelsErr    *Error
	chatResult   ChatResult
	chatErr      error
	lastPrompt   string
	lastReqModel string
}

func (m *mockDriver) Models(ctx context.Context, cdpEndpoint string) ([]Model, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.modelsErr != nil {
		return nil, m.modelsErr
	}
	return m.models, nil
}

func (m *mockDriver) Chat(ctx context.Context, cdpEndpoint string, req ChatRequest, prompt string) (ChatResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastPrompt = prompt
	m.lastReqModel = req.Model
	if m.chatErr != nil {
		return m.chatResult, m.chatErr
	}
	return m.chatResult, nil
}

func TestProviderWithMockDriver(t *testing.T) {
	mock := &mockDriver{
		models: []Model{
			{ID: "gpt-5-4-thinking", Title: "GPT-5.4 Thinking"},
		},
		chatResult: ChatResult{
			Status: http.StatusOK,
			Completion: wire.Body{
				"id":     "chatcmpl-test",
				"object": "chat.completion",
				"model":  "gpt-5-4-thinking",
				"choices": []any{
					map[string]any{
						"index": 0,
						"message": map[string]any{
							"role":    "assistant",
							"content": "Hello from mock!",
						},
						"finish_reason": "stop",
					},
				},
			},
		},
	}

	p := NewProvider(config.Provider{BaseURL: DefaultBaseURL}, nil)
	p.driver = mock

	ctx := context.Background()

	// 1. Models call
	models, err := p.Models(ctx)
	if err != nil {
		t.Fatalf("Models failed: %v", err)
	}
	if len(models) != 1 || models[0].ID != "gpt-5-4-thinking" {
		t.Fatalf("unexpected models: %v", models)
	}

	// 2. Chat call (non-streaming)
	req := ChatRequest{
		Model: "gpt-5-4-thinking",
		Body: wire.Body{
			"messages": []any{
				map[string]any{"role": "user", "content": "ping"},
			},
		},
	}
	res, cErr := p.Chat(ctx, req)
	if cErr != nil {
		t.Fatalf("Chat failed: %v", cErr)
	}
	if res.Status != http.StatusOK {
		t.Fatalf("expected status 200, got %d", res.Status)
	}
	if res.Completion == nil {
		t.Fatal("expected non-nil Completion")
	}

	// Verify prompt was passed to driver
	if mock.lastPrompt != "ping" {
		t.Fatalf("expected lastPrompt %q, got %q", "ping", mock.lastPrompt)
	}
}

func TestStreamChunkFormatAndPrefixIntegrity(t *testing.T) {
	// Verify monotonic prefix check behavior
	accumulated := "The answer is 42."

	// Correct prefix
	validNext := "The answer is 42. And here is why:"
	if !strings.HasPrefix(validNext, accumulated) {
		t.Fatal("validNext should have accumulated as prefix")
	}
	delta := validNext[len(accumulated):]
	if delta != " And here is why:" {
		t.Fatalf("unexpected delta: %q", delta)
	}

	// Invalid rewrites include same-length replacements and truncation.
	for _, corruptedNext := range []string{"Answer is 42.", "The answer is 4"} {
		if len(corruptedNext) >= len(accumulated) && strings.HasPrefix(corruptedNext, accumulated) {
			t.Fatalf("rewritten output %q should fail prefix check", corruptedNext)
		}
	}

	// Verify SSE reader streamCloser cleanup
	cleanedUp := false
	lockReleased := false

	pr, pw := io.Pipe()
	closer := &streamCloser{
		ReadCloser: pr,
		cancel: func() {
			cleanedUp = true
		},
		releaseLock: func() {
			lockReleased = true
		},
	}

	go func() {
		_, _ = fmt.Fprintf(pw, "data: test\n\n")
		_ = pw.Close()
	}()

	buf := make([]byte, 1024)
	n, _ := closer.Read(buf)
	if !strings.Contains(string(buf[:n]), "data: test") {
		t.Fatalf("unexpected read: %s", string(buf[:n]))
	}

	_ = closer.Close()
	if !cleanedUp {
		t.Fatal("expected tab cleanup on Close")
	}
	if !lockReleased {
		t.Fatal("expected lock release on Close")
	}
}

func TestProviderStreamingMock(t *testing.T) {
	pr, pw := io.Pipe()

	mock := &mockDriver{
		chatResult: ChatResult{
			Status: http.StatusOK,
			Stream: pr,
		},
	}

	p := NewProvider(config.Provider{BaseURL: DefaultBaseURL}, nil)
	p.driver = mock

	go func() {
		chunk1 := `{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"Hel"},"finish_reason":null}]}`
		chunk2 := `{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":null}]}`
		chunk3 := `{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`

		_, _ = fmt.Fprintf(pw, "data: %s\n\n", chunk1)
		_, _ = fmt.Fprintf(pw, "data: %s\n\n", chunk2)
		_, _ = fmt.Fprintf(pw, "data: %s\n\n", chunk3)
		_, _ = fmt.Fprintf(pw, "data: [DONE]\n\n")
		_ = pw.Close()
	}()

	res, err := p.Chat(context.Background(), ChatRequest{
		Model:  "gpt-5-4-thinking",
		Stream: true,
		Body: wire.Body{
			"messages": []any{
				map[string]any{"role": "user", "content": "hi"},
			},
		},
	})
	if err != nil {
		t.Fatalf("unexpected Chat error: %v", err)
	}
	if res.Stream == nil {
		t.Fatal("expected non-nil Stream")
	}
	defer res.Stream.Close()

	bodyBytes, err := io.ReadAll(res.Stream)
	if err != nil {
		t.Fatalf("failed to read stream: %v", err)
	}

	bodyStr := string(bodyBytes)
	if !strings.Contains(bodyStr, "chat.completion.chunk") {
		t.Fatalf("missing chunk in stream: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, "data: [DONE]\n\n") {
		t.Fatalf("missing [DONE] in stream: %s", bodyStr)
	}
	// Verify unknown token usage is absent (not fabricated)
	if strings.Contains(bodyStr, `"usage"`) {
		t.Fatalf("stream should not fabricate usage: %s", bodyStr)
	}
}

func TestModelValidationAndAutoExpose(t *testing.T) {
	catalogWithoutAuto := []Model{
		{ID: "gpt-5-4-thinking", Title: "GPT-5.4 Thinking"},
		{ID: "gpt-5-mini", Title: "GPT-5 Mini"},
	}

	// Requesting auto when account does not expose auto
	var foundAuto *Model
	for i := range catalogWithoutAuto {
		if strings.EqualFold(catalogWithoutAuto[i].ID, "auto") {
			foundAuto = &catalogWithoutAuto[i]
			break
		}
	}
	if foundAuto != nil {
		t.Fatal("should not find auto in catalogWithoutAuto")
	}

	// Requesting unavailable model
	targetModel := "gpt-4-turbo"
	targetCatalogModel := FindModelInCatalog(catalogWithoutAuto, targetModel)
	if targetCatalogModel != nil {
		t.Fatalf("model %q should be unavailable", targetModel)
	}
}

func TestFreshChatURLUsesModelAuto(t *testing.T) {
	src := string(mustReadChatSource(t))
	if !strings.Contains(src, `navURL := "https://chatgpt.com/?model=auto"`) {
		t.Fatalf("fresh chat navigation must use ?model=auto to guarantee composer hydration")
	}
}

func TestMouseEventSequenceMatchesReference(t *testing.T) {
	src := string(mustReadChatSource(t))
	start := strings.Index(src, "func clickSendButton")
	if start < 0 {
		t.Fatal("clickSendButton function not found")
	}
	sendFn := src[start:]
	if end := strings.Index(sendFn, "func jsonStringLiteral"); end > 0 {
		sendFn = sendFn[:end]
	}
	for _, expectedEvt := range []string{"pointerdown", "mousedown", "pointerup", "mouseup", "click"} {
		if !strings.Contains(sendFn, expectedEvt) {
			t.Fatalf("clickSendButton must dispatch mouse event %q", expectedEvt)
		}
	}
	if !strings.Contains(sendFn, "bubbles: true") || !strings.Contains(sendFn, "cancelable: true") {
		t.Fatal("clickSendButton mouse events must be bubbles and cancelable")
	}
	if !strings.Contains(sendFn, "time.Now().Add(10 * time.Second)") {
		t.Fatal("clickSendButton must wait up to 10s for send button readiness")
	}
}

func TestDetectorBudgetsModelClassification(t *testing.T) {
	reasoningCases := []string{
		"gpt-5-4-thinking",
		"o1-preview",
		"o3-mini",
		"o4-high",
		"deep-research",
		"model-reasoning",
		"gpt-5-t-mini",
	}
	for _, model := range reasoningCases {
		first, idle := detectorBudgets(model)
		if first != FirstAnswerTimeout || idle != IdleTimeout {
			t.Fatalf("model %q should get reasoning budgets (300s, 120s), got (%v, %v)", model, first, idle)
		}
	}

	defaultCases := []string{
		"gpt-4o",
		"gpt-4o-mini",
		"auto",
		"",
	}
	for _, model := range defaultCases {
		first, idle := detectorBudgets(model)
		if first != 90*time.Second || idle != 90*time.Second {
			t.Fatalf("model %q should get default budgets (90s, 90s), got (%v, %v)", model, first, idle)
		}
	}
}

func TestBackendTransportFailedDistinguishesNotReady(t *testing.T) {
	transportErr := &Error{
		Status:  http.StatusBadGateway,
		Kind:    KindBrowser,
		Message: "ChatGPT conversation fetch transport failed: net::ERR_CONNECTION_RESET",
	}
	if !backendTransportFailed(transportErr) {
		t.Fatal("expected transport failure to return true")
	}

	authErr := &Error{
		Status:  http.StatusUnauthorized,
		Kind:    KindAuth,
		Message: "ChatGPT conversation fetch returned HTTP 401",
	}
	if backendTransportFailed(authErr) {
		t.Fatal("auth error must not be classified as backend transport failure")
	}

	notReadyErr := &Error{
		Status:  http.StatusGatewayTimeout,
		Kind:    KindTimeout,
		Message: "ChatGPT turn did not reconcile within observation budget",
	}
	if backendTransportFailed(notReadyErr) {
		t.Fatal("timeout/not-ready must not be classified as backend transport failure")
	}
}

func TestConversationProjectionDoesNotDoubleEscapeJS(t *testing.T) {
	src := string(mustReadChatSource(t))
	start := strings.Index(src, "func fetchTurnProjection")
	if start < 0 {
		t.Fatal("fetchTurnProjection not found")
	}
	fn := src[start:]
	if end := strings.Index(fn, "func waitForConversationID"); end > 0 {
		fn = fn[:end]
	}
	if strings.Contains(fn, `\\n`) {
		t.Fatal("fetchTurnProjection Go raw string must not contain double-escaped \\\\n")
	}
	if !strings.Contains(fn, `.join('\n')`) {
		t.Fatal("fetchTurnProjection must join text parts with single newline '\\n'")
	}
}
