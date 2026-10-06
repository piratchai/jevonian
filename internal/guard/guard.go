// Package guard isolates providers from each other: a per-provider concurrency
// cap plus a circuit breaker with a single half-open probe.
//
// Port of src/provider-guard.ts, but as an injected value instead of module
// state. A wedged host must not absorb every in-flight turn: when a provider is
// saturated or its breaker is open, routing moves to the next candidate instead
// of queueing behind it or paying a fresh timeout each turn.
//
// Transport failures (reset, refused, timeout, retryable 5xx) belong here.
// Quota/billing refusals belong in internal/quota — conflating the two makes
// the dashboard report a flaky host as "exhausted".
package guard

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Defaults match src/provider-guard.ts.
const (
	DefaultConcurrency      = 8
	DefaultFailureThreshold = 3
	DefaultCooldown         = 30 * time.Second
)

// Block explains why Begin refused a provider.
type Block string

const (
	BlockNone        Block = ""
	BlockBreakerOpen Block = "breaker-open"
	BlockSaturated   Block = "saturated"
)

// Options configure a Guard. Zero values fall back to the defaults.
type Options struct {
	Concurrency      int
	FailureThreshold int
	Cooldown         time.Duration
	Now              func() time.Time
}

// OptionsFromEnv reads JEVONIAN_PROVIDER_CONCURRENCY,
// JEVONIAN_PROVIDER_BREAKER_THRESHOLD and JEVONIAN_PROVIDER_BREAKER_COOLDOWN_MS
// with the same floors/ceilings as the TS guard.
func OptionsFromEnv() Options {
	return Options{
		Concurrency:      readInt("JEVONIAN_PROVIDER_CONCURRENCY", DefaultConcurrency, 1, 256),
		FailureThreshold: readInt("JEVONIAN_PROVIDER_BREAKER_THRESHOLD", DefaultFailureThreshold, 1, 100),
		Cooldown: time.Duration(readInt(
			"JEVONIAN_PROVIDER_BREAKER_COOLDOWN_MS", int(DefaultCooldown/time.Millisecond), 1_000, 30*60_000,
		)) * time.Millisecond,
	}
}

type state struct {
	inFlight  int
	failures  int
	openUntil time.Time // zero while closed
	probing   bool      // a half-open probe is out
}

// Guard tracks in-flight attempts and breaker state per provider.
// Safe for concurrent use; bounded by the number of configured providers.
type Guard struct {
	opts Options
	mu   sync.Mutex
	st   map[string]*state
	caps map[string]int // per-provider concurrency overrides
}

