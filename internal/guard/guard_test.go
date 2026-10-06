package guard_test

import (
	"sync"
	"testing"
	"time"

	"github.com/xinyao27/jevonian/internal/guard"
	"github.com/xinyao27/jevonian/internal/routing"
)

var _ routing.GuardView = (*guard.Guard)(nil)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestSaturationBlocksThenFrees(t *testing.T) {
	g := guard.New(guard.Options{Concurrency: 2})
	a1, b := g.Begin("p")
	if b != guard.BlockNone {
		t.Fatalf("first begin blocked: %s", b)
	}
	a2, b := g.Begin("p")
	if b != guard.BlockNone {
		t.Fatalf("second begin blocked: %s", b)
	}
	if _, b := g.Begin("p"); b != guard.BlockSaturated {
		t.Fatalf("third begin = %q, want saturated", b)
	}
	if !g.Blocked("p") {
		t.Fatal("Blocked should report saturation")
	}
	// Other providers are unaffected — isolation is the point.
	if _, b := g.Begin("other"); b != guard.BlockNone {
		t.Fatalf("other provider blocked: %s", b)
	}
	a1.End(true)
	if _, b := g.Begin("p"); b != guard.BlockNone {
		t.Fatalf("after release still blocked: %s", b)
	}
	a2.End(true)
}

func TestEndIsIdempotent(t *testing.T) {
	g := guard.New(guard.Options{Concurrency: 1})
	a, _ := g.Begin("p")
	a.End(true)
	a.End(true)
	a.Release()
	if got := g.Snapshot("p").InFlight; got != 0 {
		t.Fatalf("inFlight = %d, want 0", got)
	}
	// A double End must not have freed a slot it never held.
	a2, _ := g.Begin("p")
	if _, b := g.Begin("p"); b != guard.BlockSaturated {
		t.Fatalf("slot over-released: %q", b)
	}
	a2.End(true)
}

func TestBreakerOpensHalfOpensAndRecovers(t *testing.T) {
	c := &clock{now: time.Unix(1_000, 0)}
	g := guard.New(guard.Options{Concurrency: 8, FailureThreshold: 3, Cooldown: 30 * time.Second, Now: c.Now})

	for i := 0; i < 3; i++ {
		a, b := g.Begin("p")
		if b != guard.BlockNone {
			t.Fatalf("attempt %d blocked early: %s", i, b)
		}
		a.End(false)
	}
	if _, b := g.Begin("p"); b != guard.BlockBreakerOpen {
		t.Fatalf("after threshold = %q, want breaker-open", b)
	}
	if !g.Snapshot("p").Open {
		t.Fatal("snapshot should be open")
	}

	c.Advance(31 * time.Second)
	if g.Blocked("p") {
		t.Fatal("Blocked must not claim or deny the probe once cooldown passed")
	}
	probe, b := g.Begin("p")
	if b != guard.BlockNone {
		t.Fatalf("half-open probe refused: %s", b)
	}
	// While the probe is out, everyone else waits for its verdict.
	if _, b := g.Begin("p"); b != guard.BlockBreakerOpen {
		t.Fatalf("second caller during probe = %q", b)
	}
	if !g.Blocked("p") {
		t.Fatal("Blocked should be true while probe is out")
	}
	probe.End(true)
	if _, b := g.Begin("p"); b != guard.BlockNone {
		t.Fatalf("recovered provider still blocked: %s", b)
	}
	if s := g.Snapshot("p"); s.Failures != 0 || s.Open {
		t.Fatalf("snapshot after recovery = %+v", s)
	}
}

func TestFailedProbeReopens(t *testing.T) {
	c := &clock{now: time.Unix(1_000, 0)}
	g := guard.New(guard.Options{FailureThreshold: 1, Cooldown: 10 * time.Second, Now: c.Now})
	a, _ := g.Begin("p")
	a.End(false)
	c.Advance(11 * time.Second)
	probe, b := g.Begin("p")
	if b != guard.BlockNone {
		t.Fatalf("probe refused: %s", b)
	}
	probe.End(false)
	if _, b := g.Begin("p"); b != guard.BlockBreakerOpen {
		t.Fatalf("failed probe should reopen, got %q", b)
	}
}

func TestReleaseDoesNotJudgeHost(t *testing.T) {
	g := guard.New(guard.Options{FailureThreshold: 1})
	a, _ := g.Begin("p")
	a.Release() // client hung up — not the host's fault
	if s := g.Snapshot("p"); s.Failures != 0 || s.Open || s.InFlight != 0 {
		t.Fatalf("release judged host: %+v", s)
	}
}

func TestConcurrentBeginNeverExceedsCap(t *testing.T) {
	const cap = 4
	g := guard.New(guard.Options{Concurrency: cap})
	var mu sync.Mutex
	held, peak := 0, 0
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, b := g.Begin("p")
			if b != guard.BlockNone {
				return
			}
			mu.Lock()
			held++
			if held > peak {
				peak = held
			}
			mu.Unlock()
			time.Sleep(time.Millisecond)
			mu.Lock()
			held--
			mu.Unlock()
			a.End(true)
		}()
	}
	wg.Wait()
	if peak > cap {
		t.Fatalf("peak in-flight %d exceeded cap %d", peak, cap)
	}
}

func TestOptionsFromEnvClamps(t *testing.T) {
	t.Setenv("JEVONIAN_PROVIDER_CONCURRENCY", "0")
	t.Setenv("JEVONIAN_PROVIDER_BREAKER_THRESHOLD", "9999")
	t.Setenv("JEVONIAN_PROVIDER_BREAKER_COOLDOWN_MS", "nope")
	o := guard.OptionsFromEnv()
	if o.Concurrency != 1 || o.FailureThreshold != 100 || o.Cooldown != guard.DefaultCooldown {
		t.Fatalf("opts = %+v", o)
	}
}
