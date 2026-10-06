package cursor

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"testing"
)

func jsonUnmarshal(data string, v any) error { return json.Unmarshal([]byte(data), v) }

// Port of src/cursor.test.ts — the pure decode/classify halves. The
// token-locked pieces live in token_test.go; the Run stream lives in
// run_test.go / provider_test.go.

func TestParseAboutReadsEmailAndPlan(t *testing.T) {
	account, ok := ParseAbout(`{"subscriptionTier":"pro","userEmail":"me@work.dev"}`)
	if !ok {
		t.Fatal("expected an account")
	}
	if account.User != "me@work.dev" || account.Plan != "pro" {
		t.Fatalf("got %+v", account)
	}
}

func TestParseAboutSkipsUpdateNotice(t *testing.T) {
	out := "[32mUpdate available[0m\n{\"userEmail\":\"a@b.c\",\"subscriptionTier\":\"free\"}"
	account, ok := ParseAbout(out)
	if !ok || account.User != "a@b.c" || account.Plan != "free" {
		t.Fatalf("got %+v, %v", account, ok)
	}
}

func TestParseAboutNobodyNamed(t *testing.T) {
	if _, ok := ParseAbout(`{"subscriptionTier":"pro"}`); ok {
		t.Fatal("expected no account")
	}
	if _, ok := ParseAbout("not json"); ok {
		t.Fatal("expected no account")
	}
}

func TestParseModelsReadsIDNameLines(t *testing.T) {
	out := "claude-opus-5.5-high - Claude Opus 5.5 1M\n\x1b[1mgrok-4.7-low\x1b[0m - Grok 4.7​ (default)\n"
	models := ParseModels(out)
	if len(models) != 2 {
		t.Fatalf("got %d models: %+v", len(models), models)
	}
	if models[0].ID != "claude-opus-5.5-high" || models[1].ID != "grok-4.7-low" {
		t.Fatalf("ids: %+v", models)
	}
	if models[0].Name != "Claude Opus 5.5 1M" {
		t.Fatalf("name[0]: %q", models[0].Name)
	}
	if models[1].Name != "Grok 4.7" {
		t.Fatalf("name[1]: %q", models[1].Name)
	}
}

func TestContextReadsMillionSuffix(t *testing.T) {
	if got := Context("claude-opus-5.5-high", "Claude Opus 5.5 1M"); got != 1_000_000 {
		t.Fatalf("got %d", got)
	}
	if got := Context("auto", "Auto"); got != 200_000 {
		t.Fatalf("got %d", got)
	}
	if got := Context("gpt-5.5-fast", "GPT-5.5 Fast"); got != 200_000 {
		t.Fatalf("got %d", got)
	}
}

func TestSplitID(t *testing.T) {
	cases := []struct {
		id, family, effort string
	}{
		{"grok-4.7-low-fast", "grok-4.7-fast", "low"},
		{"claude-opus-5-thinking-high", "claude-opus-5-thinking", "high"},
		{"claude-4.6-opus-high-thinking", "claude-4.6-opus-thinking", "high"},
		{"gpt-5.2", "gpt-5.2", ""},
	}
	for _, c := range cases {
		family, effort := SplitID(c.id)
		if family != c.family || effort != c.effort {
			t.Fatalf("SplitID(%q) = (%q, %q), want (%q, %q)", c.id, family, effort, c.family, c.effort)
		}
	}
}

func TestCollapseModelsFamilyToDefault(t *testing.T) {
	collapsed := CollapseModels([]Model{
		{ID: "grok-4.7-low", Name: "Grok 4.7 Low", Context: 200_000},
		{ID: "grok-4.7-medium", Name: "Grok 4.7 Medium", Context: 200_000},
		{ID: "grok-4.7-high", Name: "Grok 4.7 High", Context: 200_000},
	})
	if len(collapsed) != 1 {
		t.Fatalf("got %d models", len(collapsed))
	}
	if collapsed[0].ID != "grok-4.7" {
		t.Fatalf("id: %q", collapsed[0].ID)
	}
	if collapsed[0].Name != "Grok 4.7" {
		t.Fatalf("name: %q", collapsed[0].Name)
	}
}

