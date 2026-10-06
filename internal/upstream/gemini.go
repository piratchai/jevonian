package upstream

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/oauth"
	"github.com/xinyao27/jevonian/internal/provider/gemini"
	"github.com/xinyao27/jevonian/internal/wire"
	anthropicwire "github.com/xinyao27/jevonian/internal/wire/anthropic"
	openaiwire "github.com/xinyao27/jevonian/internal/wire/openai"
	responseswire "github.com/xinyao27/jevonian/internal/wire/responses"
)

// responseWrapper lets an adapter re-shape a successful upstream reply (Gemini
// answers in its own envelope, folded back to Chat Completions here).
type responseWrapper interface {
	WrapResponse(resp *http.Response, stream bool, model string) (*http.Response, error)
}

// geminiAdapter is the Antigravity (Cloud Code Assist) egress. The body is a
// Gemini envelope built from a Chat Completions fold of the client body; the
// reply is folded back to Chat Completions so every downstream path treats it
// as an ordinary OpenAI-wire answer.
type geminiAdapter struct {
	openaiAdapter
	stream bool
}

func (*geminiAdapter) AlwaysStreams() bool { return false }

func (a *geminiAdapter) Prepare(in PrepInput) (wire.Body, error) {
	a.stream = in.Stream
	var chat wire.Body
	switch in.ClientKind {
	case KindResponses:
		chat = responseswire.ToChatRequest(in.ClientBody, in.Model)
	case KindAnthropic:
		chat = anthropicwire.ToChatRequest(in.ClientBody, in.Model)
	default:
		chat = wire.Body{}
		for k, v := range in.ClientBody {
			chat[k] = v
		}
		msgs := wire.AsSlice(in.ClientBody["messages"])
		normalized := openaiwire.NormalizeMessages(msgs)
		out := make([]any, len(normalized))
		for i, m := range normalized {
			out[i] = m
		}
		chat["messages"] = out
	}
	return wire.RewritePromptBodies(gemini.Envelope(oauth.ResolveAntigravityProject(), in.Model, gemini.ChatToGemini(chat)), in.PromptPolicy), nil
}

func (a *geminiAdapter) EndpointURL(p config.Provider) string {
	return gemini.Endpoint(p.BaseURL, a.stream)
}

// WrapResponse folds the Gemini reply into Chat Completions JSON or SSE.
func (a *geminiAdapter) WrapResponse(resp *http.Response, stream bool, model string) (*http.Response, error) {
	out := *resp
	out.Header = resp.Header.Clone()
	if stream {
		out.Header.Set("Content-Type", "text/event-stream")
		out.Body = wire.TranslateReader(resp.Body, gemini.NewToChatStream(model, nil, nil))
		return &out, nil
	}
	raw, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	var parsed any
	_ = json.Unmarshal(raw, &parsed)
	chat := gemini.ChatCompletion(gemini.Unwrap(parsed), model)
	data, err := json.Marshal(chat)
	if err != nil {
		return nil, err
	}
	out.Header.Set("Content-Type", "application/json")
	out.Header.Del("Content-Length")
	out.ContentLength = int64(len(data))
	out.Body = io.NopCloser(bytes.NewReader(data))
	return &out, nil
}
