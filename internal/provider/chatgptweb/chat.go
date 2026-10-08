package chatgptweb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/google/uuid"
	"github.com/xinyao27/jevonian/internal/wire"
)

const (
	BrowserActionTimeout = 45 * time.Second
	FirstAnswerTimeout   = 300 * time.Second
	TotalTurnTimeout     = 900 * time.Second
	IdleTimeout          = 120 * time.Second
	PollInterval         = 300 * time.Millisecond
)

type backendNode struct {
	ID          string   `json:"id"`
	Parent      *string  `json:"parent"`
	Children    []string `json:"children"`
	Role        string   `json:"role"`
	CreateTime  float64  `json:"create_time"`
	EndTurn     bool     `json:"end_turn"`
	ContentType string   `json:"content_type"`
	Text        string   `json:"text"`
}

type backendProjection struct {
	Nodes       map[string]backendNode `json:"nodes"`
	CurrentNode string                 `json:"current_node"`
}

type baselineCounts struct {
	UserCount int `json:"userCount"`
	AsstCount int `json:"asstCount"`
}

type pollState struct {
	Status     string `json:"status"`
	Message    string `json:"message,omitempty"`
	AsstCount  int    `json:"asstCount"`
	Text       string `json:"text"`
	HTMLLen    int    `json:"htmlLen"`
	Children   int    `json:"children"`
	HasAction  bool   `json:"hasAction"`
	IsThinking bool   `json:"isThinking"`
	HasStop    bool   `json:"hasStop"`
}

type conversationRequest struct {
	Model          string `json:"model"`
	Action         string `json:"action"`
	ConversationID string `json:"conversation_id"`
	Messages       []struct {
		ID     string `json:"id"`
		Author struct {
			Role string `json:"role"`
		} `json:"author"`
		Content struct {
			Parts []any `json:"parts"`
		} `json:"content"`
	} `json:"messages"`
}

type conversationEvidence struct {
	mu           sync.Mutex
	expectedText string
	requests     map[network.RequestID]conversationRequest
	responseIDs  map[network.RequestID]int
	pendingCode  map[network.RequestID]int
	failures     map[network.RequestID]string
	pendingFail  map[network.RequestID]string
}

func isConversationSendURL(raw string) bool {
	path := raw
	if parsed, err := url.Parse(raw); err == nil {
		path = parsed.Path
	}
	path = strings.TrimRight(path, "/")
	return strings.HasSuffix(path, "/backend-api/f/conversation") || strings.HasSuffix(path, "/backend-api/conversation")
}

func validMessageID(id string) bool {
	parsed, err := uuid.Parse(id)
	return err == nil && parsed.Version() == 4
}

func newConversationEvidence(expectedText string) *conversationEvidence {
	return &conversationEvidence{
		expectedText: expectedText,
		requests:     make(map[network.RequestID]conversationRequest),
		responseIDs:  make(map[network.RequestID]int),
		pendingCode:  make(map[network.RequestID]int),
		failures:     make(map[network.RequestID]string),
		pendingFail:  make(map[network.RequestID]string),
	}
}

func (e *conversationEvidence) recordRequest(id network.RequestID, request conversationRequest) {
	if request.Model == "" || request.Action != "next" || len(request.Messages) == 0 || request.Messages[0].Author.Role != "user" {
		return
	}
	if id == "" {
		return
	}
	parts := request.Messages[0].Content.Parts
	textParts := make([]string, 0, len(parts))
	for _, part := range parts {
		if text, ok := part.(string); ok {
			textParts = append(textParts, text)
		}
	}
	if e.expectedText != "" && strings.Join(textParts, "\n") != e.expectedText {
		return
	}
	if messageID := request.Messages[0].ID; messageID == "" || !validMessageID(messageID) {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.requests[id] = request
	if code, ok := e.pendingCode[id]; ok {
		e.responseIDs[id] = code
		delete(e.pendingCode, id)
	}
	if message, ok := e.pendingFail[id]; ok {
		e.failures[id] = message
		delete(e.pendingFail, id)
	}
}

func (e *conversationEvidence) recordResponse(id network.RequestID, code int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.requests[id]; ok {
		e.responseIDs[id] = code
	} else {
		e.pendingCode[id] = code
	}
}

func (e *conversationEvidence) recordFailure(id network.RequestID, message string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.requests[id]; ok {
		e.failures[id] = message
	} else if message != "" {
		e.pendingFail[id] = message
	}
}

func (e *conversationEvidence) snapshot() (bool, string, bool, int, string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.requests) != 1 {
		return false, "", false, 0, ""
	}
	for id, request := range e.requests {
		if request.Model == "" {
			continue
		}
		code, responded := e.responseIDs[id]
		return true, request.Model, responded, code, e.failures[id]
	}
	return false, "", false, 0, ""
}

func (e *conversationEvidence) turnID() (string, string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.requests) != 1 {
		return "", ""
	}
	for _, request := range e.requests {
		if len(request.Messages) == 0 {
			continue
		}
		return request.ConversationID, request.Messages[0].ID
	}
	return "", ""
}

func selectTurnAssistantText(projection backendProjection, userID string) (string, bool) {
	if userID == "" || len(projection.Nodes) == 0 {
		return "", false
	}
	user, ok := projection.Nodes[userID]
	if !ok || user.Role != "user" {
		for key, node := range projection.Nodes {
			if node.ID == userID && node.Role == "user" {
				userID, user, ok = key, node, true
				break
			}
		}
	}
	if !ok {
		return "", false
	}
	// Recover missing children edges from parent pointers. Never walk across
	// a later user turn, even when it is a descendant in the same graph.
	children := make(map[string][]string, len(projection.Nodes))
	for key, node := range projection.Nodes {
		children[key] = append(children[key], node.Children...)
		if node.Parent != nil {
			children[*node.Parent] = append(children[*node.Parent], key)
		}
	}
	queue := append([]string(nil), children[userID]...)
	_ = user
	visited := make(map[string]bool, len(projection.Nodes))
	var selected backendNode
	found := false
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if visited[id] {
			continue
		}
		visited[id] = true
		node, exists := projection.Nodes[id]
		if !exists || node.Role == "user" {
			continue
		}
		if node.Role == "assistant" && node.EndTurn && node.ContentType == "text" && strings.TrimSpace(node.Text) != "" {
			if !found || node.CreateTime > selected.CreateTime {
				selected, found = node, true
			}
		}
		queue = append(queue, children[id]...)
	}
	if found {
		return selected.Text, true
	}
	return "", false
}

