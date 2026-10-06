package upstream

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/oauth"
	freebuffprovider "github.com/xinyao27/jevonian/internal/provider/freebuff"
	"github.com/xinyao27/jevonian/internal/quota"
	"github.com/xinyao27/jevonian/internal/wire"
)

// freebuffAdapter is Freebuff's OpenAI-shaped, stream-only egress. The body is
// built by the shared OpenAI adapter; freebuffAttempt then wraps it in the CLI
// envelope, because the envelope needs a live session and run that only exist
// once the attempt starts.
type freebuffAdapter struct{ openaiAdapter }

func (*freebuffAdapter) AlwaysStreams() bool { return true }

func (*freebuffAdapter) EndpointURL(p config.Provider) string {
	return freebuffprovider.ChatURL(p.BaseURL)
}

// freebuffManager returns the session manager for a provider+token, creating
// it on first use. One token → one manager, so every turn on an account shares
// a single upstream session instead of superseding each other's.
func (r *Runner) freebuffManager(p config.Provider, token string) *freebuffprovider.Manager {
	key := p.Name + "\x00" + token
	r.freebuffMu.Lock()
	defer r.freebuffMu.Unlock()
	if r.freebuff == nil {
		r.freebuff = map[string]*freebuffprovider.Manager{}
	}
	if m, ok := r.freebuff[key]; ok {
		return m
	}
	m := freebuffprovider.NewManager(freebuffprovider.Options{
		HTTP:     r.deps.HTTP,
		BaseURL:  p.BaseURL,
		Token:    token,
		Resolver: freebuffprovider.NewResolver(nil),
		Now:      r.deps.Now,
		Sleep:    r.deps.Sleep,
	})
	r.freebuff[key] = m
	return m
}

// freebuffAttempt runs one Freebuff turn: resolve the model, take a session
// and a run, wrap the body in the CLI envelope, POST it, and — when the
// server says the session or run went stale — mint fresh ones and retry once.
func (r *Runner) freebuffAttempt(ctx context.Context, req AttemptRequest, at *Attempt, body wire.Body, auth oauth.AuthResolution) {
	provider := at.Entry.Provider
	mgr := r.freebuffManager(provider, auth.Token)

	model, ok := freebuffprovider.NewResolver(nil).Resolve(at.Entry.Model)
	if !ok {
		// An id the table does not know is passed through under the default
		// agent: the server is the authority on what exists.
		model, _ = freebuffprovider.NewResolver(nil).Resolve(freebuffprovider.DefaultModel)
		model.ID = strings.TrimPrefix(at.Entry.Model, "freebuff/")
	}
	prepared := freebuffprovider.Prepare(body, model)

	headers := make(http.Header)
	freebuffprovider.SetHeaders(headers, auth.Token)
	for k, v := range auth.Headers {
		headers.Set(k, v)
	}
	url := freebuffprovider.ChatURL(provider.BaseURL)

	// acquire takes the session and run. A stale-session verdict on the first
	// try drops the cache and asks for another round; every other failure is
	// recorded on the attempt as final.
	type acquired int
	const (
		gotBoth acquired = iota
		again
		final
	)
	acquire := func(try int) (freebuffprovider.Session, string, acquired) {
		sess, err := mgr.Ensure(ctx, model.ID)
		if err == nil {
			var runID string
			if runID, err = mgr.Run(ctx, model.ID, model.Agent); err == nil {
				return sess, runID, gotBoth
			}
		}
		var fe *freebuffprovider.Error
		if errors.As(err, &fe) && fe.Retryable() && try == 0 {
			mgr.Invalidate(model.ID)
			at.Retries++
			return freebuffprovider.Session{}, "", again
		}
		if ctx.Err() != nil {
			at.Err = err
			at.Outcome = Outcome{Kind: OutcomeCanceled}
			return freebuffprovider.Session{}, "", final
		}
		r.freebuffFailed(at, provider, err)
		return freebuffprovider.Session{}, "", final
	}

	var last *freebuffprovider.Error
	for try := 0; try < 2; try++ {
		sess, runID, state := acquire(try)
		if state == final {
			return
		}
		if state == again {
			continue
		}

		h := headers.Clone()
		h.Set("x-freebuff-model", model.ID)
		h.Set("x-freebuff-instance-id", sess.InstanceID)
		opts := PostOptions{
			URL:     url,
			Headers: h,
			Body:    MarshalBody(freebuffprovider.WithMetadata(prepared, runID, sess.InstanceID)),
			Stream:  at.Stream,
		}
		result, perr := r.client.Post(ctx, opts)
		at.Retries += result.Retries
		if perr != nil {
			at.Err = perr
			at.Outcome = Classify(0, "", perr)
			if ctx.Err() != nil {
				at.Outcome = Outcome{Kind: OutcomeCanceled}
			}
			return
		}

		resp := result.Response
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if at.Stream {
				guarded, gerr := GuardFirstByte(ctx, resp, r.client.Timeouts.FirstByteMS)
				if gerr != nil {
					at.Err = gerr
					at.Outcome = Classify(0, "", gerr)
					if ctx.Err() != nil {
						at.Outcome = Outcome{Kind: OutcomeCanceled}
					}
					return
				}
				resp = guarded
			}
			at.Status = resp.StatusCode
			at.Response = resp
			at.Outcome = Outcome{Kind: OutcomeSuccess}
			return
		}

		fe := freebuffprovider.Classify(resp.StatusCode, result.Text, resp.Header, r.now())
		last = fe
		if fe.Retryable() && try == 0 {
			if fe.Kind == freebuffprovider.KindRun {
				mgr.InvalidateRun(model.ID, model.Agent)
			} else {
				mgr.Invalidate(model.ID)
			}
			at.Retries++
			continue
		}
		break
	}
	if last != nil {
		r.freebuffFailed(at, provider, last)
	}
}

