package devin

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/wire"
)

// Provider is the server-facing seam for one Devin subscription provider.
// Every field is optional: HTTP defaults to http.DefaultClient, BaseURL to the
// provider config's baseUrl (then DefaultBaseURL), and Token to reading the
// Devin CLI's credentials.toml for Login.
type Provider struct {
	HTTP    *http.Client
	BaseURL string
	Login   *config.ProviderLogin
	// Token overrides credential resolution (the server's OAuth cache plugs in here).
	Token func(ctx context.Context) (string, error)
}

// NewProvider builds a Provider from a config entry.
func NewProvider(p config.Provider, client *http.Client) *Provider {
	return &Provider{HTTP: client, BaseURL: p.BaseURL, Login: p.Login}
}

func (p *Provider) client() *http.Client {
	if p.HTTP != nil {
		return p.HTTP
	}
	return http.DefaultClient
}

func (p *Provider) baseURL() string {
	if strings.TrimSpace(p.BaseURL) != "" {
		return p.BaseURL
	}
	return DefaultBaseURL
}

// ResolveToken returns the token Chat/Models/Quota would use.
func (p *Provider) ResolveToken(ctx context.Context) (string, error) {
	if p.Token != nil {
		return p.Token(ctx)
	}
	return ReadToken(p.Login)
}

// ChatRequest is one Chat Completions turn bound for Devin.
type ChatRequest struct {
	// Body is an OpenAI Chat Completions object (messages, tools, max_tokens, …).
	Body  wire.Body
	Model string
	// Stream selects ChatResult.Stream (OpenAI SSE) over ChatResult.Completion.
	Stream  bool
	Options ChatOptions
	// Stream callbacks; see StreamOptions.
	OnFinish func(Finish)
	OnEvent  func(wire.StreamEvent)
}

// ChatResult is either a classified refusal (Error, nothing sent to the
// client yet — safe to fail over) or an answer: Stream for stream requests,
// Completion + Finish for the rest. A non-stream Finish.Error is a refusal
// that arrived after content began and should also be failed over.
type ChatResult struct {
	Error      *StreamError
	Stream     io.ReadCloser
	Completion wire.Body
	Finish     Finish
	// PolicyRetried is set when a content_policy refusal was retried once without
	// the client's system prompt (counts as a retry in the ledger).
	PolicyRetried bool
	// Status is the upstream HTTP status of the last attempt (0 on transport error).
	Status int
}

// Chat runs one Devin turn: POST the Connect request, classify a non-2xx or
// leading error trailer, retry once without the client system prompt on a
// content_policy refusal, and adapt the stream for the caller. A returned error
// is a transport failure (eligible for transport failover); refusals come back
// as ChatResult.Error.
func (p *Provider) Chat(ctx context.Context, req ChatRequest) (ChatResult, error) {
	token, err := p.ResolveToken(ctx)
	if err != nil {
		return ChatResult{}, err
	}
	stream, result, err := p.open(ctx, token, req.Body, req)
	if err != nil {
		return result, err
	}
	if result.Error != nil && result.Error.Kind == KindContentPolicy {
		// The wire neutralizes the signatures we know, but that list trails the
		// client: one retry without the client's system prompt clears the rest.
		body := make(wire.Body, len(req.Body))
		for k, v := range req.Body {
			body[k] = v
		}
		body["messages"] = StripAgentSystemMessages(wire.AsSlice(req.Body["messages"]))
		stream, result, err = p.open(ctx, token, body, req)
		result.PolicyRetried = true
		if err != nil {
			return result, err
		}
	}
	if result.Error != nil {
		return result, nil
	}
	if req.Stream {
		result.Stream = ToChatStream(req.Model, stream, StreamOptions{Token: token, OnFinish: req.OnFinish, OnEvent: req.OnEvent})
		return result, nil
	}
	result.Completion, result.Finish = ChatCompletion(stream, req.Model, token)
	return result, nil
}

func (p *Provider) open(ctx context.Context, token string, body wire.Body, req ChatRequest) (io.ReadCloser, ChatResult, error) {
	payload := BuildChatRequest(token, body, req.Model, req.Options)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, ChatURL(p.baseURL()), bytes.NewReader(payload))
	if err != nil {
		return nil, ChatResult{}, err
	}
	httpReq.Header = Headers(token, HeadersStream)
	resp, err := p.client().Do(httpReq)
	if err != nil {
		return nil, ChatResult{}, err
	}
	result := ChatResult{Status: resp.StatusCode}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		text, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		result.Error = ClassifyError(resp.StatusCode, string(text), token)
		return nil, result, nil
	}
	if resp.Body == nil || resp.Body == http.NoBody {
		result.Error = &StreamError{Status: 502, Kind: KindOther, Message: "Devin returned an empty response body"}
		return nil, result, nil
	}
	stream, perr := Peek(resp.Body, token)
	if perr != nil {
		result.Error = perr
		return nil, result, nil
	}
	return stream, result, nil
}

// Models lists the routable models (disabled entries, fusion combos and
// server-side routers dropped) and persists their metadata for pricing and
// capability lookups.
func (p *Provider) Models(ctx context.Context) ([]Model, error) {
	token, err := p.ResolveToken(ctx)
	if err != nil {
		return nil, err
	}
	catalog, err := FetchModels(ctx, p.client(), token, p.baseURL())
	if err != nil {
		return nil, err
	}
	usable := make([]Model, 0, len(catalog))
	for _, m := range catalog {
		if IsRoutable(m) {
			usable = append(usable, m)
		}
	}
	SaveModelMeta(usable)
	return usable, nil
}

// Quota reads the live plan and daily/weekly windows.
func (p *Provider) Quota(ctx context.Context) (UserStatus, error) {
	token, err := p.ResolveToken(ctx)
	if err != nil {
		return UserStatus{}, err
	}
	return FetchUserStatus(ctx, p.client(), token, p.baseURL())
}

// ModelScoped reports whether a rate-limit refusal caps only the model (the
// free tier's "Reached free model rate limit"), so the cooldown should be
// model-scoped rather than bench the whole provider. Port of the modelScoped
// test in markDevinRefusal (src/upstream.ts).
func ModelScoped(err *StreamError) bool {
	return modelScopedPattern.MatchString(err.Message)
}
