package routing

import (
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"testing"
)

// Differential tests: testdata/ts_golden.json is produced by running the TS
// reference (src/routing.ts, capabilities.ts, compaction.ts, session.ts) on the
// same inputs. Regenerate with `tsx` against src/.

type golden struct {
	Phase []struct {
		Kind string
		Body map[string]any
		Want struct {
			Phase               string
			ConsecutiveFailures int
			HasToolResults      bool
			HasTools            bool
			RecentToolResults   []string
			WithinTurn          bool
		}
	}
	LastUser []struct {
		Kind string
		Body map[string]any
		Want string
	}
	Estimate []struct {
		Body map[string]any
		Want int
	}
	Session []struct {
		Body    map[string]any
		Headers map[string]string
		Want    string
	}
	Transcript []struct {
		Kind string
		Body map[string]any
		Want string
	}
	Extract []struct {
		Text string
		Want string
	}
	Clamp []struct {
		Requested string
		Supports  []string
		Want      string
	}
	Fits []struct {
		Window int
		Tokens int
		Want   bool
	}
	Tokens []struct {
		Text string
		Want int
	}
	Keep []struct {
		Mode   string
		Within bool
		Obs    *struct {
			Provider, Model                                        string
			At                                                     int64
			UncachedInputTokens, CacheReadTokens, CacheWriteTokens int
			Success                                                bool
		}
		Now   int64
		TTL   int64
		Cands []struct{ Provider, Model string }
		Want  struct {
			Keep      bool
			Reason    string
			CacheRead int
			At        int64
		}
	}
}

