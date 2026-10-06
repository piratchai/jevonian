package routing

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/quota"
	"github.com/xinyao27/jevonian/internal/upstream"
)

// routingOffer is one routing's narrowed pool plus the brain-facing view.
// src/routing.ts RoutingOffer.
type routingOffer struct {
	id             string
	label          string
	description    string
	candidates     []CapableCandidate
	offeredToBrain []CacheCandidateView
	skipped        []RouteSkip
}

// benchmarkFocusFor picks the boards this turn should weigh.
// src/leaderboard.ts benchmarkFocusFor.
func benchmarkFocusFor(signals PhaseSignals, routingID string) map[string]any {
	focus := func(domain string, boards []string, reason string) map[string]any {
		return map[string]any{"prefer_domain": domain, "prefer_boards": boards, "reason": reason}
	}
	if signals.ConsecutiveFailures > 0 {
		return focus("agent", []string{
			"terminal-bench", "terminal-bench-hard", "swe-bench-verified", "toolathlon",
			"artificial-analysis-coding-agent-index",
		}, "recent tool failures — terminal / SWE / agent coding indices first")
	}
	if routingID == "chat" {
		return focus("general", []string{
			"agents-last-exam", "artificial-analysis-coding-agent-index", "osworld-verified",
		}, "chat routing — agent quality over pure coding benches")
	}
	if signals.HasTools || signals.HasToolResults || routingID == "execute" {
		return focus("code", []string{
			"swe-bench-verified", "swe-bench-pro", "terminal-bench", "aider-polyglot",
			"toolathlon", "artificial-analysis-coding-index",
		}, "agentic / tool turn — SWE, terminal, aider, tool-use boards")
	}
	if routingID == "plan" {
		return focus("code", []string{
			"swe-bench-verified", "swe-bench-pro", "artificial-analysis-coding-agent-index",
			"mcp-atlas",
		}, "planning turn — SWE and agent boards over chat defaults")
	}
	return focus("code", []string{
		"swe-bench-verified", "swe-bench-pro", "terminal-bench",
		"artificial-analysis-coding-index",
	}, "default — coding boards from models.dev")
}

