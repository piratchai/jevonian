package wire

import (
	"strings"
	"testing"
)

func TestBareModelID(t *testing.T) {
	for in, want := range map[string]string{
		"deepseek-v4.1-flash":               "deepseek-v4.1-flash",
		"openai/gpt-6-astra":                "gpt-6-astra",
		"accounts/fireworks/models/kimi-k3": "kimi-k3",
		"vendor/":                           "vendor/",
	} {
		if got := BareModelID(in); got != want {
			t.Errorf("BareModelID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestModelVendor(t *testing.T) {
	for in, want := range map[string]string{
		"deepseek/deepseek-flash":           "deepseek",
		"accounts/fireworks/models/kimi-k3": "accounts",
		"gpt-6":                             "",
		"/leading":                          "",
	} {
		if got := ModelVendor(in); got != want {
			t.Errorf("ModelVendor(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSoftErrorMessage(t *testing.T) {
	got := SoftErrorMessage("  rate\n  limited  ")
	if !strings.HasPrefix(got, SoftErrorPrefix+": rate limited") {
		t.Fatalf("got %q", got)
	}
	if SoftErrorMessage("") != SoftErrorPrefix+". The turn was stopped safely — please retry." {
		t.Fatalf("empty = %q", SoftErrorMessage(""))
	}
}

func TestSSEFrames(t *testing.T) {
	if got := string(SSEData(Body{"a": float64(1)})); got != "data: {\"a\":1}\n\n" {
		t.Fatalf("data = %q", got)
	}
	if got := string(SSEEvent(Body{"type": "message_stop"})); got != "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n" {
		t.Fatalf("event = %q", got)
	}
	if got := string(SSEEvent(Body{"x": float64(1)})); !strings.HasPrefix(got, "event: message\n") {
		t.Fatalf("fallback event = %q", got)
	}
}

func TestSplitSseFrames(t *testing.T) {
	datas, rest := SplitSseFrames("data: {\"a\":1}\n\ndata: [DONE]\n\ndata: partial")
	if len(datas) != 2 || datas[1] != "[DONE]" {
		t.Fatalf("datas = %v", datas)
	}
	if rest != "data: partial" {
		t.Fatalf("rest = %q", rest)
	}
}

type echo struct{ finished bool }

func (e *echo) Handle(event Body, sink EventSink) { sink.EmitData(event) }
func (e *echo) Finish(sink EventSink) {
	e.finished = true
	sink.Emit(SSEDone)
}

func TestTranslatorStreamChunkedInput(t *testing.T) {
	// Bytes split across writes mid-frame must still parse.
	tr := &echo{}
	stream := NewTranslatorStream(tr)
	done := make(chan string)
	go func() {
		var out strings.Builder
		buf := make([]byte, 7) // smaller than a frame to exercise Reader buffering
		r := stream.Reader()
		for {
			n, err := r.Read(buf)
			out.Write(buf[:n])
			if err != nil {
				break
			}
		}
		done <- out.String()
	}()
	_, _ = stream.Write([]byte("data: {\"a\""))
	_, _ = stream.Write([]byte(":1}\n"))
	_, _ = stream.Write([]byte("\ndata: {\"b\":2}")) // no trailing blank line
	_ = stream.Close()
	out := <-done
	if !strings.Contains(out, `data: {"a":1}`) || !strings.Contains(out, `data: {"b":2}`) {
		t.Fatalf("out = %q", out)
	}
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Fatalf("missing done: %q", out)
	}
	if !tr.finished {
		t.Fatal("Finish not called")
	}
	if _, err := stream.Write([]byte("x")); err != ErrStreamClosed {
		t.Fatalf("write after close err = %v", err)
	}
}

func TestStreamTracker(t *testing.T) {
	var tracker StreamTracker
	status, _, errText := tracker.CancelOutcome()
	if status != 499 || errText != "client canceled" {
		t.Fatalf("empty = %d %q", status, errText)
	}
	tracker.Feed(StreamEvent{Kind: StreamUsage, Usage: Usage{Input: 3, Output: 2}})
	tracker.Feed(StreamEvent{Kind: StreamContent})
	status, usage, _ := tracker.CancelOutcome()
	if status != 200 || usage.Input != 3 {
		t.Fatalf("delivered = %d %+v", status, usage)
	}
	tracker.Feed(StreamEvent{Kind: StreamError, Message: "boom"})
	if tracker.Failure() != "boom" || tracker.Finished() {
		t.Fatalf("tracker failure=%q finished=%v", tracker.Failure(), tracker.Finished())
	}
	tracker.Feed(StreamEvent{Kind: StreamFinish})
	if !tracker.Finished() {
		t.Fatal("not finished")
	}
}

func TestTextOf(t *testing.T) {
	if TextOf("x") != "x" {
		t.Fatal("string")
	}
	if got := TextOf([]any{Body{"text": "a"}, Body{"type": "image"}, Body{"text": "b"}}); got != "a\nb" {
		t.Fatalf("array = %q", got)
	}
	if TextOf(nil) != "" {
		t.Fatal("nil")
	}
	if got := TextOf(Body{"k": "v"}); got != `{"k":"v"}` {
		t.Fatalf("object = %q", got)
	}
}
