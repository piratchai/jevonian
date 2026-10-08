package routing

import "strings"

// Response header names the server stamps from a Decision.
// src/upstream.ts the decision header block (~line 430).
const (
	HeaderModel          = "x-jevonian-model"
	HeaderProvider       = "x-jevonian-provider"
	HeaderPhase          = "x-jevonian-phase"
	HeaderReason         = "x-jevonian-reason"
	HeaderEffort         = "x-jevonian-effort"
	HeaderEffortNote     = "x-jevonian-effort-note"
	HeaderSkipped        = "x-jevonian-skipped"
	HeaderSession        = "x-jevonian-session"
	HeaderRequestID      = "x-jevonian-request-id"
	HeaderBrain          = "x-jevonian-brain"
	HeaderBrainChannel   = "x-jevonian-brain-channel"
	HeaderCanonical      = "x-jevonian-canonical"
	HeaderCacheState     = "x-jevonian-cache-state"
	HeaderCacheKeep      = "x-jevonian-cache-keep"
	HeaderQuotaFailovers = "x-jevonian-quota-failovers"
	HeaderRetries        = "x-jevonian-retries"
	HeaderSoftError      = "x-jevonian-soft-error"
)

// Request headers routing reads. src/routing.ts firstHeader call sites.
const (
	RequestHeaderPhase           = "x-jevonian-phase"
	RequestHeaderTier            = "x-jevonian-tier" // legacy alias of x-jevonian-phase
	RequestHeaderEffort          = "x-jevonian-effort"
	RequestHeaderReasoningEffort = "x-reasoning-effort"
	RequestHeaderAffinity        = "x-jevonian-affinity"
	RequestHeaderSession         = "x-jevonian-session"
)

// Base reasons: the first segment of x-jevonian-reason.
//
// A reason is a colon-joined chain: one base, then any number of suffixes in
// the order the TS router appends them. The server must stamp Decision.Reason
// verbatim and only append the transport suffixes below.
const (
	// ReasonPinnedModel: a concrete model id was requested; no brain ran.
	// src/routing.ts decideRoute pinned path.
	ReasonPinnedModel = "pinned-model"
	// ReasonCanonicalModel: the requested id resolved via canonical/identity
	// spelling or modelAliases. src/routing.ts decideRoute variants path.
	ReasonCanonicalModel = "canonical-model"
	// ReasonRemoteCompaction: Codex compaction_trigger pinned to a Responses
	// provider. src/upstream.ts ~line 273 (stamped by upstream, not routing).
	ReasonRemoteCompaction = "remote-compaction"
)

// Base reason prefixes that carry a routing id.
const (
	// "explicit:<routing>" — jevonian/<id> or the x-jevonian-phase header.
	prefixExplicit = "explicit:"
	// "brain:<routing>" — a brain verdict picked the routing.
	prefixBrain = "brain:"
	// "brain-fallback:<routing>" — every brain channel failed; classifyPhase
	// picked. src/routing.ts heuristic fallback.
	prefixBrainFallback = "brain-fallback:"
)

// Suffixes routing appends. src/routing.ts decideRoute.
const (
	SuffixQuotaFallback      = "quota-fallback"       // explicit tier fully spent, widened to other routings
	SuffixNoCandidate        = "no-candidate"         // explicit tier had no candidate at all, widened
	SuffixQuotaSkip          = "quota-skip"           // some declared candidates were withheld
	SuffixContextSkip        = "context-skip"         // a candidate's window could not hold the turn
	SuffixEffortSkip         = "effort-skip"          // a candidate could not meet the effort floor
	SuffixToolSkip           = "tool-skip"            // a candidate has no native tool-calling channel
	SuffixBrainLowConfidence = "brain-low-confidence" // verdict below the channel's minConfidence
	SuffixEffortClamped      = "effort-clamped"       // effortNote set
	SuffixCacheHot           = "cache-hot"
	SuffixCacheStale         = "cache-stale"
	// "canonical:<requested>" — the chosen candidate came via canonical variants.
	prefixCanonicalSuffix = "canonical:"
	// "cache-<keepReason>" — cache affinity kept the conversation in place.
	prefixCacheKeepSuffix = "cache-"
)

