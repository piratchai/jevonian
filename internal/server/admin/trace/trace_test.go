package trace

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func initFor(id string) Init {
	return Init{RequestID: id, Session: "sess-1", Path: "/chat/completions", RequestedModel: "jevonian/auto", Stream: true}
}

func strp(s string) *string { return &s }

func TestBeginRouteAndSeq(t *testing.T) {
	s := New(nil)
	started := s.BeginRoute(Init{RequestID: "req-1", Session: "sess-1", ClientSession: "client-session"})
	if started.Done || len(started.Tries) != 0 || started.Failovers != 0 || started.Seq <= 0 {
		t.Fatalf("unexpected start: %+v", started)
	}
	if started.ClientSession != "client-session" {
		t.Fatal("client session lost")
	}
	s.Weigh("req-1", []Candidate{{Provider: "p", Model: "m"}})
	w, _ := s.Get("req-1")
	if w.Seq <= started.Seq {
		t.Fatal("weigh did not bump seq")
	}
	s.BeginTry("req-1", "p", "m", "initial", "")
	a, _ := s.Get("req-1")
	if a.Seq <= w.Seq {
		t.Fatal("beginTry did not bump seq")
	}
	s.EndTry("req-1", 200, "")
	e, _ := s.Get("req-1")
	if e.Seq <= a.Seq {
		t.Fatal("endTry did not bump seq")
	}
}

func TestTries(t *testing.T) {
	s := New(nil)
	s.BeginRoute(initFor("req-1"))
	s.BeginTry("req-1", "a", "m1", "initial", "")
	s.EndTry("req-1", 429, "quota")
	s.BeginTry("req-1", "b", "m2", "failover", "")
	s.EndTry("req-1", 200, "")
	r, _ := s.Get("req-1")
	if r.Failovers != 1 || len(r.Tries) != 2 {
		t.Fatalf("got %+v", r)
	}
	if r.Tries[0].Fail != "quota" || *r.Tries[0].Status != 429 || !r.Tries[0].Done {
		t.Fatalf("first try %+v", r.Tries[0])
	}
	if r.Tries[1].Cause != "failover" || *r.Tries[1].Status != 200 {
		t.Fatalf("second try %+v", r.Tries[1])
	}

	s.BeginRoute(initFor("req-2"))
	s.BeginTry("req-2", "a", "m1", "initial", "")
	s.EndTry("req-2", 0, "http-502")
	s.BeginTry("req-2", "a", "m1", "retry", "")
	s.EndTry("req-2", 200, "")
	r2, _ := s.Get("req-2")
	if r2.Failovers != 0 || r2.Tries[0].Cause != "initial" || r2.Tries[1].Cause != "retry" {
		t.Fatalf("retry counted as failover: %+v", r2)
	}

	s.BeginRoute(initFor("req-3"))
	s.EndTry("req-3", 200, "")
	if r3, _ := s.Get("req-3"); len(r3.Tries) != 0 {
		t.Fatal("endTry without open attempt should no-op")
	}
	s.BeginTry("req-3", "a", "m1", "initial", "")
	if r3, _ := s.Get("req-3"); r3.Tries[0].Done || r3.Tries[0].Ms != nil {
		t.Fatal("in-flight attempt should stay open")
	}
}

func TestFirstToken(t *testing.T) {
	now := time.UnixMilli(1_000_000)
	s := New(func() time.Time { return now })
	s.BeginRoute(Init{RequestID: "req-1", Session: "s", StartedAt: now.UnixMilli() - 25})
	s.BeginTry("req-1", "a", "m1", "initial", "")
	s.FirstToken("req-1")
	r, _ := s.Get("req-1")
	if r.TTFTMs == nil || *r.TTFTMs < 20 || r.Tries[0].TTFTMs == nil {
		t.Fatalf("ttft %+v", r)
	}
	now = now.Add(time.Second)
	s.FirstToken("req-1")
	r2, _ := s.Get("req-1")
	if *r2.TTFTMs != *r.TTFTMs {
		t.Fatal("second FirstToken moved the measurement")
	}
	s.FirstToken("missing")
}

func TestFinishRoute(t *testing.T) {
	s := New(nil)
	s.BeginRoute(initFor("req-1"))
	s.BeginTry("req-1", "a", "m1", "initial", "")
	r, ok := s.FinishRoute("req-1", 200, "")
	if !ok || !r.Done || *r.Status != 200 || r.Ms == nil || !r.Tries[0].Done || *r.Tries[0].Status != 200 {
		t.Fatalf("finish %+v", r)
	}
	stored, _ := s.Get("req-1")
	if stored.Seq != r.Seq {
		t.Fatalf("returned seq %d != stored %d", r.Seq, stored.Seq)
	}

	s.BeginRoute(initFor("req-2"))
	s.BeginTry("req-2", "a", "m1", "initial", "")
	s.FinishRoute("req-2", 499, "client canceled")
	r2, _ := s.Get("req-2")
	if r2.Error != "client canceled" || r2.Tries[0].Fail != "client-canceled" {
		t.Fatalf("cancel %+v", r2)
	}
	if _, ok := s.FinishRoute("missing", 200, ""); ok {
		t.Fatal("unknown trace finished")
	}
}

