package freebuff

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	// expiryMargin retires a session slightly early so a turn never starts on
	// one that dies mid-flight.
	expiryMargin = 60 * time.Second
	// runTTL bounds how long a root run is reused. The server only checks that
	// the run exists, so reuse saves two upstream calls per turn.
	runTTL = 10 * time.Minute
	// maxQueuePolls bounds the waiting-room loop for one call.
	maxQueuePolls = 8
	queuePollGap  = 1500 * time.Millisecond
	callTimeout   = 20 * time.Second
)

// Session is a live free session for one model.
type Session struct {
	InstanceID string
	// Model is the model the server locked the session to. A limited-tier
	// account is pinned to one model and answers 409 for any other.
	Model     string
	ExpiresAt time.Time
}

func (s Session) usable(now time.Time) bool {
	return s.InstanceID != "" && now.Add(expiryMargin).Before(s.ExpiresAt)
}

// Kind classifies a Freebuff refusal for routing.
type Kind string

const (
	KindAuth        Kind = "auth"         // 401: token rejected
	KindBanned      Kind = "banned"       // 403 {"status":"banned"}
	KindRateLimit   Kind = "rate_limit"   // 429: daily free quota
	KindWaitingRoom Kind = "waiting_room" // queued / 503
	KindSession     Kind = "session"      // stale session; mint another
	KindRun         Kind = "run"          // run gone; start another
	KindModel       Kind = "model"        // model unavailable for this account
	KindOther       Kind = "other"
)

// Error is a classified upstream refusal.
type Error struct {
	Status   int
	Kind     Kind
	Message  string
	ResetsAt time.Time     // zero when unstated
	Retry    time.Duration // zero when unstated
}

func (e *Error) Error() string {
	return fmt.Sprintf("Freebuff %s (HTTP %d): %s", e.Kind, e.Status, e.Message)
}

// Retryable reports whether minting a fresh session or run may fix the call.
func (e *Error) Retryable() bool { return e.Kind == KindSession || e.Kind == KindRun }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func has(lower string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(lower, n) {
			return true
		}
	}
	return false
}

// Classify maps a non-2xx response to an Error. now anchors relative resets.
func Classify(status int, body string, header http.Header, now time.Time) *Error {
	lower := strings.ToLower(body)
	e := &Error{Status: status, Kind: KindOther, Message: truncate(strings.TrimSpace(body), 500)}
	if e.Message == "" {
		e.Message = http.StatusText(status)
	}
	e.Retry = retryAfter(header, body)

	var parsed struct {
		ResetAt   string `json:"resetAt"`
		ResumesAt string `json:"resumes_at"`
	}
	_ = json.Unmarshal([]byte(body), &parsed)

	switch {
	case status == http.StatusForbidden && has(lower, `"status":"banned"`, `"status": "banned"`):
		e.Kind = KindBanned
		e.ResetsAt = parseTime(parsed.ResumesAt)
	case status == http.StatusUnauthorized:
		e.Kind = KindAuth
	case has(lower, "model_unavailable") || status == http.StatusGone && !has(lower, "session_expired"):
		e.Kind = KindModel
	case has(lower, "freebuff_update_required", "waiting_room_required", "session_superseded",
		"session_expired", "session_model_mismatch"):
		// Matched before 503/429 so a gate code is never mistaken for load.
		e.Kind = KindSession
	case status == http.StatusBadRequest && has(lower, "runid not found", "runid not running", "run not found"):
		e.Kind = KindRun
	case has(lower, "waiting_room_queued") || status == http.StatusServiceUnavailable:
		e.Kind = KindWaitingRoom
	case status == http.StatusTooManyRequests:
		e.Kind = KindRateLimit
		e.ResetsAt = parseTime(parsed.ResetAt)
		if e.ResetsAt.IsZero() && e.Retry > 0 {
			e.ResetsAt = now.Add(e.Retry)
		}
	}
	return e
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC()
	}
	return time.Time{}
}

func retryAfter(h http.Header, body string) time.Duration {
	var parsed struct {
		RetryAfterMs int64 `json:"retryAfterMs"`
	}
	if json.Unmarshal([]byte(body), &parsed) == nil && parsed.RetryAfterMs > 0 {
		return time.Duration(parsed.RetryAfterMs) * time.Millisecond
	}
	raw := h.Get("Retry-After")
	if raw == "" {
		return 0
	}
	if secs, err := strconv.Atoi(raw); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(raw); err == nil {
		return time.Until(t)
	}
	return 0
}

