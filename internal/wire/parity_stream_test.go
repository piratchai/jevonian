package wire_test

import (
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/xinyao27/jevonian/internal/wire"
	"github.com/xinyao27/jevonian/internal/wire/anthropic"
	"github.com/xinyao27/jevonian/internal/wire/responses"
)

// Differential fixtures from the TypeScript stream bridges: seeded random SSE
// (Anthropic, Chat, Responses) is fed through the TS TransformStream with a
// fixed chunk plan; Go must emit the same frames once random ids and
// timestamps are masked.
type streamCase struct {
	Fn    string `json:"fn"`
	Input string `json:"input"`
	Plan  []int  `json:"plan"`
	Want  string `json:"want"`
}

var (
	maskID      = regexp.MustCompile(`(chatcmpl-|msg_|resp_)[0-9a-f]{24}`)
	maskCreated = regexp.MustCompile(`"(created|created_at)":\d+`)
	maskCallID  = regexp.MustCompile(`call_[0-9a-f]{16}`)
	maskToolu   = regexp.MustCompile(`toolu_[0-9a-f]{8,24}`)
)

func maskStream(s string) string {
	s = maskID.ReplaceAllString(s, "${1}ID")
	s = maskCreated.ReplaceAllString(s, `"$1":0`)
	s = maskToolu.ReplaceAllString(s, "toolu_RANDOM")
	return maskCallID.ReplaceAllString(s, "call_RANDOM")
}

// frames splits an SSE body into (event name, parsed JSON payload) pairs so
// the comparison ignores JSON key order, which Go sorts and TS does not.
func frames(t *testing.T, s string) []any {
	t.Helper()
	var out []any
	for _, block := range strings.Split(s, "\n\n") {
		if strings.TrimSpace(block) == "" {
			continue
		}
		frame := map[string]any{}
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event:"):
				frame["event"] = strings.TrimSpace(line[6:])
			case strings.HasPrefix(line, "data:"):
				data := strings.TrimSpace(line[5:])
				var parsed any
				if json.Unmarshal([]byte(data), &parsed) == nil {
					frame["data"] = parsed
				} else {
					frame["data"] = data
				}
			default:
				frame["raw"] = line
			}
		}
		out = append(out, frame)
	}
	return out
}

func feed(t wire.Translator, input string, plan []int) string {
	stream := wire.NewTranslatorStream(t)
	var out strings.Builder
	done := make(chan struct{})
	go func() {
		buf := make([]byte, 1<<16)
		r := stream.Reader()
		for {
			n, err := r.Read(buf)
			out.Write(buf[:n])
			if err != nil {
				break
			}
		}
		close(done)
	}()
	data := []byte(input)
	pos := 0
	for _, size := range plan {
		if pos >= len(data) {
			break
		}
		end := min(pos+size, len(data))
		_, _ = stream.Write(data[pos:end])
		pos = end
	}
	if pos < len(data) {
		_, _ = stream.Write(data[pos:])
	}
	_ = stream.Close()
	<-done
	return out.String()
}

func TestStreamBridgesMatchTypeScriptFuzz(t *testing.T) {
	raw, err := os.ReadFile("testdata/ts_stream_fuzz.json")
	if err != nil {
		t.Skip("no TS stream fixture")
	}
	var cases []streamCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	failures := map[string]int{}
	for i, c := range cases {
		var translator wire.Translator
		switch c.Fn {
		case "anthropicToChatStream":
			translator = anthropic.NewToChatStream("m", nil, nil)
		case "chatToAnthropicStream":
			translator = anthropic.NewChatToStream("m", anthropic.ChatToStreamOptions{})
		case "responsesToChatStream":
			translator = responses.NewToChatStream("m", nil)
		case "chatToResponsesStream":
			translator = responses.NewFromChatStream("m", nil)
		default:
			t.Fatalf("unknown fn %s", c.Fn)
		}
		got := maskStream(feed(translator, c.Input, c.Plan))
		want := maskStream(c.Want)
		if !reflect.DeepEqual(frames(t, got), frames(t, want)) {
			failures[c.Fn]++
			if failures[c.Fn] <= 2 {
				t.Errorf("case %d %s mismatch plan=%v\ninput:\n%s\n--- go:\n%s\n--- ts:\n%s", i, c.Fn, c.Plan, c.Input, got, want)
			}
		}
	}
	for fn, n := range failures {
		t.Errorf("%s: %d mismatches", fn, n)
	}
}
