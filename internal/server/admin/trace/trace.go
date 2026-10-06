// Package trace is the bounded in-memory routing trace: what routing did with
// a turn, recorded as it did it. Port of src/trace.ts.
//
// A Store is an injected value (no package state). The upstream attempt loop
// writes to it (BeginRoute / BeginTry / EndTry / FirstToken / FinishRoute);
// the admin API reads it (Recent / Get / WaitForSession / Subscribe). Only the
// newest Keep turns stay resident; nothing is written to disk.
package trace

import (
	"context"
	"sync"
	"time"
)

// Keep is how many turns stay resident.
const Keep = 200

// WaitDefault is the default long-poll ceiling.
const WaitDefault = 60 * time.Second

// Skip records why a candidate was withheld.
type Skip struct {
	Reason string `json:"reason"`
	Detail string `json:"detail"`
}

// Candidate is one candidate as routing weighed it.
type Candidate struct {
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	Canonical  string `json:"canonical,omitempty"`
	Rank       int    `json:"rank"`
	Quota      string `json:"quota,omitempty"`
	CacheState string `json:"cacheState,omitempty"`
	Skipped    *Skip  `json:"skipped,omitempty"`
}

// Try is one upstream attempt.
type Try struct {
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	Effort    string `json:"effort,omitempty"`
	Cause     string `json:"cause"` // initial | retry | failover
	StartedAt int64  `json:"startedAt"`
	Ms        *int64 `json:"ms,omitempty"`
	TTFTMs    *int64 `json:"ttftMs,omitempty"`
	Status    *int   `json:"status,omitempty"`
	Fail      string `json:"fail,omitempty"`
	Done      bool   `json:"done"`
}

// Brain is how routing decided.
type Brain struct {
	Channel    string   `json:"channel,omitempty"`
	Model      string   `json:"model,omitempty"`
	Confidence *float64 `json:"confidence,omitempty"`
	Ms         int64    `json:"ms"`
}

// Route is a single turn's routing.
type Route struct {
	Seq            int64       `json:"seq"`
	RequestID      string      `json:"requestId"`
	Session        string      `json:"session"`
	ClientSession  string      `json:"clientSession,omitempty"`
	KeyID          string      `json:"keyId,omitempty"`
	KeyName        string      `json:"keyName,omitempty"`
	At             string      `json:"at"`
	Path           string      `json:"path"`
	RequestedModel string      `json:"requestedModel"`
	Stream         bool        `json:"stream"`
	Phase          string      `json:"phase,omitempty"`
	Reason         string      `json:"reason,omitempty"`
	Brain          *Brain      `json:"brain,omitempty"`
	CacheKeep      string      `json:"cacheKeep,omitempty"`
	Order          []Candidate `json:"order"`
	Tries          []Try       `json:"tries"`
	Failovers      int         `json:"failovers"`
	Done           bool        `json:"done"`
	Status         *int        `json:"status,omitempty"`
	Error          string      `json:"error,omitempty"`
	Ms             *int64      `json:"ms,omitempty"`
	TTFTMs         *int64      `json:"ttftMs,omitempty"`

	startedAt int64
}

// Init starts a trace.
type Init struct {
	RequestID      string
	Session        string
	ClientSession  string
	KeyID          string
	KeyName        string
	Path           string
	RequestedModel string
	Stream         bool
	StartedAt      int64 // epoch ms; 0 means now
	Phase          string
	Reason         string
	CacheKeep      string
}

// Decision is a re-route update; nil/empty fields are left alone.
type Decision struct {
	Phase     *string
	Reason    *string
	CacheKeep *string
	Brain     *Brain
}

type waiter struct {
	session string
	keyID   *string
	after   int64
	ch      chan Route
}

// Store holds recent traces. The zero value is not usable; call New.
type Store struct {
	mu        sync.Mutex
	now       func() time.Time
	order     []string // insertion order, oldest first
	byID      map[string]*Route
	latest    map[string]string // session → request id
	listeners map[int]func(Route)
	nextSub   int
	waiters   map[*waiter]struct{}
}

// New builds a store. now may be nil (wall clock).
func New(now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{
		now:       now,
		byID:      map[string]*Route{},
		latest:    map[string]string{},
		listeners: map[int]func(Route){},
		waiters:   map[*waiter]struct{}{},
	}
}

func (s *Store) ms() int64 { return s.now().UnixMilli() }