// Suffixes the transport layer appends after routing. src/upstream.ts.
const (
	// SuffixQuotaFailover: a 429/402/refusal walked to the next plan entry.
	// src/upstream.ts ~line 1202.
	SuffixQuotaFailover = "quota-failover"
	// SuffixContextRetry: a provider context rejection retried after compaction.
	// src/upstream.ts ~line 1645.
	SuffixContextRetry = "context-retry"
)

// ReasonExplicit is "explicit:<routing>".
func ReasonExplicit(routing string) string { return prefixExplicit + routing }

// ReasonBrain is "brain:<routing>"; an unset verdict reads "brain:unset".
// src/routing.ts applyVerdict.
func ReasonBrain(routing string) string {
	if routing == "" {
		routing = "unset"
	}
	return prefixBrain + routing
}

// ReasonBrainFallback is "brain-fallback:<routing>".
func ReasonBrainFallback(routing string) string { return prefixBrainFallback + routing }

// AppendReason joins a suffix onto a reason chain.
func AppendReason(reason, suffix string) string {
	if reason == "" {
		return suffix
	}
	return reason + ":" + suffix
}

// WithQuotaFailover marks a decision re-pointed by quota/refusal failover.
// src/upstream.ts `${decision.reason}:quota-failover`.
func WithQuotaFailover(reason string) string { return AppendReason(reason, SuffixQuotaFailover) }

// WithContextRetry marks a retry after compaction.
// src/upstream.ts `${retry.reason}:context-retry`.
func WithContextRetry(reason string) string { return AppendReason(reason, SuffixContextRetry) }

func withCanonical(reason, canonical string) string {
	return AppendReason(reason, prefixCanonicalSuffix+canonical)
}

func withCacheKeep(reason string, keep CacheKeepReason) string {
	return AppendReason(reason, prefixCacheKeepSuffix+string(keep))
}

// ReasonParts splits a reason chain into its base and suffixes. Routing ids
// and canonical ids never contain ':' (routing ids are slugs), but a canonical
// suffix may carry a model id that does (`canonical:openrouter/x:free`), so
// everything after "canonical:" up to the next known suffix is kept together.
type ReasonParts struct {
	// Base is "pinned-model", "canonical-model", "explicit", "brain",
	// "brain-fallback", or "remote-compaction".
	Base string
	// Routing is the routing id for explicit/brain/brain-fallback bases.
	Routing  string
	Suffixes []string
}

// ParseReason splits a reason string for logs, dashboards, and tests.
func ParseReason(reason string) ReasonParts {
	segments := strings.Split(reason, ":")
	if len(segments) == 0 || reason == "" {
		return ReasonParts{}
	}
	parts := ReasonParts{Base: segments[0]}
	rest := segments[1:]
	switch parts.Base {
	case "explicit", "brain", "brain-fallback":
		if len(rest) > 0 {
			parts.Routing = rest[0]
			rest = rest[1:]
		}
	}
	for i := 0; i < len(rest); i++ {
		if rest[i] == "canonical" && i+1 < len(rest) {
			// Absorb the model id until the next recognised suffix.
			j := i + 1
			id := rest[j]
			for j+1 < len(rest) && !knownSuffix(rest[j+1]) {
				j++
				id += ":" + rest[j]
			}
			parts.Suffixes = append(parts.Suffixes, "canonical:"+id)
			i = j
			continue
		}
		parts.Suffixes = append(parts.Suffixes, rest[i])
	}
	return parts
}

// Has reports whether the parsed chain contains suffix.
func (p ReasonParts) Has(suffix string) bool {
	for _, s := range p.Suffixes {
		if s == suffix {
			return true
		}
	}
	return false
}

func knownSuffix(s string) bool {
	switch s {
	case SuffixQuotaFallback, SuffixNoCandidate, SuffixQuotaSkip, SuffixContextSkip,
		SuffixEffortSkip, SuffixToolSkip, SuffixBrainLowConfidence, SuffixEffortClamped, SuffixCacheHot,
		SuffixCacheStale, SuffixQuotaFailover, SuffixContextRetry, "canonical":
		return true
	}
	return strings.HasPrefix(s, prefixCacheKeepSuffix)
}