// decideBrain: code narrows each routing's pool; the scorer picks a routing;
// the model is the first healthy entry. src/routing.ts decideRoute brain path.
func (t *turnCtx) decideBrain(ctx context.Context, requestedRaw, requestID, session string, turns int) (*Decision, error) {
	cfg, deps := t.cfg, t.deps
	brains := cfg.Routing.Brains
	if len(brains) == 0 {
		return nil, &RouteError{
			Status:  400,
			Message: "No Jev brain is configured. Add one under Providers → Routing brain, or request an explicit routing (jevonian/plan, …) or a concrete model.",
		}
	}

	guard := cfg.Routing.QuotaGuard
	conversationTokens := CompactionEstimate(t.input.Body)
	requestedEffort := headerEffort(t.input.Headers)
	defaultEffort := brainEffort(cfg.Routing.DefaultEffort)
	var minEffort string
	if requestedEffort != "" {
		minEffort = requestedEffort
	} else if !cfg.Routing.BrainPicksEffort {
		minEffort = defaultEffort
	}

	allSkipped := []RouteSkip{}
	offers := []routingOffer{}
	executeQuotaFiltered := false
	buildOffer := func(entry config.RoutingEntry, pool []TierPick) {
		capable := CapableCandidates(cfg, deps, pool)
		usable, skipped := PartitionByCapability(capable, conversationTokens, minEffort, true)
		allSkipped = append(allSkipped, skipped...)
		offered := usable
		if len(offered) == 0 {
			offered = capable
		}
		picks := make([]TierPick, len(offered))
		for i, c := range offered {
			picks[i] = c.TierPick
		}
		offers = append(offers, routingOffer{
			id: entry.ID, label: entry.Label, description: entry.Description,
			candidates:     offered,
			offeredToBrain: cacheCandidates(deps, picks, t.previous, t.now, int64(conversationTokens), t.cacheTTLMs),
			skipped:        skipped,
		})
	}

	for _, entry := range t.routings {
		declared := t.candidatesFor(entry.ID)
		healthy := declared
		if guard.Enabled {
			healthy = []TierPick{}
			for _, c := range declared {
				if !candidateExhausted(cfg, deps, c, t.now) {
					healthy = append(healthy, c)
				}
			}
		}
		// Skip a routing whose every provider is spent while others have room.
		if len(healthy) == 0 {
			if entry.ID == "execute" && len(declared) > 0 && guard.Enabled {
				executeQuotaFiltered = true
			}
			continue
		}
		buildOffer(entry, healthy)
	}
	if len(offers) == 0 {
		for _, entry := range t.routings {
			declared := t.candidatesFor(entry.ID)
			if len(declared) == 0 {
				continue
			}
			buildOffer(entry, declared)
		}
	}
	if len(offers) == 0 {
		return nil, &RouteError{Message: "No models available for routing. Configure routing.routings or add models to a provider."}
	}

	// Collapse duplicate skip notes: the same model can sit in several routings.
	skippedSeen := map[string]bool{}
	skipped := []RouteSkip{}
	for _, s := range allSkipped {
		k := s.Provider + "/" + s.Model + "/" + s.Reason
		if skippedSeen[k] {
			continue
		}
		skippedSeen[k] = true
		skipped = append(skipped, s)
	}

	contextOverflow := true
	for _, o := range offers {
		if len(o.candidates) > 0 {
			contextOverflow = false
			break
		}
	}
	if !contextOverflow {
		hasContext := false
		for _, s := range allSkipped {
			if s.Reason == "context" {
				hasContext = true
				break
			}
		}
		if hasContext {
			contextOverflow = true
			for _, o := range offers {
				found := false
				for _, s := range o.skipped {
					if s.Reason == "context" {
						found = true
						break
					}
				}
				if !found {
					contextOverflow = false
					break
				}
			}
		}
	}

	flatOffered := []CacheCandidateView{}
	seenFlat := map[string]bool{}
	for _, o := range offers {
		for _, c := range o.offeredToBrain {
			k := PlanKey(c.Provider, c.Model)
			if seenFlat[k] {
				continue
			}
			seenFlat[k] = true
			flatOffered = append(flatOffered, c)
		}
	}
	var previousCandidate *CacheCandidateView
	if t.previous != nil {
		for i := range flatOffered {
			c := flatOffered[i]
			if c.Provider == t.previous.Provider && c.Model == t.previous.Model {
				previousCandidate = &flatOffered[i]
				break
			}
		}
	}
	if previousCandidate != nil {
		for i := range flatOffered {
			c := &flatOffered[i]
			if c.Cache.EffectiveInputCostUSD != nil && previousCandidate.Cache.EffectiveInputCostUSD != nil {
				diff := *c.Cache.EffectiveInputCostUSD - *previousCandidate.Cache.EffectiveInputCostUSD
				c.SwitchPenaltyUSD = &diff
			} else {
				c.SwitchPenaltyUSD = nil
			}
		}
		for i := range offers {
			for j := range offers[i].offeredToBrain {
				c := &offers[i].offeredToBrain[j]
				if c.Cache.EffectiveInputCostUSD != nil && previousCandidate.Cache.EffectiveInputCostUSD != nil {
					diff := *c.Cache.EffectiveInputCostUSD - *previousCandidate.Cache.EffectiveInputCostUSD
					c.SwitchPenaltyUSD = &diff
				} else {
					c.SwitchPenaltyUSD = nil
				}
			}
		}
	}

	brainState := map[string]any{
		"last_user_message":    LastUserMessage(t.input.Body, t.input.Kind),
		"recent_messages":      recentMessages(t.input.Body, t.input.Kind, 4),
		"recent_tool_calls":    recentToolCalls(t.input.Body, t.input.Kind, 3),
		"recent_tool_results":  t.signals.RecentToolResults,
		"has_tool_results":     t.signals.HasToolResults,
		"has_tools":            t.signals.HasTools,
		"consecutive_failures": t.signals.ConsecutiveFailures,
		"session_turns":        turns,
		"message_count":        messageCount(t.input.Body),
		"estimated_tokens":     conversationTokens,
	}
	if last := assistantMessages(t.input.Body, t.input.Kind); len(last) > 0 {
		// TS: .replace(/\s+/g, " ").trim().slice(0, 500); an all-blank tail is omitted.
		if text := truncateRunes(strings.TrimSpace(whitespaceRe.ReplaceAllString(last[len(last)-1], " ")), 500); text != "" {
			brainState["last_assistant_message"] = text
		}
	}
	if goal := sessionGoal(t.input.Body, t.input.Kind); goal != "" {
		brainState["session_goal"] = goal
	}
	if t.previous != nil {
		brainState["previous_model"] = t.previous.Model
		brainState["previous_routing"] = t.previous.Phase
	}
	if t.keepReason != "" {
		brainState["cache_keep"] = string(t.keepReason)
	}
	if minEffort != "" {
		brainState["requested_effort"] = minEffort
	}
	if len(skipped) > 0 {
		brainState["skipped"] = skipped
	}

	// Benchmark soft evidence: only when the leaderboard knows scores.
	// src/routing.ts includeBenchmarkHints.
	firstViews := make([]map[string]any, len(offers))
	hits := 0
	if deps.LeaderboardView != nil {
		for i, o := range offers {
			if len(o.offeredToBrain) > 0 {
				firstViews[i] = deps.LeaderboardView(o.offeredToBrain[0].Model)
			}
		}
		for _, v := range firstViews {
			if v != nil {
				hits++
			}
		}
	}
	coverage := "none"
	switch {
	case hits == len(offers) && hits > 0:
		coverage = "full"
	case hits > 0:
		coverage = "partial"
	}
	if coverage != "none" {
		brainState["benchmark_focus"] = benchmarkFocusFor(t.signals, "")
		brainState["benchmarks_coverage"] = coverage
	}

	routingPayload := make([]any, 0, len(offers))
	for _, o := range offers {
		models := make([]any, 0, len(o.offeredToBrain))
		for i, c := range o.offeredToBrain {
			m := map[string]any{
				"model":            c.Model,
				"provider":         c.Provider,
				"preference_rank":  i + 1,
				"cache":            c.Cache,
				"switchPenaltyUsd": c.SwitchPenaltyUSD,
			}
			if c.Canonical != "" {
				m["canonical"] = c.Canonical
			}
			// Benchmark scores only attach to the first model — the one this
			// routing would actually serve. src/routing.ts routingPayload.
			if i == 0 && deps.LeaderboardView != nil {
				if b := deps.LeaderboardView(c.Model); b != nil {
					m["benchmarks"] = b
				}
			}
			models = append(models, m)
		}
		entry := map[string]any{
			"id": o.id, "label": o.label, "description": o.description, "models": models,
		}
		if coverage != "none" {
			entry["benchmark_focus"] = benchmarkFocusFor(t.signals, o.id)
		}
		routingPayload = append(routingPayload, entry)
	}

	wantsTranscript := false
	for _, b := range brains {
		if b.FullPrompt {
			wantsTranscript = true
			break
		}
	}
	var transcript string
	if wantsTranscript {
		transcript = FullTranscript(t.input.Body, t.input.Kind)
	}

	skipChannels := map[string]bool{}
	for _, b := range brains {
		if linked := providerByName(cfg, b.Channel); linked != nil {
			if quotaStatus(deps, *linked, "", t.now, guard.LowPercent).Status == quota.StatusExhausted {
				skipChannels[b.Channel] = true
			}
		}
	}

	var best *brainPick
	if deps.BrainBreakerOpen != nil && deps.BrainBreakerOpen() {
		// Skip the brain round entirely.
	} else {
		// One round of channels, not the full upstream budget: a brain that is
		// down must fail fast into the heuristic. src/routing.ts brainBudget.
		for round := 0; round < 2 && best == nil; round++ {
			for _, b := range brains {
				if skipChannels[b.Channel] {
					continue
				}
				started := t.now
				if deps.Now != nil {
					started = deps.Now()
				}
				ready := map[string]any{}
				for k, v := range brainState {
					ready[k] = v
				}
				ready["routings"] = routingPayload
				// Flat candidates mirror for older brain stubs.
				ready["candidates"] = flatOffered
				if !cfg.Routing.BrainPicksEffort {
					ready["picks_effort"] = false
				}
				state := StateForBrain(b, ready, transcript)
				var outcome AskResult
				if deps.Scorer != nil {
					outcome = deps.Scorer.Score(ctx, b, state, !cfg.Routing.BrainPicksEffort)
				} else {
					outcome = AskResult{Failure: &Failure{Error: "no scorer configured"}}
				}
				if deps.RecordBrainCall != nil {
					latency := int64(0)
					if deps.Now != nil {
						latency = deps.Now() - started
					}
					deps.RecordBrainCall(BrainCallRecord{
						Brain: b, Session: session, RequestID: requestID,
						KeyID: t.input.KeyID, KeyName: t.input.KeyName,
						Started: started, State: state,
						Verdict: outcome.Choice, Failure: outcome.Failure,
						LatencyMs: latency,
					})
				}
				if outcome.Choice == nil {
					// 402 = no credits; 403 = WAF/auth. Retrying this channel
					// cannot recover within the turn. src/routing.ts.
					if outcome.Failure != nil {
						if outcome.Failure.Status == 402 || outcome.Failure.Status == 403 {
							skipChannels[b.Channel] = true
						}
						if outcome.Failure.Status == 402 && deps.CaptureUsageLimit != nil {
							if linked := providerByName(cfg, b.Channel); linked != nil {
								deps.CaptureUsageLimit(*linked, 402, outcome.Failure.Error)
							}
						}
					}
					continue
				}
				best = &brainPick{verdict: outcome.Choice, channel: b.Channel, brain: b}
				break
			}
			if best == nil && round == 0 {
				// The whole-round retry uses the same transient backoff as upstream
				// calls: attempt 2's delay. src/routing.ts withRetry over the round.
				t.sleep(time.Duration(upstream.RetryDelayMS(2)) * time.Millisecond)
			}
		}
	}
	if deps.RecordBrainOutcome != nil {
		deps.RecordBrainOutcome(best != nil)
	}

	reason := ""
	var source BrainSource = BrainJev
	var confidence float64
	hasConfidence := false
	brainChannel := ""
	if best == nil {
		// Prefer staying up over failing the agent: a heuristic pick from
		// classifyPhase is far cheaper than a 502 that freezes the client.
		// src/routing.ts the heuristic fallback.
		var preferred *routingOffer
		for i := range offers {
			if offers[i].id == t.signals.Phase {
				preferred = &offers[i]
				break
			}
		}
		if preferred == nil && t.signals.HasToolResults {
			for i := range offers {
				if offers[i].id == "execute" {
					preferred = &offers[i]
					break
				}
			}
		}
		if preferred == nil {
			for i := range offers {
				if offers[i].id == "plan" {
					preferred = &offers[i]
					break
				}
			}
		}
		if preferred == nil && len(offers) > 0 {
			preferred = &offers[0]
		}
		if preferred == nil || len(preferred.candidates) == 0 {
			return nil, &RouteError{
				Status:  502,
				Message: fmt.Sprintf("Jev brain unavailable: all %d configured brain(s) failed.", len(brains)),
			}
		}
		best = &brainPick{verdict: &Choice{Model: preferred.id}, channel: "heuristic"}
		source = BrainHeuristic
		brainChannel = "heuristic"
		reason = ReasonBrainFallback(preferred.id)
	} else {
		confidence = best.verdict.Confidence
		hasConfidence = true
		brainChannel = best.channel
		if confidence < best.brain.MinConfidence {
			source = BrainJevLowConfidence
			reason = AppendReason(ReasonBrain(best.verdict.Model), SuffixBrainLowConfidence)
		} else {
			reason = ReasonBrain(best.verdict.Model)
		}
	}

	chosenID := ""
	if best.verdict.Model != "" && best.verdict.Model != "none_of_the_above" {
		chosenID = best.verdict.Model
	}
	var offer *routingOffer
	for i := range offers {
		if offers[i].id == chosenID {
			offer = &offers[i]
			break
		}
	}
	if offer == nil {
		offer = &offers[0]
	}
	if offer == nil {
		return nil, &RouteError{
			Status:  502,
			Message: fmt.Sprintf("Jev chose %q, which is not an available routing.", best.verdict.Model),
		}
	}
	phase := offer.id
	if t.signals.HasToolResults && t.signals.Phase == "execute" && offer.id != "execute" && executeQuotaFiltered {
		phase = "execute"
		if source == BrainHeuristic {
			reason = ReasonBrainFallback(phase)
		}
		reason = AppendReason(reason, SuffixQuotaFallback)
	}
	if len(offer.candidates) == 0 {
		return nil, &RouteError{Status: 502, Message: fmt.Sprintf("Routing %q has no available models.", offer.id)}
	}
	chosen := offer.candidates[0].TierPick
	var chosenCandidate *CacheCandidateView
	for i := range offer.offeredToBrain {
		c := offer.offeredToBrain[i]
		if c.Provider == chosen.Provider && c.Model == chosen.Model {
			chosenCandidate = &offer.offeredToBrain[i]
			break
		}
	}

	declaredCandidates := map[string]bool{}
	for _, e := range t.routings {
		for _, c := range RoutingCandidates(cfg, deps, e, t.input.Kind) {
			declaredCandidates[PlanKey(c.Provider, c.Model)] = true
		}
	}
	offeredCandidates := map[string]bool{}
	for _, o := range offers {
		for _, c := range o.candidates {
			offeredCandidates[PlanKey(c.Provider, c.Model)] = true
		}
	}
	for k := range declaredCandidates {
		if !offeredCandidates[k] {
			reason = AppendReason(reason, SuffixQuotaSkip)
			break
		}
	}
	for _, s := range skipped {
		if s.Reason == "context" {
			reason = AppendReason(reason, SuffixContextSkip)
			break
		}
	}
	for _, s := range skipped {
		if s.Reason == "effort" {
			reason = AppendReason(reason, SuffixEffortSkip)
			break
		}
	}
	if chosen.Canonical != "" {
		reason = withCanonical(reason, chosen.Canonical)
	}
	if chosenCandidate != nil {
		switch chosenCandidate.Cache.State {
		case CacheHot:
			reason = AppendReason(reason, SuffixCacheHot)
		case CacheStale:
			reason = AppendReason(reason, SuffixCacheStale)
		}
	}
	if t.keepApplied && t.keepReason != "" {
		reason = withCacheKeep(reason, t.keepReason)
	}

	tierEffort := t.tierEffort(phase)
	wanted := tierEffort
	if wanted == "" && cfg.Routing.BrainPicksEffort {
		wanted = brainEffort(best.verdict.Effort)
	}
	fallback := wanted
	if fallback == "" {
		fallback = requestedEffort
	}
	if fallback == "" {
		fallback = defaultEffort
	}
	applied := ClampEffort(fallback, t.effortsOf(chosen.Model))
	note := ""
	switch {
	case wanted != "" && applied != "" && wanted != applied:
		note = fmt.Sprintf("clamped %q to %q", wanted, applied)
	case tierEffort != "" && applied != "":
		note = fmt.Sprintf("tier set %q", tierEffort)
	case requestedEffort != "" && applied != "" && requestedEffort != applied:
		note = fmt.Sprintf("requested %q, model supports %q", requestedEffort, applied)
	}
	if note != "" {
		reason = AppendReason(reason, SuffixEffortClamped)
	}

	t.commit(session, phase, chosen, turns)

	others := [][]TierPick{}
	for _, o := range offers {
		if o.id != offer.id {
			picks := make([]TierPick, len(o.candidates))
			for i, c := range o.candidates {
				picks[i] = c.TierPick
			}
			others = append(others, picks)
		}
	}
	chosenPicks := make([]TierPick, len(offer.candidates))
	for i, c := range offer.candidates {
		chosenPicks[i] = c.TierPick
	}
	flatAll := []CacheCandidateView{}
	for _, o := range offers {
		flatAll = append(flatAll, o.offeredToBrain...)
	}
	d := &Decision{
		Model: chosen.Model, Provider: chosen.Provider, Phase: phase,
		RequestedModel: requestedRaw, Canonical: chosen.Canonical,
		Virtual: true, Routed: true, Reason: reason,
		Session: session, RequestID: requestID,
		Brain: source, BrainChannel: brainChannel,
		Confidence: confidence, HasConfidence: hasConfidence,
		Effort: applied, EffortNote: note,
		ContextOverflow: contextOverflow,
		CacheKeep:       t.keepReason,
		Order: weighedOrder(failoverPlan(chosen, chosenPicks, others), cfg, deps, t.now, flatAll,
			func() []RouteSkip {
				out := []RouteSkip{}
				for _, o := range offers {
					out = append(out, o.skipped...)
				}
				return out
			}()),
	}
	if len(skipped) > 0 {
		d.Skipped = skipped
	}
	if chosenCandidate != nil {
		c := chosenCandidate.Cache
		d.Cache = &c
		d.SwitchPenaltyUSD = chosenCandidate.SwitchPenaltyUSD
	}
	return d, nil
}

// brainPick is the answering channel entry and its verdict.
type brainPick struct {
	verdict *Choice
	channel string
	// brain is the exact entry that answered: low confidence is judged
	// against that entry's minConfidence. src/routing.ts entry.minConfidence.
	brain config.BrainConfig
}

// (The inter-round retry sleeps upstream.RetryDelayMS(2) — the same transient
// backoff as upstream calls. src/routing.ts withRetry over the channel round.)