func TestCollapseModelsSingleton(t *testing.T) {
	collapsed := CollapseModels([]Model{{ID: "auto", Name: "Auto", Context: 200_000}})
	if len(collapsed) != 1 || collapsed[0].ID != "auto" || collapsed[0].Name != "Auto" || collapsed[0].Context != 200_000 {
		t.Fatalf("got %+v", collapsed)
	}
}

func TestClassifyErrorReadsConnectDetail(t *testing.T) {
	body := `{"code":"resource_exhausted","message":"You have hit your usage limit","details":[{"debug":{"details":{"title":"Usage limit","detail":"try again in 2h"}}}]}`
	err := ClassifyError(429, body)
	if err.Kind != KindQuota {
		t.Fatalf("kind: %s", err.Kind)
	}
	if err.Status != 429 {
		t.Fatalf("status: %d", err.Status)
	}
	if err.Message != "Usage limit: try again in 2h" {
		t.Fatalf("message: %q", err.Message)
	}
}

func TestClassifyErrorRegionAndAuth(t *testing.T) {
	if got := ClassifyError(403, `{"message":"This region is not yet available for your team"}`); got.Kind != KindRegion {
		t.Fatalf("kind: %s", got.Kind)
	}
	if got := ClassifyError(401, `{"code":"unauthenticated","message":"token expired"}`); got.Kind != KindAuth {
		t.Fatalf("kind: %s", got.Kind)
	}
}

func TestClassifyErrorUnparseableBody(t *testing.T) {
	err := ClassifyError(503, "upstream is unavailable")
	if err.Kind != KindCapacity {
		t.Fatalf("kind: %s", err.Kind)
	}
	if err.Status != 503 {
		t.Fatalf("status: %d", err.Status)
	}
}

func TestEncodeFrame(t *testing.T) {
	frame := EncodeFrame([]byte{1, 2, 3}, 0)
	want := []byte{0, 0, 0, 0, 3, 1, 2, 3}
	if !bytes.Equal(frame, want) {
		t.Fatalf("got %v, want %v", frame, want)
	}
}

// Extra coverage: the frame splitter (used by every Run test) and the protobuf
// value round-trip.

func TestFrameSplitterEndFlag(t *testing.T) {
	payload := []byte("hello")
	chunk := EncodeFrame(payload, 0x02)
	var splitter frameSplitter
	frames, err := splitter.push(chunk)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 || !frames[0].end || !bytes.Equal(frames[0].payload, payload) {
		t.Fatalf("got %+v", frames)
	}
}

func TestFrameSplitterPartial(t *testing.T) {
	payload := make([]byte, 10)
	for i := range payload {
		payload[i] = byte(i)
	}
	chunk := EncodeFrame(payload, 0)
	var splitter frameSplitter
	frames, err := splitter.push(chunk[:7])
	if err != nil || len(frames) != 0 {
		t.Fatalf("partial push: %v %v", frames, err)
	}
	frames, err = splitter.push(chunk[7:])
	if err != nil || len(frames) != 1 {
		t.Fatalf("completion: %v %v", frames, err)
	}
	if !bytes.Equal(frames[0].payload, payload) {
		t.Fatalf("payload mismatch")
	}
	if binary.BigEndian.Uint32(chunk[1:5]) != 10 {
		t.Fatal("length header")
	}
}

func TestPbValueRoundTrip(t *testing.T) {
	var value any
	if err := jsonUnmarshal(`{"a":1,"b":"x","c":[true,null,2.5]}`, &value); err != nil {
		t.Fatal(err)
	}
	back := pbAny(pbValue(value))
	m, ok := back.(map[string]any)
	if !ok {
		t.Fatalf("got %T", back)
	}
	if m["a"] != 1.0 || m["b"] != "x" {
		t.Fatalf("got %v", m)
	}
	list, ok := m["c"].([]any)
	if !ok || len(list) != 3 || list[0] != true || list[1] != nil || list[2] != 2.5 {
		t.Fatalf("list: %v", m["c"])
	}
}
