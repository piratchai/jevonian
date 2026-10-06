package softstream_test

import (
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/softstream"
)

// Differential fixtures from src/soft-error.ts: softErrorMessage,
// redactSecrets and softCompletionChunks (every wire, every placement option).
type softCase struct {
	Fn      string          `json:"fn"`
	Reason  string          `json:"reason"`
	Text    string          `json:"text"`
	Secrets []string        `json:"secrets"`
	Kind    string          `json:"kind"`
	Opts    json.RawMessage `json:"opts"`
	Want    json.RawMessage `json:"want"`
}

var (
	softID      = regexp.MustCompile(`(chatcmpl-soft-|msg_soft_|resp_soft_)[0-9a-f]{16,24}`)
	softCreated = regexp.MustCompile(`"(created|created_at)":\d+`)
)

func softMask(s string) string {
	s = softID.ReplaceAllString(s, "${1}ID")
	return softCreated.ReplaceAllString(s, `"$1":0`)
}

func parseFrames(chunks []string) []any {
	var out []any
	for _, chunk := range chunks {
		frame := map[string]any{}
		for _, line := range strings.Split(strings.TrimSpace(softMask(chunk)), "\n") {
			switch {
			case strings.HasPrefix(line, "event:"):
				frame["event"] = strings.TrimSpace(line[6:])
			case strings.HasPrefix(line, "data:"):
				var v any
				data := strings.TrimSpace(line[5:])
				if json.Unmarshal([]byte(data), &v) == nil {
					frame["data"] = v
				} else {
					frame["data"] = data
				}
			}
		}
		out = append(out, frame)
	}
	return out
}

func TestSoftErrorMatchesTypeScript(t *testing.T) {
	raw, err := os.ReadFile("testdata/ts_soft_fuzz.json")
	if err != nil {
		t.Skip("no TS soft-error fixture")
	}
	var cases []softCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for i, c := range cases {
		switch c.Fn {
		case "softErrorMessage":
			var want string
			_ = json.Unmarshal(c.Want, &want)
			if got := softstream.SoftErrorMessage(c.Reason); got != want {
				t.Errorf("case %d softErrorMessage(%q)\n go: %q\n ts: %q", i, c.Reason, got, want)
			}
		case "redactSecrets":
			var want string
			_ = json.Unmarshal(c.Want, &want)
			if got := softstream.RedactSecrets(c.Text, c.Secrets...); got != want {
				t.Errorf("case %d redactSecrets\n go: %q\n ts: %q", i, got, want)
			}
		case "softCompletionChunks":
			var in struct {
				Started    bool `json:"started"`
				Index      int  `json:"index"`
				CloseIndex *int `json:"closeIndex"`
				CloseItem  *struct {
					OutputIndex int            `json:"outputIndex"`
					Item        map[string]any `json:"item"`
					Text        string         `json:"text"`
				} `json:"closeItem"`
			}
			_ = json.Unmarshal(c.Opts, &in)
			opts := softstream.SoftCompletionOptions{Started: in.Started, Index: in.Index, CloseIndex: -1}
			if in.CloseIndex != nil {
				opts.CloseIndex = *in.CloseIndex
			}
			if in.CloseItem != nil {
				opts.CloseItem = &softstream.OpenItem{OutputIndex: in.CloseItem.OutputIndex, Item: in.CloseItem.Item, Text: in.CloseItem.Text}
			}
			var kind config.UpstreamWire
			switch c.Kind {
			case "anthropic":
				kind = config.WireAnthropic
			case "responses":
				kind = config.WireResponses
			default:
				kind = config.WireOpenAI
			}
			message := "Jevonian hit an internal error: boom. The turn was stopped safely — please retry."
			var gotChunks []string
			for _, chunk := range softstream.SoftCompletionChunks(kind, "m", message, opts) {
				gotChunks = append(gotChunks, string(chunk))
			}
			var wantChunks []string
			_ = json.Unmarshal(c.Want, &wantChunks)
			if !reflect.DeepEqual(parseFrames(gotChunks), parseFrames(wantChunks)) {
				gj, _ := json.Marshal(parseFrames(gotChunks))
				wj, _ := json.Marshal(parseFrames(wantChunks))
				t.Errorf("case %d softCompletionChunks kind=%s opts=%s\n go: %s\n ts: %s", i, c.Kind, c.Opts, gj, wj)
			}
		}
	}
}