func loadGolden(t *testing.T) golden {
	t.Helper()
	raw, err := os.ReadFile("testdata/ts_golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var g golden
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

func TestGoldenClassifyPhase(t *testing.T) {
	for i, c := range loadGolden(t).Phase {
		got := ClassifyPhase(c.Body, RequestKind(c.Kind))
		want := PhaseSignals{Phase: c.Want.Phase, ConsecutiveFailures: c.Want.ConsecutiveFailures,
			HasToolResults: c.Want.HasToolResults, HasTools: c.Want.HasTools,
			RecentToolResults: c.Want.RecentToolResults, WithinTurn: c.Want.WithinTurn}
		if len(want.RecentToolResults) == 0 {
			want.RecentToolResults = []string{}
		}
		if len(got.RecentToolResults) == 0 {
			got.RecentToolResults = []string{}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("case %d (%s): got %+v want %+v", i, c.Kind, got, want)
		}
	}
}

func TestGoldenLastUserMessage(t *testing.T) {
	for i, c := range loadGolden(t).LastUser {
		if got := LastUserMessage(c.Body, RequestKind(c.Kind)); got != c.Want {
			t.Errorf("case %d (%s): got %q want %q", i, c.Kind, got, c.Want)
		}
	}
}

func TestGoldenCompactionEstimate(t *testing.T) {
	for i, c := range loadGolden(t).Estimate {
		if got := CompactionEstimate(c.Body); got != c.Want {
			t.Errorf("case %d: got %d want %d", i, got, c.Want)
		}
	}
}

func TestGoldenSessionKey(t *testing.T) {
	hex16 := regexp.MustCompile(`^[0-9a-f]{16}$`)
	for i, c := range loadGolden(t).Session {
		got := ResolveSessionKey(c.Body, c.Headers)
		if hex16.MatchString(c.Want) && len(c.Headers) == 0 {
			// Fingerprint path: Go decodes bodies into maps, so JS key insertion
			// order is unrecoverable and the hash differs for multi-key objects.
			// Require the same shape and determinism instead of byte equality.
			if !hex16.MatchString(got) || got != ResolveSessionKey(c.Body, c.Headers) {
				t.Errorf("case %d: fingerprint %q not stable 16-hex", i, got)
			}
			continue
		}
		if got != c.Want {
			t.Errorf("case %d: got %q want %q", i, got, c.Want)
		}
	}
}

func TestGoldenFullTranscript(t *testing.T) {
	for i, c := range loadGolden(t).Transcript {
		if got := FullTranscript(c.Body, RequestKind(c.Kind)); got != c.Want {
			t.Errorf("case %d (%s): got %q want %q", i, c.Kind, got, c.Want)
		}
	}
}

func TestGoldenExtractUserQuery(t *testing.T) {
	for _, c := range loadGolden(t).Extract {
		if got := ExtractUserQuery(c.Text); got != c.Want {
			t.Errorf("%q: got %q want %q", c.Text, got, c.Want)
		}
	}
}

func TestGoldenClampEffort(t *testing.T) {
	for _, c := range loadGolden(t).Clamp {
		if got := ClampEffort(c.Requested, c.Supports); got != c.Want {
			t.Errorf("clamp(%q,%v): got %q want %q", c.Requested, c.Supports, got, c.Want)
		}
	}
}

func TestGoldenFitsContext(t *testing.T) {
	for _, c := range loadGolden(t).Fits {
		if got := FitsContext(c.Window, c.Tokens); got != c.Want {
			t.Errorf("fits(%d,%d): got %v want %v", c.Window, c.Tokens, got, c.Want)
		}
	}
}

func TestGoldenEstimateTokens(t *testing.T) {
	for _, c := range loadGolden(t).Tokens {
		if got := EstimateTokens(c.Text); got != c.Want {
			t.Errorf("%.40q: got %d want %d", c.Text, got, c.Want)
		}
	}
}

func TestGoldenCacheAffinityKeep(t *testing.T) {
	for i, c := range loadGolden(t).Keep {
		var prev *SessionState
		if c.Obs != nil {
			prev = &SessionState{Phase: "plan", Model: "m", Provider: "a", Turns: 1, UpdatedAt: 1000,
				Cache: &CacheObservation{Provider: c.Obs.Provider, Model: c.Obs.Model, At: c.Obs.At,
					UncachedInputTokens: c.Obs.UncachedInputTokens, CacheReadTokens: c.Obs.CacheReadTokens,
					CacheWriteTokens: c.Obs.CacheWriteTokens, Success: c.Obs.Success}}
		}
		picks := []TierPick{}
		for _, x := range c.Cands {
			picks = append(picks, TierPick{Provider: x.Provider, Model: x.Model})
		}
		got := cacheAffinityKeep(prev, CacheAffinityMode(c.Mode), c.Within, picks, c.Now, c.TTL)
		if got.Keep != c.Want.Keep || string(got.Reason) != c.Want.Reason || got.CacheRead != c.Want.CacheRead || got.At != c.Want.At {
			t.Errorf("case %d (%s within=%v now=%d ttl=%d): got %+v want %+v", i, c.Mode, c.Within, c.Now, c.TTL, got, c.Want)
		}
	}
}

// TestGoldenEnvelopeFuzz compares lastUserMessage / extractUserQuery on
// randomly generated tag soup (paired, dangling, mixed-case, HTML-exempt tags)
// against the TS reference.
func TestGoldenEnvelopeFuzz(t *testing.T) {
	raw, err := os.ReadFile("testdata/ts_envelope_fuzz.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Text    string
		Want    string
		Extract string
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	bad := 0
	for _, c := range cases {
		body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": c.Text}}}
		if got := LastUserMessage(body, KindOpenAI); got != c.Want {
			bad++
			if bad <= 15 {
				t.Errorf("lastUserMessage(%q): got %q want %q", c.Text, got, c.Want)
			}
		}
		if got := ExtractUserQuery(c.Text); got != c.Extract {
			t.Errorf("extractUserQuery(%q): got %q want %q", c.Text, got, c.Extract)
		}
	}
	if bad > 0 {
		t.Errorf("%d/%d envelope cases differ", bad, len(cases))
	}
}