// Options tune a Manager.
type Options struct {
	HTTP     *http.Client
	BaseURL  string
	Token    string
	Resolver *Resolver
	Now      func() time.Time
	Sleep    func(time.Duration)
	// QueueGap overrides the waiting-room poll interval (tests).
	QueueGap time.Duration
}

// Manager owns the session and run state for one token. Safe for concurrent
// use: creation is single-flight per model, so a burst of turns shares one
// upstream session instead of stealing it from each other (a second POST for
// the same account supersedes the first and the in-flight chat gets a 409).
type Manager struct {
	opts Options

	mu       sync.Mutex
	sessions map[string]Session
	creating map[string]*flight
	runs     map[string]runEntry
}

type flight struct {
	done chan struct{}
	sess Session
	err  error
}

type runEntry struct {
	id string
	at time.Time
}

// NewManager builds a Manager.
func NewManager(opts Options) *Manager {
	if opts.Resolver == nil {
		opts.Resolver = NewResolver(nil)
	}
	return &Manager{
		opts:     opts,
		sessions: map[string]Session{},
		creating: map[string]*flight{},
		runs:     map[string]runEntry{},
	}
}

func (m *Manager) now() time.Time {
	if m.opts.Now != nil {
		return m.opts.Now()
	}
	return time.Now()
}

func (m *Manager) sleep(d time.Duration) {
	if m.opts.Sleep != nil {
		m.opts.Sleep(d)
		return
	}
	time.Sleep(d)
}

func (m *Manager) client() *http.Client {
	if m.opts.HTTP != nil {
		return m.opts.HTTP
	}
	return http.DefaultClient
}

func (m *Manager) base() string { return EndpointFromBaseURL(m.opts.BaseURL) }

// Invalidate drops the cached session and run for a model so the next Ensure
// mints fresh ones.
func (m *Manager) Invalidate(model string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, model)
	for key := range m.runs {
		if strings.HasPrefix(key, model+"\x00") {
			delete(m.runs, key)
		}
	}
}

// InvalidateRun drops only the cached run (the session is still good).
func (m *Manager) InvalidateRun(model, agent string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.runs, model+"\x00"+agent)
}

// Ensure returns a usable session for model, creating one when needed.
func (m *Manager) Ensure(ctx context.Context, model string) (Session, error) {
	for {
		m.mu.Lock()
		if s, ok := m.sessions[model]; ok && s.usable(m.now()) {
			m.mu.Unlock()
			return s, nil
		}
		if f, ok := m.creating[model]; ok {
			m.mu.Unlock()
			select {
			case <-f.done:
				if f.err != nil {
					return Session{}, f.err
				}
				if f.sess.usable(m.now()) {
					return f.sess, nil
				}
				continue
			case <-ctx.Done():
				return Session{}, ctx.Err()
			}
		}
		f := &flight{done: make(chan struct{})}
		m.creating[model] = f
		m.mu.Unlock()

		f.sess, f.err = m.create(ctx, model)

		m.mu.Lock()
		delete(m.creating, model)
		if f.err == nil {
			m.sessions[model] = f.sess
		}
		m.mu.Unlock()
		close(f.done)
		return f.sess, f.err
	}
}

type sessionResponse struct {
	Status      string `json:"status"`
	InstanceID  string `json:"instanceId"`
	Model       string `json:"model"`
	ExpiresAt   string `json:"expiresAt"`
	RemainingMs int64  `json:"remainingMs"`
}

func (r sessionResponse) session(model string, now time.Time) Session {
	s := Session{InstanceID: r.InstanceID, Model: model}
	if r.Model != "" {
		s.Model = r.Model
	}
	switch {
	case parseTime(r.ExpiresAt) != (time.Time{}):
		s.ExpiresAt = parseTime(r.ExpiresAt)
	case r.RemainingMs > 0:
		s.ExpiresAt = now.Add(time.Duration(r.RemainingMs) * time.Millisecond)
	default:
		s.ExpiresAt = now.Add(time.Hour)
	}
	return s
}