func fetchTurnProjection(ctx context.Context, tabCtx context.Context, conversationID string) (backendProjection, *Error) {
	idJSON, _ := json.Marshal(conversationID)
	js := fmt.Sprintf(`(async () => {
		try {
			const session = await fetch('/api/auth/session', {credentials:'include'}).then(r => r.json());
			if (!session || !session.accessToken) return JSON.stringify({__status:401});
			const response = await fetch('/backend-api/conversation/' + encodeURIComponent(%s) + '?offset=0&limit=50', {
				headers: {Authorization:'Bearer ' + session.accessToken}
			});
			if (!response.ok) return JSON.stringify({__status:response.status});
			const data = await response.json();
			const mapping = data.mapping || {};
			const nodes = {};
			for (const [key, node] of Object.entries(mapping)) {
				const message = node.message || {};
				const content = message.content || {};
				const parts = content.parts || [];
				const text = content.content_type === 'text' ? parts.filter(part => typeof part === 'string' && part.trim()).join('\n') : '';
				nodes[key] = {id:message.id || key, parent:node.parent || null, children:node.children || [], role:(message.author || {}).role || 'unknown', create_time:message.create_time || 0, end_turn:!!message.end_turn, content_type:content.content_type || 'unknown', text};
			}
			return JSON.stringify({nodes,current_node:data.current_node || null});
		} catch (error) { return JSON.stringify({__error:String(error)}); }
	})()`, string(idJSON))
	raw, err := chromedp.Run(tabCtx, chromedp.Evaluate[string](js, chromedp.EvalAwaitPromise))
	if err != nil {
		return backendProjection{}, &Error{Status: http.StatusBadGateway, Kind: KindBrowser, Message: fmt.Sprintf("ChatGPT conversation fetch transport failed: %v", err)}
	}
	var envelope struct {
		Status int    `json:"__status"`
		Error  string `json:"__error"`
	}
	_ = json.Unmarshal([]byte(raw), &envelope)
	if envelope.Status != 0 {
		kind, status := KindBrowser, http.StatusBadGateway
		if envelope.Status == http.StatusUnauthorized {
			kind, status = KindAuth, http.StatusUnauthorized
		}
		return backendProjection{}, &Error{Status: status, Kind: kind, Message: fmt.Sprintf("ChatGPT conversation fetch returned HTTP %d", envelope.Status)}
	}
	if envelope.Error != "" {
		return backendProjection{}, &Error{Status: http.StatusBadGateway, Kind: KindBrowser, Message: "ChatGPT conversation fetch transport failed: " + envelope.Error}
	}
	var projection backendProjection
	if err := json.Unmarshal([]byte(raw), &projection); err != nil {
		return backendProjection{}, &Error{Status: http.StatusBadGateway, Kind: KindBrowser, Message: fmt.Sprintf("invalid ChatGPT conversation projection: %v", err)}
	}
	return projection, nil
}

func waitForConversationID(ctx context.Context, tabCtx context.Context) (string, error) {
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-deadline.C:
			return "", context.DeadlineExceeded
		case <-ticker.C:
			id, err := chromedp.Run(tabCtx, chromedp.Evaluate[string](`(() => { const match = location.pathname.match(/\/c\/([^/]+)/); return match ? decodeURIComponent(match[1]) : ''; })()`))
			if err == nil && isServerConversationID(id) {
				return id, nil
			}
		}
	}
}

// isServerConversationID reports whether id is a real server-side ChatGPT
// conversation UUID. ChatGPT first puts an optimistic client-only ID such as
// "local-chatgpt:<uuid>" into the URL. That temporary ID is rejected by
// /backend-api/conversation and must never be used for reconciliation.
// Server IDs are UUIDs (version 8 as of 2026), so any valid UUID that is not
// the local placeholder is accepted.
func isServerConversationID(id string) bool {
	if id == "" {
		return false
	}
	if strings.HasPrefix(strings.ToLower(id), "local-") {
		return false
	}
	_, err := uuid.Parse(id)
	return err == nil
}

func resolveTurnText(ctx context.Context, tabCtx context.Context, evidence *conversationEvidence) (string, bool, *Error) {
	conversationID, userID := evidence.turnID()
	if debugEnabled() {
		fmt.Printf("[DBG] resolveTurnText turnID convID=%q userID=%q\n", conversationID, userID)
	}
	if userID == "" {
		return "", false, nil
	}
	if !isServerConversationID(conversationID) {
		var err error
		conversationID, err = waitForConversationID(ctx, tabCtx)
		if err != nil {
			return "", false, &Error{Status: http.StatusGatewayTimeout, Kind: KindTimeout, Message: fmt.Sprintf("ChatGPT conversation ID did not appear after send: %v", err)}
		}
		if debugEnabled() {
			fmt.Printf("[DBG] resolveTurnText waited convID=%q\n", conversationID)
		}
	}
	projection, err := fetchTurnProjection(ctx, tabCtx, conversationID)
	if err != nil {
		if debugEnabled() {
			fmt.Printf("[DBG] fetchTurnProjection convID=%q err=%v\n", conversationID, err)
		}
		return "", false, err
	}
	text, ok := selectTurnAssistantText(projection, userID)
	return text, ok, nil
}

type checkPageResult struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// Models discovers available models for the authenticated ChatGPT session.
func (p *Provider) Models(ctx context.Context) ([]Model, error) {
	if p.driver != nil {
		return p.driver.Models(ctx, p.BaseURL)
	}

	hostPort, epErr := ValidateAndNormalizeEndpoint(p.BaseURL)
	if epErr != nil {
		return nil, epErr
	}

	if _, preErr := PreflightChrome(ctx, p.HTTPClient, hostPort); preErr != nil {
		return nil, preErr
	}

	gate := getEndpointGate(hostPort)
	releaseLock, lockErr := gate.acquire(ctx)
	if lockErr != nil {
		return nil, lockErr
	}
	defer releaseLock()

	session, sessErr := createTabSession(ctx, p.HTTPClient, hostPort)
	if sessErr != nil {
		return nil, sessErr
	}
	defer session.Close()

	if _, navErr := navigatePage(ctx, session.TabCtx, "https://chatgpt.com/"); navErr != nil {
		return nil, navErr
	}

	if err := waitForAuthenticatedPage(ctx, session.TabCtx); err != nil {
		return nil, err
	}

	models, err := FetchAccountModels(session.TabCtx)
	if err != nil {
		return nil, err
	}

	return models, nil
}

