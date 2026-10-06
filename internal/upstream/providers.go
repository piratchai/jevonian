package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/oauth"
	cursorprovider "github.com/xinyao27/jevonian/internal/provider/cursor"
	"github.com/xinyao27/jevonian/internal/provider/devin"
	"github.com/xinyao27/jevonian/internal/provider/workbuddy"
	"github.com/xinyao27/jevonian/internal/quota"
	"github.com/xinyao27/jevonian/internal/softstream"
	"github.com/xinyao27/jevonian/internal/wire"
)

// rpcAdapter prepares a chat-shaped conversation; provider modules own Connect
// framing and bidirectional IO. The runner still owns guards and classification.
// chat is the one provider-specific step: it runs the turn and maps the
// provider's refusal onto an Outcome.
type rpcAdapter struct {
	openaiAdapter
	chat rpcChat
}

func (*rpcAdapter) AlwaysStreams() bool  { return false }
func (*rpcAdapter) ExclusiveInput() bool { return true }
func (a *rpcAdapter) runAttempt(r *Runner, ctx context.Context, req AttemptRequest, at *Attempt, body wire.Body, auth oauth.AuthResolution) {
	r.rpcAttempt(ctx, req, at, a.chat, body, auth.Token)
}

// rpcTurn is one Connect-RPC turn's normalized result.
type rpcTurn struct {
	completion wire.Body
	stream     io.ReadCloser
	status     int
	retries    int
	// message is the refusal text; empty on success.
	message string
	outcome Outcome
}

// rpcChat runs one provider turn. A returned error is a transport failure.
type rpcChat func(r *Runner, ctx context.Context, req AttemptRequest, at *Attempt, body wire.Body, token string) (rpcTurn, error)

// devinChat runs one Devin turn. A 401 forces a fresh token (the user may have
// signed in again) and the Connect body is rebuilt around it, once.
// src/upstream.ts 401 refresh.
func devinChat(r *Runner, ctx context.Context, req AttemptRequest, at *Attempt, body wire.Body, token string) (rpcTurn, error) {
	var out rpcTurn
	p := devin.NewProvider(at.Entry.Provider, r.deps.HTTP)
	p.Token = func(context.Context) (string, error) { return token, nil }
	result, err := p.Chat(ctx, devin.ChatRequest{Body: body, Model: at.Entry.Model, Stream: req.Stream})
	if err == nil && result.Status == http.StatusUnauthorized && at.Entry.Provider.Auth == config.AuthOAuth &&
		at.Entry.Provider.OAuthSource != "" && at.Entry.Provider.OAuthSource != config.OAuthStatic {
		r.deps.Auth.Invalidate(string(at.Entry.Provider.OAuthSource), at.Entry.Provider.Login)
		if fresh, refreshErr := r.deps.Auth.ResolveProviderAuth(ctx, at.Entry.Provider, oauth.WireKind(at.Plan.Wire), req.ExtraHeaders.Get("x-jevonian-session")); refreshErr == nil && fresh.Token != "" {
			token = fresh.Token
			out.retries++
			result, err = p.Chat(ctx, devin.ChatRequest{Body: body, Model: at.Entry.Model, Stream: req.Stream})
		}
	}
	out.status, out.completion, out.stream = result.Status, result.Completion, result.Stream
	if result.PolicyRetried {
		out.retries++
	}
	failure := result.Error
	if failure == nil {
		failure = result.Finish.Error
	}
	if failure == nil {
		return out, err
	}
	out.status = devin.SurfacedStatus(failure)
	out.message = failure.Message
	out.outcome = Classify(out.status, out.message, nil)
	if failure.Kind == devin.KindContentPolicy {
		out.outcome.Kind = OutcomeClientError
	}
	if failure.Kind == devin.KindQuota || failure.Kind == devin.KindRateLimit || failure.RateLimit {
		out.outcome.Kind = OutcomeQuotaRefusal
		model := ""
		if devin.ModelScoped(failure) {
			model = at.Entry.Model
		}
		reset := failure.ResetsAt
		if reset.IsZero() {
			reset = r.now().Add(quota.ProviderCooldown)
		}
		if r.deps.Quota != nil {
			reason := strings.TrimSpace(failure.Message)
			if reason == "" {
				reason = string(failure.Kind)
			}
			r.deps.Quota.MarkSpent(at.Entry.Provider.Name, quota.MarkSpentOptions{Label: reason, Model: model, ResetsAt: reset})
		}
	}
	return out, err
}

