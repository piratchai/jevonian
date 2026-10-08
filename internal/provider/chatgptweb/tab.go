package chatgptweb

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/remote"
)

// TabSession owns a single dedicated tab on an existing Chrome instance.
// It guarantees we never adopt, navigate, or close any user tabs,
// and NEVER close the Chrome browser process.
type TabSession struct {
	TargetID    string
	HostPort    string
	AllocCtx    context.Context
	AllocCancel context.CancelFunc
	BrowserCtx  context.Context
	BrowserStop context.CancelFunc
	TabCtx      context.Context
	TabCancel   context.CancelFunc
}

// createTabSession creates a new target tab via Chrome DevTools Protocol,
// attaches to it using remote.NewAllocator and chromedp.WithTargetID,
// and ensures proper lifecycle isolation.
func createTabSession(ctx context.Context, _ *http.Client, hostPort string) (*TabSession, *Error) {
	// Connect to the browser-level CDP endpoint first. CreateTarget with
	// newWindow=true guarantees model discovery uses its own visible window.
	allocURL := fmt.Sprintf("http://%s", hostPort)
	allocCtx, allocCancel := remote.NewAllocator(context.Background(), allocURL)
	browserCtx, browserCancel := chromedp.NewContext(allocCtx)
	// Create dedicated target tab directly.
	createCtx, cancelCreate := context.WithTimeout(browserCtx, 15*time.Second)
	created, err := chromedp.CallBrowser(createCtx, target.CreateTarget, target.CreateTargetParams{
		URL: "about:blank",
	})
	cancelCreate()
	if err != nil {
		browserCancel()
		allocCancel()
		kind, status := KindBrowser, http.StatusBadGateway
		if ctx.Err() != nil {
			kind, status = KindTimeout, http.StatusGatewayTimeout
		}
		return nil, &Error{Status: status, Kind: kind, Message: fmt.Sprintf("failed to create dedicated browser window: %v", err)}
	}
	targetID := string(created.TargetID)

	// Attach to the owned target. Detach-on-cancel lets request timeouts end the
	// CDP event loop without closing the window that the user may inspect.
	tabCtx, tabCancel := chromedp.NewContext(allocCtx, chromedp.WithTargetID(created.TargetID), chromedp.WithDetachOnCancel())
	if _, err := chromedp.Run(tabCtx, chromedp.Action[chromedp.Void](func(ctx context.Context, target *chromedp.Target) (chromedp.Void, error) {
		return chromedp.Void{}, nil
	})); err != nil {
		closeOwnedTarget(hostPort, string(created.TargetID))
		tabCancel()
		browserCancel()
		allocCancel()
		kind, status := KindBrowser, http.StatusBadGateway
		if ctx.Err() != nil {
			kind, status = KindTimeout, http.StatusGatewayTimeout
		}
		return nil, &Error{Status: status, Kind: kind, Message: fmt.Sprintf("failed to attach to owned ChatGPT target: %v", err)}
	}
	return &TabSession{
		TargetID:    targetID,
		HostPort:    hostPort,
		AllocCtx:    allocCtx,
		AllocCancel: allocCancel,
		BrowserCtx:  browserCtx,
		BrowserStop: browserCancel,
		TabCtx:      tabCtx,
		TabCancel:   tabCancel,
	}, nil
}

// Close destroys our owned target tab and frees CDP resources.
// It explicitly never closes the Chrome browser process.
// Detach leaves the owned target open for inspection but releases all CDP resources.
func (s *TabSession) Detach() {
	if s == nil {
		return
	}
	// WithDetachOnCancel detaches the target session but keeps the target open.
	if s.TabCancel != nil {
		s.TabCancel()
	}
	if s.BrowserStop != nil {
		s.BrowserStop()
	}
	if s.AllocCancel != nil {
		s.AllocCancel()
	}
}

func (s *TabSession) Close() {
	if s == nil {
		return
	}

	// Close only the target created for this operation. The session's own
	// connection can already be gone by now, so an explicit close through it
	// is unreliable, and WithDetachOnCancel makes chromedp leave the target
	// open. Use an independent connection instead.
	closeOwnedTarget(s.HostPort, s.TargetID)
	// Detach from the browser after closing our owned window.
	if s.TabCancel != nil {
		s.TabCancel()
	}
	if s.BrowserStop != nil {
		s.BrowserStop()
	}
	if s.AllocCancel != nil {
		s.AllocCancel()
	}
}

// closeOwnedTarget closes one target this session created. It uses a fresh,
// independent CDP connection so that a torn-down session connection cannot
// break the close, and falls back to the HTTP /json/close endpoint.
func closeOwnedTarget(hostPort, targetID string) {
	if targetID == "" || hostPort == "" {
		return
	}
	closed := false
	allocCtx, allocCancel := remote.NewAllocator(context.Background(), fmt.Sprintf("http://%s", hostPort))
	browserCtx, browserCancel := chromedp.NewContext(allocCtx)
	closeCtx, cancelClose := context.WithTimeout(browserCtx, 3*time.Second)
	if _, err := chromedp.CallBrowser(closeCtx, target.CloseTarget, target.CloseTargetParams{TargetID: target.ID(targetID)}); err == nil {
		closed = true
	}
	cancelClose()
	browserCancel()
	allocCancel()
	if closed {
		return
	}
	// HTTP fallback: Chrome exposes /json/close/<id> on the CDP port.
	client := &http.Client{Timeout: 2 * time.Second}
	if resp, err := client.Get(fmt.Sprintf("http://%s/json/close/%s", hostPort, targetID)); err == nil {
		resp.Body.Close()
	}
}

// BestEffortStop attempts to click the stop-generating button in the browser before closing.
func (s *TabSession) BestEffortStop() {
	if s == nil || s.TabCtx == nil {
		return
	}
	stopJS := `(() => {
		var btn = document.querySelector('button[aria-label*="Stop" i], button[data-testid="stop-button"]');
		if (btn) { btn.click(); return true; }
		return false;
	})()`
	_ = chromedp.Do(s.TabCtx,
		chromedp.Evaluate[chromedp.Void](stopJS),
	)
}
