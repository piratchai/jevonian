package server

import (
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/xinyao27/jevonian/internal/ledger"
	"github.com/xinyao27/jevonian/internal/routing"
)

// BrainRecorder returns the routing.Deps.RecordBrainCall hook: one brain
// channel call becomes a ledger row (kind "brain") plus a captured body, the
// two joined by the same id. Port of src/routing.ts recordBrainCall.
//
// The id is a fresh UUID, not "<request>:brain:<channel>": the log detail view
// loads a brain call's body by the row's own id, and body ids must match
// ^[0-9a-fA-F-]{8,64}$ (src/bodies.ts isSafeBodyId).
//
// db and prices may be nil: the body is still captured, and a call without a
// rate card is recorded as unpriced.
func BrainRecorder(db *ledger.DB, prices routing.PriceSource) func(routing.BrainCallRecord) {
	return func(rec routing.BrainCallRecord) {
		usage := routing.Usage{}
		if rec.Verdict != nil && rec.Verdict.Usage != nil {
			usage = *rec.Verdict.Usage
		}
		// The verdict carries the routed model; the brain's own model id comes
		// from the response.
		model := "jev"
		if rec.Brain.Model != "" {
			model = rec.Brain.Model
		}
		if rec.Verdict != nil && rec.Verdict.ModelName != "" {
			model = rec.Verdict.ModelName
		}
		var cost *float64
		known := false
		if prices != nil {
			if usd, ok := routing.CostOf(prices(model, ""), usage, time.Now()); ok {
				cost, known = &usd, true
			}
		}

		id := uuid.NewString()
		body := map[string]any{
			"kind":    "brain",
			"at":      time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
			"channel": rec.Brain.Channel,
			"model":   model,
		}
		if rec.State != nil {
			body["state"] = rec.State
		}
		if v := rec.Verdict; v != nil {
			verdict := map[string]any{"model": v.Model, "confidence": v.Confidence}
			if len(v.Probabilities) > 0 {
				verdict["probabilities"] = v.Probabilities
			}
			if v.Effort != "" {
				verdict["effort"] = v.Effort
			}
			if len(v.EffortProbabilities) > 0 {
				verdict["effortProbabilities"] = v.EffortProbabilities
			}
			body["verdict"] = verdict
		}
		SaveBody(id, body)

		if db == nil {
			return
		}
		row := ledger.Record{
			ID:               id,
			RequestID:        rec.RequestID,
			KeyID:            rec.KeyID,
			KeyName:          rec.KeyName,
			TS:               time.Now(),
			Session:          rec.Session,
			Path:             "/brain",
			Provider:         "brain:" + rec.Brain.Channel,
			Model:            model,
			Status:           200,
			LatencyMs:        int(time.Now().UnixMilli() - rec.Started),
			PromptTokens:     usage.Input,
			CompletionTokens: usage.Output,
			CacheReadTokens:  usage.CacheRead,
			CacheWriteTokens: usage.CacheWrite,
			CostUSD:          cost,
			PricingKnown:     known,
			Kind:             "brain",
			BrainChannel:     rec.Brain.Channel,
			Reason:           "routing-brain",
		}
		if rec.Verdict == nil {
			row.Status = 502
			row.Error = "brain unavailable"
			if rec.Failure != nil && strings.TrimSpace(rec.Failure.Error) != "" {
				row.Error = rec.Failure.Error
			}
		}
		_ = db.Append(row)
	}
}
