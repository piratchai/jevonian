package server

import (
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/softstream"
)

// ClientKindOfPath maps a request path to the client wire that must answer, the
// way clientKindOfPath does in src/upstream.ts. It determines the SSE shape a
// soft close uses.
func ClientKindOfPath(path string) config.UpstreamWire {
	switch {
	case strings.HasSuffix(path, "/messages") || strings.HasSuffix(path, "/messages/count_tokens"):
		return config.WireAnthropic
	case strings.HasSuffix(path, "/responses"):
		return config.WireResponses
	default:
		return config.WireOpenAI
	}
}

// StreamOptions configures WriteStream — the Go port of streamResponse() in
// src/upstream.ts.
type StreamOptions struct {
	// Kind is the client wire a soft completion must speak. Required for SSE
	// bodies; irrelevant for non-SSE passthroughs.
	Kind config.UpstreamWire
	// Model is echoed inside soft completion chunks.
	Model string
	// ContentType of the upstream response. Only a real SSE body is wrapped — a
	// passthrough of some other content type is forwarded untouched. Defaults to
	// text/event-stream when empty.
	ContentType string
	// Headers are extra response headers (x-jevonian-*) merged over the defaults.
	Headers map[string]string
	// KeepaliveInterval overrides the 15s idle keepalive (tests).
	KeepaliveInterval time.Duration
	// OnClientCancel fires when the client hangs up before the stream finishes;
	// the seam uses it to record a 499 ledger row.
	OnClientCancel func()
	// OnFirstChunk fires once on the first non-keepalive byte — the first-token
	// measurement point.
	OnFirstChunk func()
	// OnSoft fires when a mid-stream failure was closed softly; reason is the
	// upstream failure text (redacted) or "" for a silent early end. The seam
	// records the real 502.
	OnSoft func(reason string)
	// Redact scrubs provider credentials from upstream error text before it is
	// shown to the client or recorded.
	Redact func(string) string
}

// WriteStream answers w with an SSE response that keeps the socket warm during
// silent thinking and turns a mid-stream failure into a normal assistant turn.
//
// This is the single place a mid-stream failure becomes a soft completion: the
// harness rolls the whole message back when a stream dies abruptly, so a dropped
// upstream socket is closed with a soft assistant message instead (see
// softstream). A nil body still answers the turn rather than handing the client
// an empty 200 it would read as a broken response.
//
// The caller must have decided headers/status first; WriteStream always writes
// status 200 with the SSE content type when the body is SSE-shaped.
func WriteStream(w http.ResponseWriter, r *http.Request, body io.ReadCloser, opts StreamOptions) {
	contentType := opts.ContentType
	if contentType == "" {
		contentType = "text/event-stream"
	}
	isSSE := strings.Contains(contentType, "event-stream")

	out := body
	if isSSE {
		message := softstream.SoftErrorMessage("the upstream stream ended unexpectedly")
		out = softstream.Wrap(r.Context(), body, opts.Kind, opts.Model, softstream.SoftOptions{
			Message: message,
			OnSoft:  opts.OnSoft,
			Redact:  opts.Redact,
		})
		out = softstream.Keepalive(r.Context(), out, softstream.KeepaliveOptions{
			Interval:       opts.KeepaliveInterval,
			OnClientCancel: opts.OnClientCancel,
			OnFirstChunk:   opts.OnFirstChunk,
		})
	}

	header := w.Header()
	header.Set("content-type", contentType)
	header.Set("cache-control", "no-store")
	for k, v := range opts.Headers {
		header.Set(k, v)
	}
	w.WriteHeader(http.StatusOK)

	if out == nil {
		return
	}
	defer out.Close()

	// io.Copy honors http.Flusher through the writer only on explicit flushes;
	// flush after each upstream write so SSE events reach the client promptly.
	if flusher, ok := w.(http.Flusher); ok {
		copyFlushing(w, flusher, out)
		return
	}
	_, _ = io.Copy(w, out)
}

// copyFlushing streams r to w, flushing after every read so each SSE event is
// pushed to the client as soon as it arrives.
func copyFlushing(w http.ResponseWriter, f http.Flusher, r io.Reader) {
	buf := make([]byte, 16<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			f.Flush()
		}
		if err != nil {
			return
		}
	}
}

// WriteSoftError answers a failed streaming turn with a completed assistant
// message instead of a JSON 5xx — the Go port of errorResponse()'s streaming
// branch in src/upstream.ts.
//
// A streaming client that gets a JSON 5xx mid-agent loop can lose the whole
// turn: the harness treats a hard error as "the response is invalid" and rolls
// the message back. Answering as a completed assistant message keeps the
// conversation alive so the user can retry; the seam still records the real
// failure status in the ledger.
func WriteSoftError(w http.ResponseWriter, kind config.UpstreamWire, model, reason string, headers map[string]string) {
	text := softstream.SoftErrorMessage(reason)
	body := softstream.SoftCompletionBytes(kind, model, text)

	header := w.Header()
	header.Set("content-type", "text/event-stream")
	header.Set("cache-control", "no-store")
	header.Set(softstream.SoftErrorHeader, "1")
	for k, v := range headers {
		header.Set(k, v)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