// New returns a Guard. Missing options use the defaults.
func New(opts Options) *Guard {
	if opts.Concurrency <= 0 {
		opts.Concurrency = DefaultConcurrency
	}
	if opts.FailureThreshold <= 0 {
		opts.FailureThreshold = DefaultFailureThreshold
	}
	if opts.Cooldown <= 0 {
		opts.Cooldown = DefaultCooldown
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Guard{opts: opts, st: make(map[string]*state)}
}

// SetLimit overrides the concurrency cap for one provider. A cap below 1 clears
// the override. Used for upstreams that cannot take parallel turns on one
// account (Freebuff: a second session supersedes the first).
func (g *Guard) SetLimit(provider string, limit int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if limit < 1 {
		delete(g.caps, provider)
		return
	}
	if g.caps == nil {
		g.caps = map[string]int{}
	}
	g.caps[provider] = limit
}

// limitFor is the cap that applies to a provider. Callers hold g.mu.
func (g *Guard) limitFor(provider string) int {
	if n, ok := g.caps[provider]; ok {
		return n
	}
	return g.opts.Concurrency
}

func (g *Guard) get(provider string) *state {
	s := g.st[provider]
	if s == nil {
		s = &state{}
		g.st[provider] = s
	}
	return s
}

// Attempt is a held slot. Call End exactly once with the outcome.
type Attempt struct {
	g        *Guard
	provider string
	once     sync.Once
}

// End releases the slot and feeds the outcome into the breaker.
// ok=false only for host failures (transport / retryable 5xx), never for
// client errors or quota refusals.
func (a *Attempt) End(ok bool) {
	if a == nil {
		return
	}
	a.once.Do(func() { a.g.end(a.provider, ok) })
}

// Hold releases the slot and opens the breaker until `until`: the host said it
// is busy for a known time (a queue, a waiting room). It is a host verdict, so
// it stays in the guard and never marks the account spent. A zero or past
// `until` is a plain host failure.
func (a *Attempt) Hold(until time.Time) {
	if a == nil {
		return
	}
	a.once.Do(func() { a.g.hold(a.provider, until) })
}

// Release frees the slot without judging the host (e.g. the client hung up).
func (a *Attempt) Release() {
	if a == nil {
		return
	}
	a.once.Do(func() { a.g.release(a.provider) })
}

// Begin atomically takes a concurrency slot and, when the breaker is
// half-open, the single probe. A non-empty Block means skip this provider.
func (g *Guard) Begin(provider string) (*Attempt, Block) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.get(provider)
	now := g.opts.Now()
	halfOpen := false
	if !s.openUntil.IsZero() {
		if now.Before(s.openUntil) {
			return nil, BlockBreakerOpen
		}
		// Past the cooldown: exactly one probe goes out; the rest wait for its verdict.
		if s.probing {
			return nil, BlockBreakerOpen
		}
		halfOpen = true
	}
	if s.inFlight >= g.limitFor(provider) {
		return nil, BlockSaturated
	}
	s.inFlight++
	if halfOpen {
		s.probing = true
	}
	return &Attempt{g: g, provider: provider}, BlockNone
}

func (g *Guard) end(provider string, ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.get(provider)
	if s.inFlight > 0 {
		s.inFlight--
	}
	s.probing = false
	if ok {
		s.failures = 0
		s.openUntil = time.Time{}
		return
	}
	s.failures++
	if s.failures >= g.opts.FailureThreshold {
		s.openUntil = g.opts.Now().Add(g.opts.Cooldown)
	}
}

func (g *Guard) hold(provider string, until time.Time) {
	if !until.After(g.opts.Now()) {
		g.end(provider, false)
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.get(provider)
	if s.inFlight > 0 {
		s.inFlight--
	}
	s.probing = false
	if until.After(s.openUntil) {
		s.openUntil = until
	}
}

func (g *Guard) release(provider string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.get(provider)
	if s.inFlight > 0 {
		s.inFlight--
	}
	s.probing = false
}

// Blocked is a side-effect-free read for routing while ranking candidates:
// true when the breaker is open (no probe due) or the provider is saturated.
// Unlike Begin it never claims the half-open probe.
func (g *Guard) Blocked(provider string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.st[provider]
	if s == nil {
		return false
	}
	if !s.openUntil.IsZero() && (g.opts.Now().Before(s.openUntil) || s.probing) {
		return true
	}
	return s.inFlight >= g.limitFor(provider)
}

// Spent satisfies routing.GuardView: a blocked provider ranks as spent.
// The now argument is ignored; the guard uses its own clock.
func (g *Guard) Spent(provider string, _ int64) bool { return g.Blocked(provider) }

// Snapshot is a diagnostics view of one provider.
type Snapshot struct {
	InFlight int  `json:"inFlight"`
	Failures int  `json:"failures"`
	Open     bool `json:"open"`
}

// Snapshot returns the current state of a provider.
func (g *Guard) Snapshot(provider string) Snapshot {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.st[provider]
	if s == nil {
		return Snapshot{}
	}
	return Snapshot{
		InFlight: s.inFlight,
		Failures: s.failures,
		Open:     !s.openUntil.IsZero() && g.opts.Now().Before(s.openUntil),
	}
}

// Concurrency is the configured per-provider cap.
func (g *Guard) Concurrency() int { return g.opts.Concurrency }

func readInt(name string, fallback, lo, hi int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
