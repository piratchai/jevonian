package compaction

import (
	"context"
	"errors"

	"github.com/xinyao27/jevonian/internal/brain"
	"github.com/xinyao27/jevonian/internal/config"
)

// BrainAsker answers compaction questions through the router's brain channels:
// the first channel that answers wins, and the last error is reported when all
// fail. src/upstream.ts compactForOverflow's `asker`.
type BrainAsker struct {
	Client *brain.Client
	Brains []config.BrainConfig
}

// Ask implements Asker.
func (a BrainAsker) Ask(ctx context.Context, state State, questions map[string]Question) (map[string]Answer, error) {
	st := map[string]any{}
	if err := roundTrip(state, &st); err != nil {
		return nil, err
	}
	qs := make(map[string]brain.Question, len(questions))
	for name, q := range questions {
		qs[name] = brain.Question{Type: q.Type, Instructions: q.Instructions}
	}
	last := errors.New("no Jev brain answered")
	for _, b := range a.Brains {
		raw, err := a.Client.AskRaw(ctx, b, st, qs)
		if err != nil {
			last = err
			continue
		}
		out := make(map[string]Answer, len(raw))
		for name, v := range raw {
			rec, _ := v.(map[string]any)
			if n, ok := rec["noul"].(float64); ok {
				out[name] = Answer{Noul: &n}
			} else {
				out[name] = Answer{}
			}
		}
		return out, nil
	}
	return nil, last
}