func TestNoteDecision(t *testing.T) {
	s := New(nil)
	s.BeginRoute(Init{RequestID: "req-1", Session: "s", Phase: "plan", Reason: "brain:plan"})
	s.NoteDecision("req-1", Decision{Phase: strp("execute"), Reason: strp("brain:execute:quota-failover")})
	r, _ := s.Get("req-1")
	if r.Phase != "execute" || r.Reason != "brain:execute:quota-failover" {
		t.Fatalf("%+v", r)
	}
}

func TestLookup(t *testing.T) {
	s := New(nil)
	s.BeginRoute(initFor("old"))
	s.BeginRoute(initFor("new"))
	if r, _ := s.LatestFor("sess-1", nil); r.RequestID != "new" {
		t.Fatal("latest is not newest")
	}
	recent := s.Recent(Keep)
	if recent[0].RequestID != "new" || recent[1].RequestID != "old" {
		t.Fatal("recent not newest-first")
	}
	s.BeginRoute(Init{RequestID: "mine", Session: "k", KeyID: "key-a"})
	if r, ok := s.LatestFor("k", strp("key-a")); !ok || r.RequestID != "mine" {
		t.Fatal("key scope lost own trace")
	}
	if _, ok := s.LatestFor("k", strp("key-b")); ok {
		t.Fatal("key scope leaked")
	}
}

func TestWaitForSession(t *testing.T) {
	ctx := context.Background()
	s := New(nil)
	started := s.BeginRoute(initFor("req-1"))
	if r, ok := s.WaitForSession(ctx, "sess-1", nil, started.Seq-1, 5*time.Second); !ok || r.RequestID != "req-1" {
		t.Fatal("should resolve immediately")
	}

	done := make(chan Route, 1)
	go func() {
		r, _ := s.WaitForSession(ctx, "sess-1", nil, started.Seq, 5*time.Second)
		done <- r
	}()
	time.Sleep(20 * time.Millisecond)
	s.BeginTry("req-1", "a", "m1", "initial", "")
	select {
	case r := <-done:
		if len(r.Tries) != 1 {
			t.Fatalf("woke with %+v", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter not woken")
	}

	cur, _ := s.Get("req-1")
	if _, ok := s.WaitForSession(ctx, "sess-1", nil, cur.Seq, 10*time.Millisecond); ok {
		t.Fatal("expected timeout")
	}

	keyed := s.BeginRoute(Init{RequestID: "k1", Session: "sk", KeyID: "key-a"})
	if _, ok := s.WaitForSession(ctx, "sk", strp("key-b"), keyed.Seq, 10*time.Millisecond); ok {
		t.Fatal("other key woke waiter")
	}
}

func TestSubscribe(t *testing.T) {
	s := New(nil)
	var seen []string
	var snapshot Route
	unsub := s.Subscribe(func(r Route) { seen = append(seen, r.RequestID); snapshot = r })
	s.BeginRoute(initFor("req-1"))
	snapshot.Tries = append(snapshot.Tries, Try{Provider: "x"})
	s.BeginTry("req-1", "a", "m1", "initial", "")
	if len(seen) != 2 {
		t.Fatalf("seen %v", seen)
	}
	unsub()
	s.EndTry("req-1", 200, "")
	if len(seen) != 2 {
		t.Fatal("unsubscribe leaked")
	}
	if r, _ := s.Get("req-1"); len(r.Tries) != 1 {
		t.Fatal("listener mutated store")
	}
}

func TestRingBuffer(t *testing.T) {
	s := New(nil)
	for i := 0; i < Keep+20; i++ {
		s.BeginRoute(initFor(fmt.Sprintf("req-%d", i)))
	}
	recent := s.Recent(Keep + 20)
	if len(recent) != Keep || recent[0].RequestID != fmt.Sprintf("req-%d", Keep+19) {
		t.Fatalf("ring: %d %s", len(recent), recent[0].RequestID)
	}

	s2 := New(nil)
	live := s2.BeginRoute(initFor("live"))
	for i := 0; i < Keep+5; i++ {
		id := fmt.Sprintf("done-%d", i)
		s2.BeginRoute(Init{RequestID: id, Session: id})
		s2.FinishRoute(id, 200, "")
	}
	if r, ok := s2.Get("live"); !ok || r.Seq != live.Seq {
		t.Fatal("in-flight trace dropped")
	}
}