// Chat executes a conversation turn against ChatGPT Web.
func (p *Provider) Chat(ctx context.Context, req ChatRequest) (ChatResult, error) {
	prompt, bErr := BuildTranscript(req.Body)
	if bErr != nil {
		return ChatResult{Status: bErr.Status, Error: bErr}, bErr
	}

	if p.driver != nil {
		return p.driver.Chat(ctx, p.BaseURL, req, prompt)
	}

	hostPort, epErr := ValidateAndNormalizeEndpoint(p.BaseURL)
	if epErr != nil {
		return ChatResult{Status: epErr.Status, Error: epErr}, epErr
	}

	if _, preErr := PreflightChrome(ctx, p.HTTPClient, hostPort); preErr != nil {
		return ChatResult{Status: preErr.Status, Error: preErr}, preErr
	}

	gate := getEndpointGate(hostPort)
	releaseLock, lockErr := gate.acquire(ctx)
	if lockErr != nil {
		return ChatResult{Status: lockErr.Status, Error: lockErr}, lockErr
	}

	session, sessErr := createTabSession(ctx, p.HTTPClient, hostPort)
	if sessErr != nil {
		releaseLock()
		return ChatResult{Status: sessErr.Status, Error: sessErr}, sessErr
	}
	keepTargetOnFailure := req.KeepWindowOnFailure
	sessionOwned := true
	turnCompleted := false
	defer func() {
		if !sessionOwned {
			return
		}
		if shouldRetainFailedWindow(keepTargetOnFailure, turnCompleted) {
			session.Detach()
		} else {
			session.Close()
		}
		releaseLock()
	}()

	// Capture conversation request/response evidence without retaining prompt or token data.
	evidence := newConversationEvidence(prompt)
	maxPostDataSize := int64(1 << 20)
	networkCtx, cancelNetwork, stopNetworkCancel := browserActionContext(ctx, session.TabCtx, BrowserActionTimeout)
	_, networkErr := chromedp.Run(networkCtx, chromedp.Action[chromedp.Void](func(ctx context.Context, target *chromedp.Target) (chromedp.Void, error) {
		_, err := cdp.Call(ctx, target, network.Enable, network.EnableParams{MaxPostDataSize: &maxPostDataSize})
		return chromedp.Void{}, err
	}))
	cancelNetwork()
	stopNetworkCancel()
	if networkErr != nil {
		e := &Error{Status: http.StatusBadGateway, Kind: KindBrowser, Message: fmt.Sprintf("failed to enable ChatGPT network capture: %v", networkErr)}
		return ChatResult{Status: e.Status, Error: e}, e
	}
	requestEvents := chromedp.Events(session.TabCtx, network.RequestWillBeSent)
	responseEvents := chromedp.Events(session.TabCtx, network.ResponseReceived)
	failureEvents := chromedp.Events(session.TabCtx, network.LoadingFailed)
	go func() {
		for ev, err := range requestEvents {
			if err != nil {
				return
			}
			event := ev
			if event.Request == nil || !isConversationSendURL(event.Request.URL) || !strings.EqualFold(event.Request.Method, http.MethodPost) {
				continue
			}
			var bodyBytes []byte
			for _, entry := range event.Request.PostDataEntries {
				if len(entry.Bytes) > 0 {
					bodyBytes = append(bodyBytes, entry.Bytes...)
				}
			}
			if len(bodyBytes) == 0 && event.Request.HasPostData {
				res, callErr := chromedp.Call(session.TabCtx, network.GetRequestPostData, network.GetRequestPostDataParams{RequestID: event.RequestID})
				if callErr == nil {
					bodyBytes = res.PostData
				}
			}
			var payload conversationRequest
			if json.Unmarshal(bodyBytes, &payload) != nil {
				continue
			}
			evidence.recordRequest(event.RequestID, payload)
		}
	}()
	go func() {
		for ev, err := range responseEvents {
			if err != nil {
				return
			}
			event := ev
			if event.Response != nil && isConversationSendURL(event.Response.URL) {
				evidence.recordResponse(event.RequestID, int(event.Response.Status))
			}
		}
	}()
	go func() {
		for ev, err := range failureEvents {
			if err != nil {
				return
			}
			if strings.Contains(ev.ErrorText, "net::") {
				evidence.recordFailure(ev.RequestID, ev.ErrorText)
			}
		}
	}()

	// Navigate the fresh tab to ChatGPT home
	targetModel := strings.TrimSpace(req.Model)
	navURL := "https://chatgpt.com/?model=auto"
	navResult, navErr := navigatePage(ctx, session.TabCtx, navURL)
	if navErr != nil {
		return ChatResult{Status: navErr.Status, Error: navErr}, navErr
	}
	if err := waitForNavigationReady(ctx, session.TabCtx, navResult.LoaderID); err != nil {
		return ChatResult{Status: err.Status, Error: err}, err
	}

	// The Chat workspace initializes the active composer and model control.
	if err := ensureChatWorkspace(session.TabCtx); err != nil {
		return ChatResult{Status: err.Status, Error: err}, err
	}

	// Verify page state (login, challenge, composer presence)
	if err := waitForPageReady(ctx, session.TabCtx); err != nil {
		return ChatResult{Status: err.Status, Error: err}, err
	}

	// Model discovery & selection
	models, mErr := FetchAccountModels(session.TabCtx)
	if mErr != nil {
		return ChatResult{Status: mErr.Status, Error: mErr}, mErr
	}

	var targetCatalogModel *Model
	if targetModel == "" || targetModel == "auto" {
		for i := range models {
			if strings.EqualFold(models[i].ID, "auto") {
				targetCatalogModel = &models[i]
				break
			}
		}
		if targetCatalogModel == nil {
			e := &Error{
				Status:  http.StatusBadRequest,
				Kind:    KindModel,
				Message: "model 'auto' is not exposed by this ChatGPT account; an explicit model must be selected",
			}
			return ChatResult{Status: e.Status, Error: e}, e
		}
	} else {
		targetCatalogModel = FindModelInCatalog(models, targetModel)
		if targetCatalogModel == nil {
			e := &Error{
				Status:  http.StatusBadRequest,
				Kind:    KindModel,
				Message: fmt.Sprintf("requested model %q is not available in ChatGPT account", targetModel),
			}
			return ChatResult{Status: e.Status, Error: e}, e
		}
	}

	// Strict model selection & verification. The composer model picker lists
	// model families, not slugs, and its optimistic state can silently keep a
	// different lane. Navigating to the model-scoped URL is reliable because
	// ChatGPT resolves the query parameter into the exact family and lane.
	if selErr := SelectModelByNavigation(ctx, session.TabCtx, targetCatalogModel.ID); selErr != nil {
		return ChatResult{Status: selErr.Status, Error: selErr}, selErr
	}

	// Baseline message counts before submit
	baseline, bCountErr := getBaselineCounts(session.TabCtx)
	if bCountErr != nil {
		return ChatResult{Status: bCountErr.Status, Error: bCountErr}, bCountErr
	}

	// Safely pass text via JSON args; focus clear insert via CDP; verify exact composer content
	if insertErr := insertPromptAndVerify(session.TabCtx, prompt); insertErr != nil {
		return ChatResult{Status: insertErr.Status, Error: insertErr}, insertErr
	}

	// Click send button
	if clickErr := clickSendButton(ctx, session.TabCtx); clickErr != nil {
		return ChatResult{Status: clickErr.Status, Error: clickErr}, clickErr
	}

	// Verify the user message and the matching outbound request before polling for a reply.
	if accErr := verifySendAccepted(ctx, session.TabCtx, baseline.UserCount, evidence); accErr != nil {
		return ChatResult{Status: accErr.Status, Error: accErr}, accErr
	}

	// Handle Streaming vs Non-Streaming
	if req.Stream {
		pr, pw := io.Pipe()
		var keepTarget atomic.Bool
		stream := &streamCloser{
			ReadCloser: pr,
			cancel: func() {
				if keepTarget.Load() {
					session.Detach()
					return
				}
				session.BestEffortStop()
				session.Close()
			},
			stop:        func() { session.BestEffortStop() },
			releaseLock: releaseLock,
		}

		// The outbound request and its model are already confirmed by
		// verifySendAccepted. Record the correlation synchronously so evidence
		// is never subject to the streaming goroutine's timing.
		_, observed, _, _, _ := evidence.snapshot()
		RecordTurnEvidence(TurnEvidence{
			RequestedModel: targetCatalogModel.ID,
			ObservedModel:  observed,
			Correlated:     ModelMatchesRequested(targetCatalogModel.ID, observed),
			Streaming:      true,
		})

		go runStreamingTurn(ctx, session.TabCtx, pw, baseline.AsstCount, targetCatalogModel.ID, evidence, req.KeepWindowOnFailure, &keepTarget)
		sessionOwned = false

		return ChatResult{
			Status: http.StatusOK,
			Stream: stream,
		}, nil
	}

	// Non-streaming
	finalText, runErr := runNonStreamingTurn(ctx, session.TabCtx, baseline.AsstCount, evidence)
	if runErr != nil {
		if !keepTargetOnFailure {
			session.BestEffortStop()
		}
		return ChatResult{Status: runErr.Status, Error: runErr}, runErr
	}

	// Verify the model and that ChatGPT accepted the outbound conversation request.
	requestSeen, obs, responseSeen, responseCode, loadingFailure := evidence.snapshot()
	if !requestSeen {
		e := &Error{Status: http.StatusBadGateway, Kind: KindBrowser, Message: "assistant text appeared without an observed ChatGPT conversation POST"}
		return ChatResult{Status: e.Status, Error: e}, e
	}
	if !responseSeen || responseCode < 200 || responseCode >= 300 {
		e := &Error{Status: http.StatusBadGateway, Kind: KindBrowser, Message: fmt.Sprintf("ChatGPT conversation request failed: responseSeen=%t status=%d networkError=%q", responseSeen, responseCode, loadingFailure)}
		return ChatResult{Status: e.Status, Error: e}, e
	}
	if targetModel != "" && targetModel != "auto" && !ModelMatchesRequested(targetCatalogModel.ID, obs) {
		e := &Error{
			Status:  http.StatusBadRequest,
			Kind:    KindModel,
			Message: fmt.Sprintf("model mismatch: requested %q but outbound request used %q", targetCatalogModel.ID, obs),
		}
		return ChatResult{Status: e.Status, Error: e}, e
	}
	RecordTurnEvidence(TurnEvidence{
		RequestedModel: targetCatalogModel.ID,
		ObservedModel:  obs,
		Correlated:     ModelMatchesRequested(targetCatalogModel.ID, obs),
	})

	completion := wire.Body{
		"id":      fmt.Sprintf("chatcmpl-%s", uuid.NewString()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   targetCatalogModel.ID,
		"choices": []any{
			map[string]any{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": finalText,
				},
				"finish_reason": "stop",
			},
		},
	}

	turnCompleted = true
	return ChatResult{
		Status:     http.StatusOK,
		Completion: completion,
	}, nil
}

