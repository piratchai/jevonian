package cursor

import (
	"context"
	"io"
	"net/http"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/wire"
)

// Provider is the server-facing seam for one Cursor subscription provider.
// Every field is optional: HTTP defaults to http.DefaultClient, BaseURL to
// DefaultBaseURL, Token to the cursor-agent sign-in Token, AgentURL to
// ResolveAgentURL, and ClientVersion to the installed CLI's.
type Provider struct {
	HTTP    *http.Client
	BaseURL string
	Login   *config.ProviderLogin
	// Token overrides credential resolution (the server's OAuth cache plugs in here).
	Token func(ctx context.Context) (string, error)
	// AgentURL overrides the server-config resolution (for tests).
	AgentURL string
	// ClientVersion overrides the version header (for tests).
	ClientVersion string
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

// ResolveToken returns the token Chat would use.
func (p *Provider) ResolveToken(ctx context.Context) (string, error) {
	if p.Token != nil {
		return p.Token(ctx)
	}
	return Token(ctx, p.Login)
}

func (p *Provider) agentURL(ctx context.Context, token string) string {
	if p.AgentURL != "" {
		return p.AgentURL
	}
	return ResolveAgentURL(ctx, p.client(), token, p.BaseURL)
}

func (p *Provider) clientVersion() string {
	if p.ClientVersion != "" {
		return p.ClientVersion
	}
	return ClientVersion()
}

// ChatRequest is one Chat Completions turn bound for Cursor.
type ChatRequest struct {
	// Body is an OpenAI Chat Completions object (messages, tools, …).
	Body  wire.Body
	Model string
	// Effort is the reasoner's effort word when the model family has variants
	// ("low", "medium", "high", "xhigh", "max"); "" lets Cursor's default apply.
	Effort string
	// Fast asks for the family's fast variant when one exists.
	Fast bool
	// Stream selects ChatResult.Stream (OpenAI SSE) over ChatResult.Completion.
	Stream bool
	// Stream callbacks; see StreamOptions.
	OnFinish func(Finish)
	OnEvent  func(wire.StreamEvent)
	// RequestID is the x-request-id header; empty mints one.
	RequestID string
}

// ChatResult is either a classified refusal (Error, nothing sent to the
// client yet — safe to fail over) or an answer: Stream for stream requests,
// Completion + Finish for the rest. Finish.Error is a refusal that arrived
// after content began.
type ChatResult struct {
	Error      *StreamError
	Stream     io.ReadCloser
	Completion wire.Body
	Finish     Finish
	// Status is the upstream HTTP status (0 on transport error).
	Status int
}

// Chat runs one Cursor turn: read the conversation out of the OpenAI body,
// open a Connect Run, and fold or relay its events. A returned error is a
// transport failure (eligible for transport failover); refusals come back as
// ChatResult.Error.
func (p *Provider) Chat(ctx context.Context, req ChatRequest) (ChatResult, error) {
	token, err := p.ResolveToken(ctx)
	if err != nil {
		return ChatResult{}, err
	}
	conversation := ConversationBody(req.Body)
	model := req.Model
	if req.Effort != "" || req.Fast {
		model = ModelID(model, req.Effort, req.Fast)
	}
	agentURL := p.agentURL(ctx, token)
	requestID := req.RequestID
	if requestID == "" {
		requestID = uuid()
	}
	turn, err := Run(ctx, p.client(), RunOptions{
		Token:         token,
		AgentURL:      agentURL,
		SystemPrompt:  conversation.System,
		Messages:      conversation.Messages,
		Tools:         conversation.Tools,
		Model:         model,
		LastUser:      LastUser(conversation.Messages),
		RequestID:     requestID,
		ClientVersion: p.clientVersion(),
	})
	if err != nil {
		return ChatResult{}, err
	}
	if turn.Error != nil {
		return ChatResult{Error: turn.Error, Status: turn.Error.Status}, nil
	}
	result := ChatResult{Status: 200}
	if req.Stream {
		result.Stream = ToChatStream(model, turn.Events, StreamOptions{OnFinish: req.OnFinish, OnEvent: req.OnEvent})
		return result, nil
	}
	defer turn.Events.Close()
	result.Completion, result.Finish = ChatCompletion(model, turn.Events)
	return result, nil
}

// Models refreshes Cursor's own model list (fetches `cursor-agent models`,
// keeps the raw list on disk, returns the collapsed picker list). Port of the
// cursor branch in src/catalog.ts.
func (p *Provider) Models(ctx context.Context) ([]Model, error) {
	raw, err := FetchModels(ctx)
	if err != nil {
		return nil, err
	}
	return SaveCatalog(raw).Models, nil
}

// CachedModels is the collapsed picker list as last fetched, or nil.
func (p *Provider) CachedModels() []Model {
	if file := LoadCatalog(); file != nil {
		return file.Models
	}
	return nil
}

// Account is who Cursor's CLI says is signed in for this provider, or false.
func (p *Provider) Account(ctx context.Context) (Account, bool) {
	return FetchAccount(ctx)
}

// HasCredential reports whether this provider has a sign-in to read.
func (p *Provider) HasCredential() bool { return HasCredential(p.Login) }
