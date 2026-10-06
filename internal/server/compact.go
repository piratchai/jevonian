package server

import (
	"context"

	"github.com/xinyao27/jevonian/internal/brain"
	"github.com/xinyao27/jevonian/internal/compaction"
	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/routing"
	"github.com/xinyao27/jevonian/internal/upstream"
	responseswire "github.com/xinyao27/jevonian/internal/wire/responses"
)

// Server-side wiring of the routing/compaction logic, mirroring
// src/upstream.ts: remote-compaction pinning, proactive compaction on a
// Decision.ContextOverflow, and one compact-and-retry after a provider's hard
// context rejection.

// remoteCompaction reports a Codex remote-compaction v2 turn: a Responses
// request carrying a compaction trigger. Those only a native Responses host
// can answer.
func (t *turn) remoteCompaction() bool {
	return t.kind == upstream.KindResponses && responseswire.IsRemoteCompactionV2(t.body)
}

// initialDecision pins a remote-compaction turn to the ChatGPT Responses
// provider, never touching the brain; every other turn goes through Decide.
func (t *turn) initialDecision(ctx context.Context) (*routing.Decision, error) {
	if t.remoteCompaction() {
		return routing.RemoteCompactionDecision(t.cfg, t.routingDeps(), t.body, t.headers, t.requestID)
	}
	return t.decide(ctx)
}

// brainClient is the client compaction asks its raw questions through.
func (s *Server) brainClient() *brain.Client {
	if s.deps.Brain != nil {
		return s.deps.Brain
	}
	return &brain.Client{HTTP: s.deps.Client, Sleep: s.deps.Sleep}
}

// compactAndReroute shrinks the request body and re-routes it. It returns
// true when the body was rewritten and a new decision took the turn. On any
// failure the original body and decision stay, so a brain outage can never
// rewrite history or reject a request the provider might still fit.
// afterRejection stamps the `context-retry` reason suffix.
func (t *turn) compactAndReroute(ctx context.Context, afterRejection bool) bool {
	brains := t.cfg.Routing.Brains
	asker := compaction.BrainAsker{Client: t.srv.brainClient(), Brains: brains}
	res := compaction.CompactForOverflow(ctx, len(brains), asker, t.body)
	if !res.OK {
		return false
	}
	retry, err := t.decideBody(ctx, res.Body)
	if err != nil {
		return false
	}
	t.body = res.Body
	if afterRejection {
		retry.Reason = routing.WithContextRetry(retry.Reason)
	}
	t.decision = retry
	return true
}

// maxOutput is the model's output ceiling: the catalog's stated cap with the
// user's routing.capacities override layered over it.
// src/prepare.ts effectiveCapabilities(model, config.routing.capacities[model]).
func (t *turn) maxOutput(model string) int {
	var stated routing.ModelCapabilities
	if src := t.srv.deps.Routing.Capabilities; src != nil {
		stated = src(model)
	}
	var override *config.ModelCapacityConfig
	if c, ok := t.cfg.Routing.Capacities[model]; ok {
		override = &c
	}
	return routing.EffectiveCapabilities(model, stated, override).MaxOutput
}