func sendControlEnabled(found, disabled bool, ariaDisabled string) bool {
	return found && !disabled && ariaDisabled != "true"
}

func shouldClickChatWorkspace(composerReady, chatControlVisible bool) bool {
	return !composerReady && chatControlVisible
}

func shouldRetainFailedWindow(keepOnFailure, succeeded bool) bool {
	return keepOnFailure && !succeeded
}

func debugEnabled() bool {
	return os.Getenv("JEVONIAN_CHATGPTWEB_DEBUG") != ""
}

func browserActionContext(requestCtx context.Context, tabCtx context.Context, timeout time.Duration) (context.Context, context.CancelFunc, func() bool) {
	actionCtx, cancel := context.WithTimeout(tabCtx, timeout)
	stopRequestCancel := context.AfterFunc(requestCtx, cancel)
	return actionCtx, cancel, stopRequestCancel
}

func navigatePage(ctx context.Context, tabCtx context.Context, url string) (page.NavigateResult, *Error) {
	actionCtx, cancel, stopRequestCancel := browserActionContext(ctx, tabCtx, BrowserActionTimeout)
	defer cancel()
	defer stopRequestCancel()
	result, err := chromedp.Run(actionCtx, chromedp.Action[page.NavigateResult](func(ctx context.Context, target *chromedp.Target) (page.NavigateResult, error) {
		return cdp.Call(ctx, target, page.Navigate, page.NavigateParams{URL: url})
	}))
	if err != nil {
		kind := KindBrowser
		status := http.StatusBadGateway
		if errors.Is(actionCtx.Err(), context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			kind = KindTimeout
			status = http.StatusGatewayTimeout
		}
		return page.NavigateResult{}, &Error{Status: status, Kind: kind, Message: fmt.Sprintf("ChatGPT navigation command failed: %v", err)}
	}
	if result.ErrorText != "" {
		return result, &Error{Status: http.StatusBadGateway, Kind: KindBrowser, Message: "ChatGPT navigation failed: " + result.ErrorText}
	}
	if result.FrameID == "" {
		return result, &Error{Status: http.StatusBadGateway, Kind: KindBrowser, Message: "ChatGPT navigation returned no frame ID"}
	}
	return result, nil
}

func waitForNavigationReady(ctx context.Context, tabCtx context.Context, loaderID cdp.LoaderID) *Error {
	deadline := time.NewTimer(BrowserActionTimeout)
	defer deadline.Stop()
	for {
		select {
		case <-ctx.Done():
			return &Error{Status: 499, Kind: KindTimeout, Message: "request cancelled while waiting for ChatGPT navigation"}
		case <-deadline.C:
			return &Error{Status: http.StatusGatewayTimeout, Kind: KindTimeout, Message: "ChatGPT navigation committed but page did not become ready within 45s"}
		case <-time.After(250 * time.Millisecond):
		}
		ready, err := chromedp.Run(tabCtx, chromedp.Evaluate[bool](`document.readyState === 'complete' || !!document.querySelector('[contenteditable="true"], textarea')`))
		if err != nil {
			if ctx.Err() != nil {
				return &Error{Status: 499, Kind: KindTimeout, Message: "request cancelled while checking ChatGPT page readiness"}
			}
			continue
		}
		if ready {
			return nil
		}
		if loaderID == "" {
			return &Error{Status: http.StatusBadGateway, Kind: KindBrowser, Message: "ChatGPT navigation returned no loader ID and page is not ready"}
		}
	}
}