func (m *Manager) call(ctx context.Context, method, path string, body any, headers map[string]string) (int, http.Header, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, nil, nil, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, m.base()+path, reader)
	if err != nil {
		return 0, nil, nil, err
	}
	SetHeaders(req.Header, m.opts.Token)
	req.Header.Set("User-Agent", UserAgent)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := m.client().Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, resp.Header, data, nil
}

// create mints a session and waits out the waiting room.
func (m *Manager) create(ctx context.Context, model string) (Session, error) {
	instance := uuid.NewString()
	headers := map[string]string{"x-freebuff-model": model, "x-freebuff-instance-id": instance}
	status, hdr, data, err := m.call(ctx, http.MethodPost, sessionPath, map[string]any{}, headers)
	if err != nil {
		return Session{}, err
	}
	if status == http.StatusConflict {
		// session_model_mismatch: the account is locked to another model.
		return Session{}, Classify(status, string(data), hdr, m.now())
	}
	if status != http.StatusOK {
		return Session{}, Classify(status, string(data), hdr, m.now())
	}
	var resp sessionResponse
	_ = json.Unmarshal(data, &resp)
	if resp.InstanceID == "" {
		resp.InstanceID = instance
	}
	gap := queuePollGap
	if m.opts.QueueGap > 0 {
		gap = m.opts.QueueGap
	}
	for polls := 0; ; polls++ {
		switch resp.Status {
		case "active", "":
			if resp.Status == "" && resp.InstanceID == "" {
				return Session{}, &Error{Status: status, Kind: KindOther, Message: "session response had no instance id"}
			}
			return resp.session(model, m.now()), nil
		case "queued":
			if polls >= maxQueuePolls {
				return Session{}, &Error{Status: http.StatusServiceUnavailable, Kind: KindWaitingRoom,
					Message: "session stayed queued", Retry: 5 * time.Second}
			}
			m.sleep(gap)
			status, hdr, data, err = m.call(ctx, http.MethodGet, sessionPath, nil,
				map[string]string{"x-freebuff-instance-id": resp.InstanceID})
			if err != nil {
				return Session{}, err
			}
			if status != http.StatusOK {
				return Session{}, Classify(status, string(data), hdr, m.now())
			}
			keep := resp.InstanceID
			resp = sessionResponse{}
			_ = json.Unmarshal(data, &resp)
			if resp.InstanceID == "" {
				resp.InstanceID = keep
			}
		case "banned":
			return Session{}, &Error{Status: http.StatusForbidden, Kind: KindBanned, Message: "account banned upstream"}
		case "country_blocked":
			return Session{}, &Error{Status: http.StatusForbidden, Kind: KindOther, Message: "Freebuff is not available in this country"}
		default:
			// ended / superseded / none: the caller mints another.
			return Session{}, &Error{Status: status, Kind: KindSession, Message: "session " + resp.Status}
		}
	}
}

// Run returns a run id for the agent, starting one when none is cached.
func (m *Manager) Run(ctx context.Context, model, agent string) (string, error) {
	key := model + "\x00" + agent
	m.mu.Lock()
	if r, ok := m.runs[key]; ok && m.now().Sub(r.at) < runTTL {
		m.mu.Unlock()
		return r.id, nil
	}
	m.mu.Unlock()

	id, err := m.startRun(ctx, agent, nil)
	if err != nil {
		return "", err
	}
	// The CLI starts a context-pruner child beside the root run; failing to
	// start it must not fail the turn.
	_, _ = m.startRun(ctx, ContextPrunerAgent, []string{id})

	m.mu.Lock()
	m.runs[key] = runEntry{id: id, at: m.now()}
	m.mu.Unlock()
	return id, nil
}

func (m *Manager) startRun(ctx context.Context, agent string, ancestors []string) (string, error) {
	if ancestors == nil {
		ancestors = []string{}
	}
	status, hdr, data, err := m.call(ctx, http.MethodPost, runsPath,
		map[string]any{"action": "START", "agentId": agent, "ancestorRunIds": ancestors}, nil)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", Classify(status, string(data), hdr, m.now())
	}
	var resp struct {
		RunID string `json:"runId"`
	}
	if err := json.Unmarshal(data, &resp); err != nil || resp.RunID == "" {
		return "", errors.New("Freebuff agent-runs START returned no runId")
	}
	return resp.RunID, nil
}