func clone(r *Route) Route {
	out := *r
	out.Order = append([]Candidate{}, r.Order...)
	out.Tries = make([]Try, len(r.Tries))
	copy(out.Tries, r.Tries)
	return out
}

func maxZero(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

// changedLocked bumps seq and collects the fan-out; callers run notify after
// releasing the lock so a listener can never deadlock the store.
func (s *Store) changedLocked(r *Route) func() {
	r.Seq++
	snap := clone(r)
	listeners := make([]func(Route), 0, len(s.listeners))
	for _, l := range s.listeners {
		listeners = append(listeners, l)
	}
	var woken []*waiter
	for w := range s.waiters {
		if w.session != r.Session {
			continue
		}
		if w.keyID != nil && *w.keyID != r.KeyID {
			continue
		}
		if r.Seq <= w.after {
			continue
		}
		delete(s.waiters, w)
		woken = append(woken, w)
	}
	return func() {
		for _, l := range listeners {
			func() {
				defer func() { _ = recover() }()
				l(clone(&snap))
			}()
		}
		for _, w := range woken {
			w.ch <- clone(&snap)
		}
	}
}

func openTry(r *Route) *Try {
	for i := len(r.Tries) - 1; i >= 0; i-- {
		if !r.Tries[i].Done {
			return &r.Tries[i]
		}
	}
	return nil
}

func (s *Store) dropLocked(id string) {
	r := s.byID[id]
	delete(s.byID, id)
	if r != nil && s.latest[r.Session] == id {
		delete(s.latest, r.Session)
	}
	for i, v := range s.order {
		if v == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
}

// trimLocked drops the oldest traces over Keep, preferring finished ones.
func (s *Store) trimLocked() {
	overflow := len(s.order) - Keep
	if overflow <= 0 {
		return
	}
	var drop []string
	for _, id := range s.order {
		if len(drop) >= overflow {
			break
		}
		if s.byID[id].Done {
			drop = append(drop, id)
		}
	}
	for _, id := range drop {
		s.dropLocked(id)
	}
	for len(s.order) > Keep {
		s.dropLocked(s.order[0])
	}
}

// BeginRoute starts a trace; re-using an id replaces the previous trace.
func (s *Store) BeginRoute(in Init) Route {
	started := in.StartedAt
	if started == 0 {
		started = s.ms()
	}
	r := &Route{
		RequestID: in.RequestID, Session: in.Session, ClientSession: in.ClientSession,
		KeyID: in.KeyID, KeyName: in.KeyName,
		At:   time.UnixMilli(started).UTC().Format("2006-01-02T15:04:05.000Z"),
		Path: in.Path, RequestedModel: in.RequestedModel, Stream: in.Stream,
		Phase: in.Phase, Reason: in.Reason, CacheKeep: in.CacheKeep,
		Order: []Candidate{}, Tries: []Try{}, startedAt: started,
	}
	s.mu.Lock()
	if _, ok := s.byID[in.RequestID]; ok {
		s.dropLocked(in.RequestID)
	}
	s.byID[in.RequestID] = r
	s.order = append(s.order, in.RequestID)
	s.latest[in.Session] = in.RequestID
	s.trimLocked()
	notify := s.changedLocked(r)
	snap := clone(r)
	s.mu.Unlock()
	notify()
	return snap
}

func (s *Store) mutate(id string, fn func(r *Route) bool) {
	s.mu.Lock()
	r := s.byID[id]
	if r == nil || !fn(r) {
		s.mu.Unlock()
		return
	}
	notify := s.changedLocked(r)
	s.mu.Unlock()
	notify()
}

// Weigh records the candidate order routing chose from, best first.
func (s *Store) Weigh(id string, order []Candidate) {
	s.mutate(id, func(r *Route) bool {
		r.Order = make([]Candidate, len(order))
		for i, c := range order {
			c.Rank = i
			r.Order[i] = c
		}
		return true
	})
}

// NoteDecision records a (re-)decision.
func (s *Store) NoteDecision(id string, d Decision) {
	s.mutate(id, func(r *Route) bool {
		if d.Phase != nil {
			r.Phase = *d.Phase
		}
		if d.Reason != nil {
			r.Reason = *d.Reason
		}
		if d.CacheKeep != nil {
			r.CacheKeep = *d.CacheKeep
		}
		if d.Brain != nil {
			b := *d.Brain
			r.Brain = &b
		}
		return true
	})
}

// BeginTry opens an attempt; a "failover" cause also counts the failover.
func (s *Store) BeginTry(id, provider, model, cause, effort string) {
	s.mutate(id, func(r *Route) bool {
		if cause == "failover" {
			r.Failovers++
		}
		r.Tries = append(r.Tries, Try{
			Provider: provider, Model: model, Effort: effort, Cause: cause, StartedAt: s.ms(),
		})
		return true
	})
}

// EndTry closes the open attempt. status 0 means "not stated".
func (s *Store) EndTry(id string, status int, fail string) {
	s.mutate(id, func(r *Route) bool {
		t := openTry(r)
		if t == nil {
			return false
		}
		t.Done = true
		ms := maxZero(s.ms() - t.StartedAt)
		t.Ms = &ms
		if status != 0 {
			st := status
			t.Status = &st
		}
		if fail != "" {
			t.Fail = fail
		}
		return true
	})
}

// FirstToken records the first streamed content of the turn, once.
func (s *Store) FirstToken(id string) {
	s.mutate(id, func(r *Route) bool {
		if r.TTFTMs != nil {
			return false
		}
		now := s.ms()
		elapsed := maxZero(now - r.startedAt)
		r.TTFTMs = &elapsed
		if t := openTry(r); t != nil {
			v := maxZero(now - t.StartedAt)
			t.TTFTMs = &v
		}
		return true
	})
}

// FinishRoute closes a trace with the turn's final status.
func (s *Store) FinishRoute(id string, status int, errText string) (Route, bool) {
	var out Route
	found := false
	s.mutate(id, func(r *Route) bool {
		now := s.ms()
		if t := openTry(r); t != nil {
			t.Done = true
			ms := maxZero(now - t.StartedAt)
			t.Ms = &ms
			st := status
			t.Status = &st
			if t.Fail == "" && status >= 400 {
				if status == 499 {
					t.Fail = "client-canceled"
				} else {
					t.Fail = "http-" + itoa(status)
				}
			}
		}
		r.Done = true
		st := status
		r.Status = &st
		ms := maxZero(now - r.startedAt)
		r.Ms = &ms
		if errText != "" {
			r.Error = errText
		}
		found = true
		out = clone(r)
		out.Seq++ // the bump changedLocked is about to apply
		return true
	})
	return out, found
}

// LatestFor is the session's latest trace, scoped to keyID when non-nil.
func (s *Store) LatestFor(session string, keyID *string) (Route, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.latestLocked(session, keyID)
}

func (s *Store) latestLocked(session string, keyID *string) (Route, bool) {
	r := s.byID[s.latest[session]]
	if r == nil || (keyID != nil && r.KeyID != *keyID) {
		return Route{}, false
	}
	return clone(r), true
}

// Get returns a trace by request id.
func (s *Store) Get(id string) (Route, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.byID[id]
	if r == nil {
		return Route{}, false
	}
	return clone(r), true
}

// Recent lists traces newest first.
func (s *Store) Recent(limit int) []Route {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit < 0 {
		limit = 0
	}
	out := make([]Route, 0, len(s.order))
	for i := len(s.order) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, clone(s.byID[s.order[i]]))
	}
	return out
}

