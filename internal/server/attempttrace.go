package server

import (
	"fmt"
	"github.com/xinyao27/jevonian/internal/upstream"
	"github.com/xinyao27/jevonian/internal/wire"
)

// Trace retries as they happen, rather than synthesizing timings after Try.
func (t *turn) beginAttempt() {
	if t.trace == nil || t.decision == nil {
		return
	}
	cause := "initial"
	if r, ok := t.trace.Get(t.requestID); ok && len(r.Tries) > 0 {
		cause = "failover"
	}
	t.trace.BeginTry(t.requestID, t.decision.Provider, t.decision.Model, cause, t.decision.Effort)
}
func (t *turn) retryAttempt(info upstream.RetryAttempt) {
	if t.trace == nil || t.decision == nil {
		return
	}
	t.trace.EndTry(t.requestID, 0, wire.TruncateRunes(info.Failure.String(), 120))
	t.trace.BeginTry(t.requestID, t.decision.Provider, t.decision.Model, "retry", t.decision.Effort)
}
func (t *turn) endAttempt(at upstream.Attempt) {
	if t.trace == nil {
		return
	}
	fail := fmt.Sprintf("http-%d", at.Status)
	switch at.Outcome.Kind {
	case upstream.OutcomeQuotaRefusal:
		fail = "quota"
	case upstream.OutcomeContextOverflow:
		fail = "context-overflow"
	case upstream.OutcomeCanceled:
		fail = "client-canceled"
	default:
		if at.Err != nil {
			fail = "fetch: " + upstream.DescribeError(at.Err)
		}
	}
	t.trace.EndTry(t.requestID, at.Status, wire.TruncateRunes(fail, 120))
}