// freebuffFailed records a classified refusal on the attempt and benches the
// provider when the verdict is about the account. A stale session or run that
// survived its one retry is recorded as a provider refusal so routing moves on.
func (r *Runner) freebuffFailed(at *Attempt, provider config.Provider, err error) {
	var fe *freebuffprovider.Error
	if !errors.As(err, &fe) {
		at.Err = err
		at.Outcome = Classify(0, "", err)
		return
	}
	at.Status, at.Text = fe.Status, fe.Message
	if fe.Retryable() {
		at.Outcome = Outcome{Kind: OutcomeProviderRefusal}
		return
	}
	switch fe.Kind {
	case freebuffprovider.KindRateLimit, freebuffprovider.KindBanned:
		// The daily free quota (or a ban) is gone until the stated reset; the
		// reset time keeps routing off this account instead of retrying a minute later.
		at.Outcome = Outcome{Kind: OutcomeQuotaRefusal, Label: "day"}
		reset := fe.ResetsAt
		if reset.IsZero() {
			reset = r.now().Add(quota.ProviderCooldown)
		}
		at.Outcome.Resets = reset.UnixMilli()
		if r.deps.Quota != nil {
			r.deps.Quota.MarkSpent(provider.Name, quota.MarkSpentOptions{Label: "day", ResetsAt: reset})
		}
	case freebuffprovider.KindAuth:
		at.Outcome = Outcome{Kind: OutcomeProviderRefusal}
		if r.deps.Auth != nil {
			r.deps.Auth.Invalidate(string(provider.OAuthSource), provider.Login)
		}
	case freebuffprovider.KindWaitingRoom:
		// The queue is a transient host condition: fail over without benching.
		wait := fe.Retry
		if wait <= 0 {
			wait = quota.ProviderCooldown
		}
		at.Outcome = Outcome{Kind: OutcomeProviderRefusal}
		if r.deps.Quota != nil {
			r.deps.Quota.MarkSpent(provider.Name, quota.MarkSpentOptions{Label: "waiting-room", ResetsAt: r.now().Add(min(wait, 2*time.Minute))})
		}
	case freebuffprovider.KindModel:
		at.Outcome = Outcome{Kind: OutcomeProviderRefusal}
		if r.deps.Quota != nil {
			r.deps.Quota.MarkSpent(provider.Name, quota.MarkSpentOptions{
				Label: "model-unavailable", Model: at.Entry.Model, ResetsAt: r.now().Add(10 * time.Minute),
			})
		}
	default:
		at.Outcome = Classify(fe.Status, fe.Message, nil)
	}
}