// WaitForSession returns the session's latest trace once its seq passes
// after, or ok=false when wait (or ctx) elapses first. wait<=0 never blocks.
func (s *Store) WaitForSession(ctx context.Context, session string, keyID *string, after int64, wait time.Duration) (Route, bool) {
	s.mu.Lock()
	if cur, ok := s.latestLocked(session, keyID); ok && cur.Seq > after {
		s.mu.Unlock()
		return cur, true
	}
	if wait <= 0 {
		s.mu.Unlock()
		return Route{}, false
	}
	w := &waiter{session: session, keyID: keyID, after: after, ch: make(chan Route, 1)}
	s.waiters[w] = struct{}{}
	s.mu.Unlock()

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case r := <-w.ch:
		return r, true
	case <-timer.C:
	case <-ctx.Done():
	}
	s.mu.Lock()
	delete(s.waiters, w)
	s.mu.Unlock()
	select { // a wake may have raced the timeout
	case r := <-w.ch:
		return r, true
	default:
		return Route{}, false
	}
}

// Subscribe registers a listener for every change; the returned func removes it.
func (s *Store) Subscribe(fn func(Route)) func() {
	s.mu.Lock()
	id := s.nextSub
	s.nextSub++
	s.listeners[id] = fn
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		delete(s.listeners, id)
		s.mu.Unlock()
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