func waitForAuthenticatedPage(ctx context.Context, tabCtx context.Context) *Error {
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case <-ctx.Done():
			return &Error{Status: 499, Kind: KindTimeout, Message: "request cancelled while waiting for ChatGPT login"}
		case <-deadline.C:
			return &Error{Status: http.StatusGatewayTimeout, Kind: KindTimeout, Message: "timed out waiting for authenticated ChatGPT page"}
		case <-time.After(300 * time.Millisecond):
		}
		models, err := FetchAccountModels(tabCtx)
		if err == nil && len(models) > 0 {
			return nil
		}
		if err != nil && err.Kind != KindAuth && err.Kind != KindBrowser {
			return err
		}
	}
}

func ensureChatWorkspace(tabCtx context.Context) *Error {
	js := `(() => {
		const chat = [...document.querySelectorAll('button')].find(button =>
			button.offsetParent !== null && (button.innerText || '').trim() === 'Chat'
		);
		const composer = document.querySelector('div#prompt-textarea[role="textbox"]')
			|| document.querySelector('div.ProseMirror[role="textbox"][data-codex-composer]')
			|| document.querySelector('div[contenteditable="true"]#prompt-textarea')
			|| document.querySelector('[contenteditable="true"][aria-label="Ask ChatGPT"]')
			|| document.querySelector('textarea#prompt-textarea');
		const composerReady = !!composer;
		const chatControlVisible = !!chat;
		const clickChat = !composerReady && chatControlVisible;
		if (!composerReady && !chatControlVisible) return JSON.stringify({ok:false, changed:false, reason:'Chat workspace control and composer were not found'});
		if (clickChat) chat.click();
		return JSON.stringify({ok:true, changed:clickChat});
	})()`
	raw, err := chromedp.Run(tabCtx, chromedp.Evaluate[string](js))
	if err != nil {
		return &Error{Status: http.StatusBadGateway, Kind: KindBrowser, Message: fmt.Sprintf("failed to open Chat workspace: %v", err)}
	}
	var result struct {
		OK      bool   `json:"ok"`
		Changed bool   `json:"changed"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(raw), &result); err != nil || !result.OK {
		message := result.Reason
		if message == "" {
			message = "Chat workspace could not be activated"
		}
		return &Error{Status: http.StatusBadGateway, Kind: KindBrowser, Message: message}
	}
	return nil
}

func waitForPageReady(ctx context.Context, tabCtx context.Context) *Error {
	deadline := time.Now().Add(20 * time.Second)
	checkJS := `(() => {
		if (document.querySelector('#cf-turnstile, iframe[src*="turnstile"], iframe[src*="challenges"], #challenge-stage')) {
			return JSON.stringify({ status: 'challenge', message: 'Cloudflare challenge detected' });
		}
		var txt = (document.body ? document.body.innerText : '').toLowerCase();
		if (txt.includes('just a moment...') && txt.includes('cloudflare')) {
			return JSON.stringify({ status: 'challenge', message: 'Cloudflare verification required' });
		}
		if (location.pathname.startsWith('/auth') || document.querySelector('a[href*="/auth/login"], button[data-testid="login-button"]')) {
			return JSON.stringify({ status: 'login', message: 'ChatGPT session expired or not logged in' });
		}
		var composer = document.querySelector('div#prompt-textarea[role="textbox"]')
					|| document.querySelector('div.ProseMirror[role="textbox"][data-codex-composer]')
					|| document.querySelector('div[contenteditable="true"]#prompt-textarea')
					|| document.querySelector('[contenteditable="true"][aria-label="Ask ChatGPT"]')
					|| document.querySelector('textarea#prompt-textarea');
		if (composer) {
			return JSON.stringify({ status: 'ready' });
		}
		return JSON.stringify({ status: 'waiting' });
	})()`

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return &Error{Status: 499, Kind: KindTimeout, Message: "request cancelled while loading page"}
		default:
		}

		raw, err := chromedp.Run(tabCtx, chromedp.Evaluate[string](checkJS))
		if err == nil && raw != "" {
			var res checkPageResult
			if json.Unmarshal([]byte(raw), &res) == nil {
				switch res.Status {
				case "ready":
					return nil
				case "challenge":
					return &Error{Status: http.StatusForbidden, Kind: KindBrowser, Message: res.Message}
				case "login":
					return &Error{Status: http.StatusUnauthorized, Kind: KindAuth, Message: res.Message}
				}
			}
		}
		select {
		case <-ctx.Done():
			return &Error{Status: 499, Kind: KindTimeout, Message: "request cancelled while loading page"}
		case <-time.After(500 * time.Millisecond):
		}
	}

	if ctx.Err() != nil {
		return &Error{Status: http.StatusGatewayTimeout, Kind: KindTimeout, Message: "timed out waiting for ChatGPT composer"}
	}
	return &Error{
		Status:  http.StatusGatewayTimeout,
		Kind:    KindTimeout,
		Message: "ChatGPT composer selector not found on page within timeout",
	}
}

func getBaselineCounts(ctx context.Context) (baselineCounts, *Error) {
	js := `(() => {
		var u = document.querySelectorAll('[data-user-message-bubble="true"], [data-message-author-role="user"]').length;
		var a = document.querySelectorAll('[data-markdown-text-style="assistant-message"], [data-message-author-role="assistant"]').length;
		return JSON.stringify({ userCount: u, asstCount: a });
	})()`

	raw, err := chromedp.Run(ctx, chromedp.Evaluate[string](js))
	if err != nil {
		return baselineCounts{}, &Error{
			Status:  http.StatusBadGateway,
			Kind:    KindBrowser,
			Message: fmt.Sprintf("failed to get baseline message counts: %v", err),
		}
	}
	var res baselineCounts
	_ = json.Unmarshal([]byte(raw), &res)
	return res, nil
}

func insertPromptAndVerify(ctx context.Context, textValue string) *Error {
	textJSON, err := json.Marshal(textValue)
	if err != nil {
		return &Error{Status: http.StatusBadRequest, Kind: KindInvalid, Message: err.Error()}
	}

	js := `(() => {
		var el = document.querySelector('div#prompt-textarea[role="textbox"]')
			  || document.querySelector('div.ProseMirror[role="textbox"][data-codex-composer]')
			  || document.querySelector('div[contenteditable="true"]#prompt-textarea')
			  || document.querySelector('[contenteditable="true"][aria-label="Ask ChatGPT"]');
		var isTextarea = false;
		if (!el || el.offsetParent === null) {
			el = document.querySelector('textarea#prompt-textarea');
			isTextarea = true;
		}
		if (!el) return JSON.stringify({ok:false,error:'visible composer not found'});
		el.focus();
		const ua = (navigator.userAgentData && navigator.userAgentData.platform) || navigator.platform || '';
		const isMac = /mac/i.test(ua);
		return JSON.stringify({ok:true,textarea:isTextarea,isMac:isMac});
	})()`
	raw, err := chromedp.Run(ctx, chromedp.Evaluate[string](js))
	if err != nil {
		return &Error{Status: http.StatusBadGateway, Kind: KindBrowser, Message: fmt.Sprintf("failed to focus ChatGPT composer: %v", err)}
	}
	var prepared struct {
		OK       bool   `json:"ok"`
		Textarea bool   `json:"textarea"`
		IsMac    bool   `json:"isMac"`
		Error    string `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &prepared); err != nil || !prepared.OK {
		msg := prepared.Error
		if msg == "" {
			msg = "composer focus failed"
		}
		return &Error{Status: http.StatusInternalServerError, Kind: KindBrowser, Message: msg}
	}
	if err := chromedp.Do(ctx, chromedp.Action[chromedp.Void](func(ctx context.Context, target *chromedp.Target) (chromedp.Void, error) {
		modifiers := int64(2)
		if prepared.IsMac {
			modifiers = 4
		}
		if _, err := cdp.Call(ctx, target, input.DispatchKeyEvent, input.DispatchKeyEventParams{
			Type:                  input.DispatchKeyEventTypeRawKeyDown,
			Key:                   "a",
			Code:                  "KeyA",
			WindowsVirtualKeyCode: 65,
			Modifiers:             modifiers,
			Commands:              []string{"selectAll"},
		}); err != nil {
			return chromedp.Void{}, err
		}
		if _, err := cdp.Call(ctx, target, input.DispatchKeyEvent, input.DispatchKeyEventParams{
			Type:                  input.DispatchKeyEventTypeKeyUp,
			Key:                   "a",
			Code:                  "KeyA",
			WindowsVirtualKeyCode: 65,
			Modifiers:             modifiers,
		}); err != nil {
			return chromedp.Void{}, err
		}
		if _, err := cdp.Call(ctx, target, input.DispatchKeyEvent, input.DispatchKeyEventParams{Type: input.DispatchKeyEventTypeKeyDown, Key: "Backspace", Code: "Backspace"}); err != nil {
			return chromedp.Void{}, err
		}
		if _, err := cdp.Call(ctx, target, input.DispatchKeyEvent, input.DispatchKeyEventParams{Type: input.DispatchKeyEventTypeKeyUp, Key: "Backspace", Code: "Backspace"}); err != nil {
			return chromedp.Void{}, err
		}
		_, err := cdp.Call(ctx, target, input.InsertText, input.InsertTextParams{Text: textValue})
		return chromedp.Void{}, err
	})); err != nil {
		return &Error{Status: http.StatusBadGateway, Kind: KindBrowser, Message: fmt.Sprintf("failed to replace composer text: %v", err)}
	}

	verifyJS := fmt.Sprintf(`(() => {
		const el = document.querySelector('div#prompt-textarea[role="textbox"]')
			  || document.querySelector('div.ProseMirror[role="textbox"][data-codex-composer]')
			  || document.querySelector('div[contenteditable="true"]#prompt-textarea')
			  || document.querySelector('[contenteditable="true"][aria-label="Ask ChatGPT"]')
			  || document.querySelector('textarea#prompt-textarea');
		if (!el) return JSON.stringify({ok:false, error:'composer missing during verify'});
		function isPlaceholderBreakBlock(node) {
			return node.nodeType === 1 && node.childNodes.length === 1 && node.firstChild.nodeType === 1 && node.firstChild.tagName === 'BR';
		}
		function extractText(node) {
			if (node.nodeType === 3) return (node.nodeValue || '').replace(/\u00a0/g, ' ');
			if (node.nodeType !== 1) return '';
			if (node.tagName === 'BR') return '\n';
			let text = '';
			for (let i = 0; i < node.childNodes.length; i++) text += extractText(node.childNodes[i]);
			return text;
		}
		let actual = '';
		if (el.tagName === 'TEXTAREA') {
			actual = el.value || '';
		} else {
			const parts = [];
			for (let i = 0; i < el.childNodes.length; i++) {
				const child = el.childNodes[i];
				if (child.nodeType === 3) {
					const t = (child.nodeValue || '').replace(/\u00a0/g, ' ');
					if (t) parts.push(t);
				} else if (child.nodeType === 1) {
					parts.push(isPlaceholderBreakBlock(child) ? '' : extractText(child));
				}
			}
			actual = parts.join('\n');
		}
		const canonActual = actual.replace(/\r\n/g, '\n').replace(/\r/g, '\n').replace(/\u00a0/g, ' ').normalize('NFC');
		const canonExpected = (%s).replace(/\r\n/g, '\n').replace(/\r/g, '\n').replace(/\u00a0/g, ' ').normalize('NFC');
		const textOK = canonActual === canonExpected || canonActual === canonExpected + '\n';
		const send = document.querySelector('button[aria-label*="Send" i]:not([data-testid="stop-button"]), button[data-testid="send-button"], form:has(#prompt-textarea) button[type="submit"], form:has(.ProseMirror) button[type="submit"]');
		const sendEnabled = !!send && !send.disabled && send.getAttribute('aria-disabled') !== 'true';
		return JSON.stringify({ok: textOK, sendReady: sendEnabled, actualLength: canonActual.length, error: textOK ? '' : 'composer text mismatch'});
	})()`, string(textJSON))

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		verified, err := chromedp.Run(ctx, chromedp.Evaluate[string](verifyJS))
		if err == nil {
			var result struct {
				OK        bool   `json:"ok"`
				SendReady bool   `json:"sendReady"`
				Error     string `json:"error"`
			}
			if json.Unmarshal([]byte(verified), &result) == nil && result.OK {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return &Error{Status: http.StatusInternalServerError, Kind: KindBrowser, Message: "composer text verification failed within timeout"}
}

func clickSendButton(ctx context.Context, tabCtx context.Context) *Error {
	deadline := time.Now().Add(10 * time.Second)
	sendSelector := `button[aria-label*="Send" i]:not([data-testid="stop-button"]), button[data-testid="send-button"], form:has(#prompt-textarea) button[type="submit"], form:has(.ProseMirror) button[type="submit"]`
	for time.Now().Before(deadline) {
		checkJS := fmt.Sprintf(`(() => {
			const btn = document.querySelector(%s);
			return JSON.stringify({found: !!btn, disabled: !!(btn && btn.disabled), aria: btn ? (btn.getAttribute('aria-disabled') || '') : ''});
		})()`, jsonStringLiteral(sendSelector))
		probeCtx, cancelProbe, stopProbe := browserActionContext(ctx, tabCtx, 500*time.Millisecond)
		raw, err := chromedp.Run(probeCtx, chromedp.Evaluate[string](checkJS))
		cancelProbe()
		stopProbe()
		if err == nil {
			var state struct {
				Found    bool   `json:"found"`
				Disabled bool   `json:"disabled"`
				Aria     string `json:"aria"`
			}
			if json.Unmarshal([]byte(raw), &state) == nil && sendControlEnabled(state.Found, state.Disabled, state.Aria) {
				break
			}
		}
		if ctx.Err() != nil {
			return &Error{Status: 499, Kind: KindTimeout, Message: "request cancelled while waiting for ChatGPT Send control"}
		}
		time.Sleep(300 * time.Millisecond)
	}

	clickJS := fmt.Sprintf(`(() => {
		const btn = document.querySelector(%s);
		if (!btn) return 'no send button';
		if (btn.disabled || btn.getAttribute('aria-disabled') === 'true') return 'button disabled';
		const evts = ['pointerdown', 'mousedown', 'pointerup', 'mouseup', 'click'];
		for (const name of evts) {
			btn.dispatchEvent(new MouseEvent(name, {bubbles: true, cancelable: true, view: window}));
		}
		return 'sent';
	})()`, jsonStringLiteral(sendSelector))

	clickCtx, cancelClick, stopClick := browserActionContext(ctx, tabCtx, BrowserActionTimeout)
	defer cancelClick()
	defer stopClick()
	res, err := chromedp.Run(clickCtx, chromedp.Evaluate[string](clickJS))
	if err != nil {
		return &Error{Status: http.StatusBadGateway, Kind: KindBrowser, Message: fmt.Sprintf("failed to submit ChatGPT turn: %v", err)}
	}
	if res != "sent" {
		return &Error{Status: http.StatusGatewayTimeout, Kind: KindBrowser, Message: "ChatGPT Send button did not become enabled within 10s: " + res}
	}
	return nil
}

func jsonStringLiteral(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func verifySendAccepted(ctx context.Context, tabCtx context.Context, baselineUsers int, evidence *conversationEvidence) *Error {
	deadline := time.Now().Add(30 * time.Second)
	checkJS := fmt.Sprintf(`(() => document.querySelectorAll('[data-user-message-bubble="true"], [data-message-author-role="user"]').length > %d)()`, baselineUsers)

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return &Error{Status: 499, Kind: KindTimeout, Message: "context cancelled during submit"}
		default:
		}

		probeCtx, cancelProbe, stopRequestCancel := browserActionContext(ctx, tabCtx, 500*time.Millisecond)
		userMessageVisible, err := chromedp.Run(probeCtx, chromedp.Evaluate[bool](checkJS))
		cancelProbe()
		stopRequestCancel()
		if err == nil && userMessageVisible {
			requestSeen, requestModel, _, _, _ := evidence.snapshot()
			if requestSeen && requestModel != "" {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return &Error{Status: 499, Kind: KindTimeout, Message: "context cancelled during submit"}
		case <-time.After(300 * time.Millisecond):
		}
	}

	requestSeen, requestModel, responseSeen, responseCode, failure := evidence.snapshot()
	return &Error{
		Status:  http.StatusGatewayTimeout,
		Kind:    KindTimeout,
		Message: fmt.Sprintf("message submit not confirmed: requestSeen=%t model=%q responseSeen=%t responseStatus=%d networkError=%q", requestSeen, requestModel, responseSeen, responseCode, failure),
	}
}

func pollBrowserState(tabCtx context.Context) (pollState, *Error) {
	js := `(() => {
		var dialogs = document.querySelectorAll('[role="dialog"], [role="alertdialog"], div.modal');
		for (var i = 0; i < dialogs.length; i++) {
			var txt = (dialogs[i].textContent || '').toLowerCase();
			if (txt.includes('too many requests') || txt.includes('rate limit') || txt.includes('exceeded your current quota')) {
				return JSON.stringify({ status: 'rate_limited', message: dialogs[i].textContent });
			}
		}

		var asstMsgs = document.querySelectorAll('[data-markdown-text-style="assistant-message"], [data-message-author-role="assistant"]');
		var count = asstMsgs.length;
		var lastText = '', htmlLen = 0, children = 0, hasAction = false, isThinking = false;
		if (count > 0) {
			var last = asstMsgs[count - 1];
			var md = last.querySelector('.markdown') || (last.classList && last.classList.contains('markdown') ? last : null);
			var markdownText = md ? (md.textContent || '') : '';
			var rawText = (last.innerText || '').trim();
			lastText = markdownText || rawText.replace(/^(thinking|reasoning)\b[^\n]*\n?/i, '');
			htmlLen = last.innerHTML.length;
			children = last.children.length;
			var actionSelector = '[data-testid*="turn-action-button"], [data-testid*="copy"], [data-testid*="response-turn"], .turn-action-controls button';
			var rect = last.getBoundingClientRect();
			var scope = last;
			for (var depth = 0; scope && depth <= 8; depth++, scope = scope.parentElement) {
				var actions = [...scope.querySelectorAll(actionSelector)].filter(el => el.offsetParent !== null || el.getClientRects().length > 0);
				if (actions.some(el => { var r = el.getBoundingClientRect(); return r.top >= rect.top - 180 && r.top <= rect.bottom + 240; })) { hasAction = true; break; }
			}
			var thinkingElement = !!last.querySelector('.result-thinking');
			isThinking = !hasAction && (thinkingElement || (/^(thinking|reasoning)\b/i.test(rawText) && !markdownText));
		}
		var hasStop = !!document.querySelector('button[aria-label*="Stop" i], button[data-testid="stop-button"]');
		return JSON.stringify({status:'ok',asstCount:count,text:lastText || '',htmlLen,children,hasAction,isThinking,hasStop});
	})()`

	raw, err := chromedp.Run(tabCtx, chromedp.Evaluate[string](js))
	if err != nil {
		return pollState{}, &Error{Status: http.StatusBadGateway, Kind: KindBrowser, Message: fmt.Sprintf("failed to inspect ChatGPT conversation: %v", err)}
	}

	var state pollState
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		return pollState{}, &Error{Status: http.StatusBadGateway, Kind: KindBrowser, Message: fmt.Sprintf("invalid ChatGPT conversation state: %v", err)}
	}
	if state.Status == "" {
		return pollState{}, &Error{Status: http.StatusBadGateway, Kind: KindBrowser, Message: "ChatGPT conversation state is empty"}
	}
	return state, nil
}

// observeTurn is the shared completion detector. A backend not-ready result
// never unlocks DOM completion. All successful paths reconcile the UUID turn.
func observeTurn(ctx context.Context, tabCtx context.Context, baselineAsst int, evidence *conversationEvidence, emit func(string) error) (string, *Error) {
	_, model, _, _, _ := evidence.snapshot()
	firstBudget, idleBudget := detectorBudgets(model)
	start, lastProgress := time.Now(), time.Now()
	var accumulated string
	lastHTML, lastChildren := 0, 0
	var lastBackend time.Time
	reconcileDeadline := time.Time{}
	advance := func(text string) *Error {
		if !strings.HasPrefix(text, accumulated) {
			return &Error{Status: http.StatusBadGateway, Kind: KindBrowser, Message: "authoritative conversation text diverged from emitted prefix"}
		}
		if text != accumulated {
			if emit != nil {
				if err := emit(text[len(accumulated):]); err != nil {
					return &Error{Status: 499, Kind: KindBrowser, Message: "response reader closed"}
				}
			}
			accumulated = text
			lastProgress = time.Now()
		}
		return nil
	}
	for {
		if ctx.Err() != nil {
			return "", &Error{Status: 499, Kind: KindTimeout, Message: "request cancelled while observing ChatGPT turn"}
		}
		budget := firstBudget
		if accumulated != "" {
			budget = idleBudget
		}
		expired := time.Since(start) >= TotalTurnTimeout || time.Since(lastProgress) >= budget
		stateCtx, cancelState, stopState := browserActionContext(ctx, tabCtx, 5*time.Second)
		state, pollErr := pollBrowserState(stateCtx)
		cancelState()
		stopState()
		if pollErr != nil {
			return "", pollErr
		}
		if state.Status == "rate_limited" {
			return "", &Error{Status: http.StatusTooManyRequests, Kind: KindRateLimit, Message: state.Message}
		}
		if time.Since(lastBackend) >= 3*time.Second || expired {
			lastBackend = time.Now()
			text, found, fetchErr := resolveTurnText(ctx, tabCtx, evidence)
			if fetchErr != nil && fetchErr.Kind == KindAuth {
				return "", fetchErr
			}
			if found {
				if err := advance(text); err != nil {
					return "", err
				}
				if err := verifyConversationEvidence(evidence, model, true); err != nil {
					return "", err
				}
				return accumulated, nil
			}
			// Transport failure can trigger bounded authoritative reconciliation,
			// but cannot itself make unverified DOM text an API success.
			if backendTransportFailed(fetchErr) && state.HasAction && accumulated != "" && reconcileDeadline.IsZero() {
				reconcileDeadline = time.Now().Add(30 * time.Second)
			}
			if fetchErr != nil && !backendTransportFailed(fetchErr) && fetchErr.Kind != KindTimeout {
				return "", fetchErr
			}
			if expired || (!reconcileDeadline.IsZero() && time.Now().After(reconcileDeadline)) {
				return "", &Error{Status: http.StatusGatewayTimeout, Kind: KindTimeout, Message: "ChatGPT turn did not reconcile within observation budget"}
			}
		}
		if state.AsstCount > baselineAsst {
			if !state.IsThinking && state.Text != "" {
				if err := advance(strings.ReplaceAll(state.Text, "\r\n", "\n")); err != nil {
					return "", err
				}
			}
			if state.HTMLLen != lastHTML || state.Children != lastChildren {
				lastProgress = time.Now()
				lastHTML, lastChildren = state.HTMLLen, state.Children
			}
		}
		if !waitForPoll(ctx) {
			continue
		}
	}
}

func detectorBudgets(model string) (time.Duration, time.Duration) {
	for _, marker := range []string{"thinking", "-t-mini", "o1", "o3", "o4", "research", "reasoning"} {
		if strings.Contains(strings.ToLower(model), marker) {
			return FirstAnswerTimeout, IdleTimeout
		}
	}
	return 90 * time.Second, 90 * time.Second
}

func backendTransportFailed(err *Error) bool {
	return err != nil && err.Kind == KindBrowser && strings.HasPrefix(err.Message, "ChatGPT conversation fetch transport failed")
}

func verifyConversationEvidence(evidence *conversationEvidence, model string, requireResponse bool) *Error {
	seen, observed, responded, code, failure := evidence.snapshot()
	if !seen {
		return &Error{Status: http.StatusBadGateway, Kind: KindBrowser, Message: "no unique correlated ChatGPT conversation POST"}
	}
	if model != "" && model != "auto" && !ModelMatchesRequested(model, observed) {
		return &Error{Status: http.StatusBadRequest, Kind: KindModel, Message: fmt.Sprintf("model mismatch: requested %q but outbound request used %q", model, observed)}
	}
	// A 2xx response is authoritative. ChatGPT streams the turn over SSE and
	// Chrome reports net::ERR_ABORTED when the stream is torn down after
	// completion, which is not a real failure.
	if responded && code >= 200 && code < 300 {
		return nil
	}
	if failure != "" || (responded && (code < 200 || code >= 300)) || (requireResponse && !responded) {
		return &Error{Status: http.StatusBadGateway, Kind: KindBrowser, Message: fmt.Sprintf("ChatGPT conversation POST failed: responseSeen=%t status=%d", responded, code)}
	}
	return nil
}

func runNonStreamingTurn(ctx context.Context, tabCtx context.Context, baselineAsst int, evidence *conversationEvidence) (string, *Error) {
	return observeTurn(ctx, tabCtx, baselineAsst, evidence, nil)
}

func writeStreamDelta(pw *io.PipeWriter, id string, created int64, modelID, delta string) error {
	chunk := map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": modelID, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": delta}, "finish_reason": nil}}}
	data, err := json.Marshal(chunk)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(pw, "data: %s\n\n", data)
	return err
}

