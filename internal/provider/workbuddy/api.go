package workbuddy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/xinyao27/jevonian/internal/oauth/httpx"
	"github.com/xinyao27/jevonian/internal/provider/multiacct"
)

// Client is the WorkBuddy HTTP surface; it reads through an injected
// *http.Client so tests (and the server's shared transport) can drive it.
type Client struct {
	HTTP *http.Client
	// OpenBrowser opens the sign-in URL; when nil the platform launcher is used.
	OpenBrowser func(url string)
	// PollInterval / PollTimeout bound the browser sign-in poll.
	PollInterval time.Duration
	PollTimeout  time.Duration
	// Now/After/Sleep are injectable for tests.
	Now   func() time.Time
	Sleep func(time.Duration)
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) sleep(ctx context.Context, d time.Duration) error {
	if c.Sleep != nil {
		c.Sleep(d)
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// call performs one JSON RPC-shaped call and returns the `data` member.
func (c *Client) call(ctx context.Context, method, rawURL string, headers map[string]string, body any) (map[string]any, error) {
	var payload []byte
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		payload = data
	}
	h := map[string]string{
		"accept":     "application/json",
		"user-agent": "WorkBuddy/" + UAVersion,
	}
	for k, v := range headers {
		h[k] = v
	}
	if payload != nil {
		h["content-type"] = "application/json"
	}
	status, text, err := httpx.ReadAll(ctx, c.HTTP, httpx.Request{
		Method:  method,
		URL:     rawURL,
		Headers: h,
		Body:    payload,
	})
	if err != nil {
		return nil, err
	}
	var parsed any
	if jsonErr := json.Unmarshal(text, &parsed); jsonErr != nil {
		if status < 200 || status >= 300 {
			return nil, fmt.Errorf("WorkBuddy %d: %s", status, truncate(string(text), 200))
		}
		return nil, errors.New("WorkBuddy returned non-JSON")
	}
	j := asRecord(parsed)
	code := 0
	if v, ok := number(j["code"]); ok {
		code = int(v)
	} else if status < 200 || status >= 300 {
		code = status
	}
	if code != 0 {
		msg := PlainString(j["msg"])
		if msg == "" {
			msg = fmt.Sprintf("error %d", code)
		}
		return nil, &APIError{Code: code, Message: msg}
	}
	return asRecord(j["data"]), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func mergeRefreshed(current Creds, got map[string]any, now time.Time) (Creds, error) {
	access := PlainString(got["accessToken"])
	if access == "" {
		return Creds{}, errors.New("WorkBuddy gave no refreshed token")
	}
	next := current
	next.AccessToken = access
	if rt := PlainString(got["refreshToken"]); rt != "" {
		next.RefreshToken = rt
	}
	expiresAt := numberOrZero(got["expiresAt"])
	if expiresAt == 0 {
		if v, ok := number(got["expiresIn"]); ok && v > 0 {
			expiresAt = now.UnixMilli() + int64(v*1000)
		}
	}
	if expiresAt != 0 {
		next.ExpiresAt = expiresAt
	}
	refreshExpiresAt := numberOrZero(got["refreshExpiresAt"])
	if refreshExpiresAt == 0 {
		if v, ok := number(got["refreshExpiresIn"]); ok && v > 0 {
			refreshExpiresAt = now.UnixMilli() + int64(v*1000)
		}
	}
	if refreshExpiresAt != 0 {
		next.RefreshExpiresAt = refreshExpiresAt
	}
	if d := PlainString(got["domain"]); d != "" {
		next.Domain = d
	}
	if tt := PlainString(got["tokenType"]); tt != "" {
		next.TokenType = tt
	}
	return next, nil
}

// Refresh exchanges the session's refresh token for a new access token.
func (c *Client) Refresh(ctx context.Context, creds Creds, endpoint string) (Creds, error) {
	root := strings.TrimRight(endpoint, "/")
	if root == "" {
		root = Endpoint
	}
	got, err := c.call(ctx, http.MethodPost, root+"/v2/plugin/auth/token/refresh",
		map[string]string{
			"x-refresh-token":       creds.RefreshToken,
			"x-auth-refresh-source": "plugin",
			"x-domain":              domainOf(creds),
		}, map[string]any{})
	if err != nil {
		return Creds{}, err
	}
	return mergeRefreshed(creds, got, c.now())
}

// Resolve returns fresh credentials for a request. Desktop sessions are never
// written; Jevonian-stored sessions are updated after a successful refresh.
// On refresh failure a still-present access token is returned instead of the
// error (the upstream will 401 it and the caller retries).
func (c *Client) Resolve(ctx context.Context, login *multiacct.Login, endpoint string) (Creds, error) {
	creds := ReadSession(login)
	if creds == nil {
		return Creds{}, ErrNoCredentials
	}
	now := c.now()
	if creds.AccessToken != "" && !nearExpiry(creds.ExpiresAt, now) {
		return *creds, nil
	}
	if creds.RefreshToken == "" ||
		(creds.RefreshExpiresAt > 0 && now.UnixMilli() >= creds.RefreshExpiresAt) {
		if creds.AccessToken != "" {
			return *creds, nil
		}
		return Creds{}, ErrSignedOut
	}
	refreshed, err := c.Refresh(ctx, *creds, endpoint)
	if err != nil {
		if creds.AccessToken != "" {
			return *creds, nil
		}
		return Creds{}, err
	}
	// Only write back when the session lives in Jevonian's store (not the desktop file).
	if SessionPath(login) != DesktopAuthPath() {
		_ = SaveSession(refreshed, login)
	}
	return refreshed, nil
}

// SignInResult is what a completed browser sign-in produced.
type SignInResult struct {
	Creds Creds
	User  string
}

// SignIn runs the Magpie-style browser sign-in: open WorkBuddy's auth URL,
// poll until the token and account are ready, then persist the session.
func (c *Client) SignIn(ctx context.Context, endpoint string, login *multiacct.Login) (SignInResult, error) {
	root := strings.TrimRight(endpoint, "/")
	if root == "" {
		root = Endpoint
	}
	poll := c.PollInterval
	if poll <= 0 {
		poll = signinPollDefault
	}
	timeout := c.PollTimeout
	if timeout <= 0 {
		timeout = signinTimeout
	}
	deadline := c.now().Add(timeout)

	state, err := c.call(ctx, http.MethodPost,
		root+"/v2/plugin/auth/state?platform=workbuddy-ai",
		map[string]string{
			"x-no-authorization":   "true",
			"x-no-user-id":         "true",
			"x-no-enterprise-id":   "true",
			"x-no-department-info": "true",
		}, map[string]any{})
	if err != nil {
		return SignInResult{}, err
	}
	stateID := PlainString(state["state"])
	authURL := PlainString(state["authUrl"])
	if stateID == "" || !strings.HasPrefix(authURL, "https://") {
		return SignInResult{}, errors.New("WorkBuddy gave no sign-in page")
	}
	u, err := url.Parse(authURL)
	if err != nil {
		return SignInResult{}, fmt.Errorf("WorkBuddy gave an unparsable sign-in page: %w", err)
	}
	q := u.Query()
	q.Set("version", AppVersion)
	q.Set("loginSessionId", requestID())
	u.RawQuery = q.Encode()
	if c.OpenBrowser != nil {
		c.OpenBrowser(u.String())
	} else {
		openBrowserOnce(u.String())
	}

	pollFor := func(path string, headers map[string]string, retryCode int) (map[string]any, error) {
		for {
			if err := ctx.Err(); err != nil {
				return nil, errors.New("WorkBuddy sign-in aborted")
			}
			if c.now().After(deadline) {
				return nil, errors.New("the sign-in expired; start again")
			}
			if err := c.sleep(ctx, poll); err != nil {
				return nil, errors.New("WorkBuddy sign-in aborted")
			}
			got, err := c.call(ctx, http.MethodGet, root+path, headers, nil)
			if err != nil {
				var apiErr *APIError
				if errors.As(err, &apiErr) && apiErr.Code == retryCode {
					continue
				}
				return nil, err
			}
			return got, nil
		}
	}

	token, err := pollFor(
		"/v2/plugin/auth/token?state="+url.QueryEscape(stateID), nil, retryCodeToken)
	if err != nil {
		return SignInResult{}, err
	}
	creds, err := mergeRefreshed(Creds{Domain: DefaultDomain}, token, c.now())
	if err != nil {
		return SignInResult{}, err
	}
	account, err := pollFor(
		"/v2/plugin/login/account?state="+url.QueryEscape(stateID),
		map[string]string{
			"authorization":      "Bearer " + creds.AccessToken,
			"x-domain":           domainOf(creds),
			"x-no-user-id":       "true",
			"x-no-enterprise-id": "true",
		}, retryCodeAccount)
	if err != nil {
		return SignInResult{}, err
	}
	uid := PlainString(account["uid"])
	if uid == "" {
		return SignInResult{}, errors.New("WorkBuddy gave no account")
	}
	nickname := strings.TrimSpace(PlainString(account["nickname"]))
	if nickname == "" {
		nickname = strings.TrimSpace(PlainString(account["phoneNumber"]))
	}
	if nickname == "" {
		nickname = uid
	}
	creds.UID = uid
	creds.Nickname = nickname
	if err := SaveSession(creds, login); err != nil {
		return SignInResult{}, err
	}
	return SignInResult{Creds: creds, User: nickname}, nil
}

// FetchModels lists the model ids WorkBuddy's config advertises, preferring the
// CLI agent's list and falling back to the top-level product catalog.
func (c *Client) FetchModels(ctx context.Context, creds Creds, endpoint string) ([]string, error) {
	root := strings.TrimRight(endpoint, "/")
	if root == "" {
		root = Endpoint
	}
	headers := AuthHeaders(creds)
	headers["user-agent"] = "CLI/" + AppVersion + " WorkBuddy/" + UAVersion
	headers["x-requested-with"] = "XMLHttpRequest"
	headers["accept"] = "application/json"
	status, text, err := httpx.ReadAll(ctx, c.HTTP, httpx.Request{
		Method:  http.MethodGet,
		URL:     root + "/v3/config",
		Headers: headers,
	})
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("WorkBuddy config: HTTP %d", status)
	}
	var parsed any
	if err := json.Unmarshal(text, &parsed); err != nil {
		return nil, errors.New("WorkBuddy returned non-JSON")
	}
	j := asRecord(parsed)
	if code, ok := number(j["code"]); ok && code != 0 {
		if msg := PlainString(j["msg"]); msg != "" {
			return nil, fmt.Errorf("WorkBuddy config: %s", msg)
		}
		return nil, fmt.Errorf("WorkBuddy config: %v", code)
	}
	data, ok := j["data"].(map[string]any)
	if !ok || data == nil {
		data = j
	}
	models := ModelsFromConfig(data)
	if len(models) == 0 {
		return nil, errors.New("WorkBuddy's config lists no models for its CLI agent")
	}
	return models, nil
}

// ModelsFromConfig prefers the CLI agent's model ids; falls back to the
// top-level product catalog.
func ModelsFromConfig(data map[string]any) []string {
	agents, _ := data["agents"].([]any)
	for _, raw := range agents {
		if asRecord(raw)["name"] != "cli" {
			continue
		}
		models, _ := asRecord(raw)["models"].([]any)
		var ids []string
		for _, m := range models {
			if s, ok := m.(string); ok && s != "" {
				ids = append(ids, s)
			}
		}
		if len(ids) > 0 {
			return ids
		}
	}
	catalog, _ := data["models"].([]any)
	var ids []string
	for _, raw := range catalog {
		if id := PlainString(asRecord(raw)["id"]); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// QuotaWindow is one provider quota window.
type QuotaWindow struct {
	ID          string  `json:"id"`
	Label       string  `json:"label"`
	UsedPercent float64 `json:"usedPercent"`
	Note        string  `json:"note,omitempty"`
}

// Quota is the WorkBuddy credit summary.
type Quota struct {
	Windows []QuotaWindow `json:"windows"`
	Plan    string        `json:"plan,omitempty"`
}

// FetchQuota reads the billing meter's resource summary and folds the cycle
// packages into one "Credits" window.
func (c *Client) FetchQuota(ctx context.Context, creds Creds, endpoint string) (Quota, error) {
	root := strings.TrimRight(endpoint, "/")
	if root == "" {
		root = Endpoint
	}
	summary, err := c.call(ctx, http.MethodPost, root+"/billing/meter/get-user-resource-summary",
		AuthHeaders(creds), map[string]any{})
	if err != nil {
		return Quota{}, err
	}
	var total, used float64
	packages, _ := summary["Packages"].([]any)
	for _, raw := range packages {
		pkg := asRecord(raw)
		total += jsNumber(pkg["CycleTotalCapacity"])
		used += jsNumber(pkg["CycleUsedCapacity"])
	}
	var windows []QuotaWindow
	if total > 0 {
		pct := 100 * used / total
		pct = min(100, max(0, pct))
		windows = []QuotaWindow{{
			ID:          "credits",
			Label:       "Credits",
			UsedPercent: pct,
			Note:        compactNumber(used) + " / " + compactNumber(total),
		}}
	}
	plan := "Free"
	if summary["IsPaidUser"] == true {
		plan = "Pro"
	}
	return Quota{Windows: windows, Plan: plan}, nil
}

// jsNumber is `Number(v ?? 0) || 0`: numbers and numeric strings count,
// anything else is 0.
func jsNumber(v any) float64 {
	if n, ok := number(v); ok && !math.IsNaN(n) {
		return n
	}
	if s, ok := v.(string); ok {
		if n, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil && !math.IsNaN(n) {
			return n
		}
	}
	return 0
}

func compactNumber(value float64) string {
	if value >= 1_000_000 {
		return fmt.Sprintf("%.1fM", value/1_000_000)
	}
	if value >= 1_000 {
		return fmt.Sprintf("%.1fk", value/1_000)
	}
	rounded := int64(value*100 + 0.5)
	if rounded%100 == 0 {
		return fmt.Sprintf("%d", rounded/100)
	}
	// Math.round(v*100)/100 — strip a trailing zero like JSON numbers would.
	s := fmt.Sprintf("%.2f", float64(rounded)/100)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	return s
}
