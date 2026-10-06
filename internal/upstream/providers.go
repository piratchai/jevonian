package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
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
type rpcAdapter struct {
	openaiAdapter
	typ config.ProviderType
}

func (*rpcAdapter) AlwaysStreams() bool { return false }

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
func (r *Runner) rpcAttempt(parent context.Context, req AttemptRequest, at *Attempt, adapter *rpcAdapter, body wire.Body, token string) {
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
	var completion wire.Body
	var stream io.ReadCloser
	var err error
	var status int
	var message string
	var outcome Outcome
	var retries int
	if adapter.typ == config.ProviderTypeDevin {
		p := devin.NewProvider(at.Entry.Provider, r.deps.HTTP)
		p.Token = func(context.Context) (string, error) { return token, nil }
		result, e := p.Chat(ctx, devin.ChatRequest{Body: body, Model: at.Entry.Model, Stream: req.Stream})
		// A 401 forces a fresh token (the user may have signed in again) and the
		// Connect body is rebuilt around it, once. src/upstream.ts 401 refresh.
		if e == nil && result.Status == http.StatusUnauthorized && at.Entry.Provider.Auth == config.AuthOAuth &&
			at.Entry.Provider.OAuthSource != "" && at.Entry.Provider.OAuthSource != config.OAuthStatic {
			r.deps.Auth.Invalidate(string(at.Entry.Provider.OAuthSource), at.Entry.Provider.Login)
			if fresh, refreshErr := r.deps.Auth.ResolveProviderAuth(ctx, at.Entry.Provider, oauth.WireKind(at.Plan.Wire), req.ExtraHeaders.Get("x-jevonian-session")); refreshErr == nil && fresh.Token != "" {
				token = fresh.Token
				retries++
				result, e = p.Chat(ctx, devin.ChatRequest{Body: body, Model: at.Entry.Model, Stream: req.Stream})
			}
		}
		err = e
		status = result.Status
		completion = result.Completion
		stream = result.Stream
		if result.PolicyRetried {
			retries++
		}
		failure := result.Error
		if failure == nil {
			failure = result.Finish.Error
		}
		if failure != nil {
			status = devin.SurfacedStatus(failure)
			message = failure.Message
			outcome = Classify(status, message, nil)
			if failure.Kind == devin.KindContentPolicy {
				outcome.Kind = OutcomeClientError
			}
			if failure.Kind == devin.KindQuota || failure.Kind == devin.KindRateLimit {
				outcome.Kind = OutcomeQuotaRefusal
				model := ""
				if devin.ModelScoped(failure) {
					model = at.Entry.Model
				}
				reset := failure.ResetsAt
				if reset.IsZero() {
					reset = r.now().Add(quota.ProviderCooldown)
				}
				if r.deps.Quota != nil {
					r.deps.Quota.MarkSpent(at.Entry.Provider.Name, quota.MarkSpentOptions{Label: "limit", Model: model, ResetsAt: reset})
				}
			}
		}
	} else {
		p := cursorprovider.NewProvider(at.Entry.Provider, r.deps.HTTP)
		p.Token = func(context.Context) (string, error) { return token, nil }
		result, e := p.Chat(ctx, cursorprovider.ChatRequest{Body: body, Model: at.Entry.Model, Effort: at.Entry.Effort, Stream: req.Stream})
		err = e
		status = result.Status
		completion = result.Completion
		stream = result.Stream
		failure := result.Error
		if failure == nil {
			failure = result.Finish.Error
		}
		if failure != nil {
			status = cursorprovider.SurfacedStatus(failure)
			message = failure.Message
			outcome = Classify(status, message, nil)
			switch failure.Kind {
			case cursorprovider.KindContext:
				outcome.Kind = OutcomeContextOverflow
			case cursorprovider.KindInvalid:
				outcome.Kind = OutcomeClientError
			case cursorprovider.KindAuth, cursorprovider.KindRegion:
				outcome.Kind = OutcomeProviderRefusal
			}
			if failure.Kind == cursorprovider.KindAuth {
				r.deps.Auth.Invalidate(string(at.Entry.Provider.OAuthSource), at.Entry.Provider.Login)
			}
			if outcome.Kind == OutcomeQuotaRefusal || outcome.Kind == OutcomeRateLimit {
				r.bench(at.Entry.Provider, &Attempt{Outcome: outcome, Text: message})
			}
		}
	}
	at.Retries = retries
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
	if message != "" {
		if stream != nil {
			stream.Close()
		}
		cleanup()
		at.Status = status
		at.Outcome = outcome
		at.Text = wire.MarshalJSON(map[string]any{"error": map[string]string{"message": message, "type": "api_error"}})
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
		data, encodeErr := json.Marshal(completion)
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