func writeStreamDone(pw *io.PipeWriter, id string, created int64, modelID string) error {
	chunk := map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": modelID, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}}
	data, err := json.Marshal(chunk)
	if err != nil {
		return err
	}
	if _, err = fmt.Fprintf(pw, "data: %s\n\n", data); err != nil {
		return err
	}
	_, err = fmt.Fprintf(pw, "data: [DONE]\n\n")
	return err
}

func runStreamingTurn(ctx context.Context, tabCtx context.Context, pw *io.PipeWriter, baselineAsst int, modelID string, evidence *conversationEvidence, keepWindowOnFailure bool, keepTarget *atomic.Bool) {
	defer pw.Close()
	fail := func(err error) {
		if keepWindowOnFailure {
			keepTarget.Store(true)
		}
		_ = pw.CloseWithError(err)
	}
	if err := verifyConversationEvidence(evidence, modelID, false); err != nil {
		fail(err)
		return
	}
	chunkID := fmt.Sprintf("chatcmpl-%s", uuid.NewString())
	created := time.Now().Unix()
	_, err := observeTurn(ctx, tabCtx, baselineAsst, evidence, func(delta string) error {
		return writeStreamDelta(pw, chunkID, created, modelID, delta)
	})
	if err != nil {
		fail(err)
		return
	}
	if err := writeStreamDone(pw, chunkID, created, modelID); err != nil {
		fail(err)
	}
}

func waitForPoll(ctx context.Context) bool {
	timer := time.NewTimer(PollInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

type streamCloser struct {
	io.ReadCloser
	cancel      func()
	stop        func()
	releaseLock func()
	once        sync.Once
}

func (s *streamCloser) Close() error {
	s.once.Do(func() {
		if s.stop != nil {
			s.stop()
		}
		_ = s.ReadCloser.Close()
		if s.cancel != nil {
			s.cancel()
		}
		if s.releaseLock != nil {
			s.releaseLock()
		}
	})
	return nil
}