// cursorChat runs one Cursor turn and maps its refusal kinds.
func cursorChat(r *Runner, ctx context.Context, req AttemptRequest, at *Attempt, body wire.Body, token string) (rpcTurn, error) {
	var out rpcTurn
	p := cursorprovider.NewProvider(at.Entry.Provider, r.deps.HTTP)
	p.Token = func(context.Context) (string, error) { return token, nil }
	result, err := p.Chat(ctx, cursorprovider.ChatRequest{Body: body, Model: at.Entry.Model, Effort: at.Entry.Effort, Stream: req.Stream})
	out.status, out.completion, out.stream = result.Status, result.Completion, result.Stream
	failure := result.Error
	if failure == nil {
		failure = result.Finish.Error
	}
	if failure == nil {
		return out, err
	}
	out.status = cursorprovider.SurfacedStatus(failure)
	out.message = failure.Message
	out.outcome = Classify(out.status, out.message, nil)
	switch failure.Kind {
	case cursorprovider.KindContext:
		out.outcome.Kind = OutcomeContextOverflow
	case cursorprovider.KindInvalid:
		out.outcome.Kind = OutcomeClientError
	case cursorprovider.KindAuth, cursorprovider.KindRegion:
		out.outcome.Kind = OutcomeProviderRefusal
	}
	if failure.Kind == cursorprovider.KindAuth {
		r.deps.Auth.Invalidate(string(at.Entry.Provider.OAuthSource), at.Entry.Provider.Login)
	}
	if out.outcome.Kind == OutcomeQuotaRefusal || out.outcome.Kind == OutcomeRateLimit {
		r.bench(at.Entry.Provider, &Attempt{Outcome: out.outcome, Text: out.message})
	}
	return out, err
}

type workbuddyAdapter struct{ openaiAdapter }

func (*workbuddyAdapter) AlwaysStreams() bool { return true }
func (a *workbuddyAdapter) Prepare(in PrepInput) (wire.Body, error) {
	policy := in.PromptPolicy
	in.PromptPolicy = config.PromptPolicyConfig{}
	body, err := a.openaiAdapter.Prepare(in)
	if err != nil {
		return nil, err
	}
	body = workbuddy.EnsureSystem(body)
	body["stream"] = true
	return wire.RewritePromptBodies(body, policy), nil
}

// RPC contexts remain alive while the response is consumed. The initial clock
// includes protocol negotiation and the leading-refusal peek, not the whole
// stream; non-stream turns retain the total clock instead.
func (r *Runner) rpcAttempt(parent context.Context, req AttemptRequest, at *Attempt, chat rpcChat, body wire.Body, token string) {
	ctx, cancel := context.WithCancelCause(parent)
	ms, phase := r.client.Timeouts.TotalMS, PhaseTotal
	if req.Stream {
		ms, phase = r.client.Timeouts.FirstByteMS, PhaseFirstByte
	}
	var timer *time.Timer
	done := make(chan struct{})
	if ms > 0 {
		timer = time.AfterFunc(time.Duration(ms)*time.Millisecond, func() { cancel(&TimeoutError{Phase: phase, MS: ms}); close(done) })
	}
	cleanup := func() {
		if timer != nil {
			timer.Stop()
		}
		cancel(nil)
	}
	turn, err := chat(r, ctx, req, at, body, token)
	stream := turn.stream
	at.Retries = turn.retries
	if err != nil || ctx.Err() != nil {
		if stream != nil {
			stream.Close()
		}
		if ctx.Err() != nil {
			err = mapContextError(ctx, ctx.Err())
		}
		at.Err = err
		at.Outcome = Classify(0, "", err)
		if parent.Err() != nil {
			at.Outcome.Kind = OutcomeCanceled
		}
		cleanup()
		return
	}
	if turn.message != "" {
		if stream != nil {
			stream.Close()
		}
		cleanup()
		at.Status = turn.status
		at.Outcome = turn.outcome
		at.Text = wire.MarshalJSON(map[string]any{"error": map[string]string{"message": turn.message, "type": "api_error"}})
		return
	}
	if req.Stream && timer != nil && !timer.Stop() {
		<-done
		if stream != nil {
			stream.Close()
		}
		at.Err = mapContextError(ctx, ctx.Err())
		at.Outcome = Classify(0, "", at.Err)
		cleanup()
		return
	}
	if stream == nil {
		data, encodeErr := json.Marshal(turn.completion)
		if encodeErr != nil {
			cleanup()
			at.Err = encodeErr
			at.Outcome = Classify(0, "", encodeErr)
			return
		}
		stream = io.NopCloser(bytes.NewReader(data))
	}
	if req.Stream {
		stream = softstream.IdleGuard(stream, r.client.Timeouts.IdleMS)
	}
	at.Stream = req.Stream
	at.Status = 200
	at.Outcome = Outcome{Kind: OutcomeSuccess}
	contentType := "application/json"
	if req.Stream {
		contentType = "text/event-stream"
	}
	at.Response = &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: &contextBody{ReadCloser: stream, ctx: ctx, cleanup: cleanup, once: sync.Once{}}}
}
