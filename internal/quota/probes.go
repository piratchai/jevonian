package quota

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/oauth"
	"github.com/xinyao27/jevonian/internal/provider/devin"
	"github.com/xinyao27/jevonian/internal/provider/workbuddy"
)

type liveQuota struct {
	Windows    []Window
	Balance    *Balance
	Resets     *ResetCredits
	Plan, Note string
}

func probeKind(p config.Provider) string {
	u, _ := url.Parse(p.BaseURL)
	host := ""
	path := ""
	if u != nil {
		host = strings.ToLower(u.Hostname())
		path = u.Path
	}
	matches := func(suffix string) bool { return host == suffix || strings.HasSuffix(host, "."+suffix) }
	switch {
	case matches("opencode.ai") && strings.Contains(path, "/zen/go"):
		return "opencode"
	case matches("commandcode.ai"):
		return "commandcode"
	case p.Type == config.ProviderTypeGemini || (p.Auth == config.AuthOAuth && p.OAuthSource == config.OAuthAntigravity):
		return "antigravity"
	case p.Type == config.ProviderTypeCursor || (p.Auth == config.AuthOAuth && p.OAuthSource == config.OAuthCursor):
		return "cursor"
	case p.Type == config.ProviderTypeDevin || (p.Auth == config.AuthOAuth && p.OAuthSource == config.OAuthDevin):
		return "devin"
	case matches("deepseek.com"):
		return "deepseek"
	case matches("openrouter.ai"):
		return "openrouter"
	case matches("moonshot.ai") || matches("moonshot.cn"):
		return "moonshot"
	case p.Auth == config.AuthOAuth:
		switch p.OAuthSource {
		case config.OAuthClaudeCode:
			return "claude"
		case config.OAuthCodex:
			return "codex"
		case config.OAuthCursor:
			return "cursor"
		case config.OAuthWorkbuddyAI:
			return "workbuddy"
		}
	}
	return ""
}
func (s *Service) fetchLive(ctx context.Context, p config.Provider) (liveQuota, error) {
	kind := probeKind(p)
	if kind == "" {
		return liveQuota{}, nil
	}
	auth, err := s.resolveAuth(ctx, p)
	if err != nil {
		return liveQuota{}, err
	}
	base := strings.TrimRight(p.BaseURL, "/")
	if kind == "cursor" {
		if auth.Token == "" {
			return liveQuota{}, fmt.Errorf("Missing Cursor token for provider %q", p.Name)
		}
		return s.cursorUsage(ctx, auth.Token)
	}
	if kind == "devin" {
		if auth.Token == "" {
			return liveQuota{}, fmt.Errorf("Missing Devin token for provider %q", p.Name)
		}
		status, err := devin.FetchUserStatus(ctx, s.opts.HTTP, auth.Token, base)
		if err != nil {
			return liveQuota{}, err
		}
		out := liveQuota{Plan: status.Plan}
		for _, w := range status.Windows() {
			reset := ""
			if !w.ResetsAt.IsZero() {
				reset = w.ResetsAt.UTC().Format(isoMillis)
			}
			out.Windows = append(out.Windows, Window{ID: w.ID, Label: w.Label, UsedPercent: w.UsedPercent, ResetsAt: reset})
		}
		return out, nil
	}
	if kind == "workbuddy" {
		// ResolveProviderAuth uses the shared OAuth resolver's injected session refresh.
		token := auth.Token
		if token == "" {
			token = strings.TrimPrefix(auth.Headers["authorization"], "Bearer ")
		}
		creds := workbuddy.Creds{AccessToken: token, UID: auth.Headers["x-user-id"], Domain: auth.Headers["x-domain"]}
		client := &workbuddy.Client{HTTP: s.opts.HTTP}
		q, err := client.FetchQuota(ctx, creds, workbuddy.EndpointFromBaseURL(base))
		if err != nil {
			return liveQuota{}, err
		}
		out := liveQuota{Plan: q.Plan}
		for i, w := range q.Windows {
			out.Windows = append(out.Windows, Window{ID: w.ID, Label: w.Label, UsedPercent: w.UsedPercent})
			if i == 0 {
				out.Note = w.Note
			}
		}
		return out, nil
	}
	origin, err := originOf(base)
	if err != nil {
		return liveQuota{}, err
	}
	target, method := base, http.MethodGet
	var body any
	switch kind {
	case "claude":
		target = origin + "/api/oauth/usage"
	case "codex":
		target = s.opts.CodexUsageURL
		if target == "" {
			target = os.Getenv("JEVONIAN_CODEX_USAGE_URL")
		}
		if target == "" {
			if strings.Contains(base, "chatgpt.com/backend-api/codex") {
				target = "https://chatgpt.com/backend-api/wham/usage"
			} else {
				target = strings.TrimRight(base, "/") + "/wham/usage"
			}
		}
	case "opencode":
		target = base + "/usage"
	case "commandcode":
		return s.commandCode(ctx, auth.Headers)
	case "antigravity":
		target = strings.TrimSuffix(base, "/v1internal") + "/v1internal:fetchAvailableModels"
		method = http.MethodPost
		project := auth.Project
		if project == "" {
			project = "default-cli-project"
		}
		body = map[string]string{"project": project}
	case "deepseek":
		target = origin + "/user/balance"
	case "openrouter":
		target = base + "/credits"
	case "moonshot":
		target = base + "/users/me/balance"
	}
	j, status, err := s.requestJSON(ctx, method, target, auth.Headers, body)
	if err != nil {
		return liveQuota{}, err
	}
	if status < 200 || status >= 300 {
		return liveQuota{}, probeHTTPError(kind, status)
	}
	switch kind {
	case "claude":
		out := liveQuota{}
		for _, pair := range [][2]string{{"five_hour", "5h"}, {"seven_day", "7d"}} {
			w := record(j[pair[0]])
			if pct, ok := finiteNumber(w["utilization"]); ok {
				out.Windows = append(out.Windows, Window{ID: pair[1], Label: pair[1], UsedPercent: clampPercent(pct), ResetsAt: toISO(w["resets_at"])})
			}
		}
		limits, _ := j["limits"].([]any)
		for _, raw := range limits {
			limit := record(raw)
			model := record(record(limit["scope"])["model"])
			label := strings.TrimSpace(text(model["display_name"]))
			pct, ok := finiteNumber(limit["percent"])
			if label == "" || !ok {
				continue
			}
			id := text(model["id"])
			if id == "" {
				id = strings.ToLower(label)
			}
			out.Windows = append(out.Windows, Window{ID: "scoped-" + id, Label: label, Model: label, UsedPercent: clampPercent(pct), ResetsAt: toISO(limit["resets_at"])})
		}
		return out, nil
	case "codex":
		out := liveQuota{Plan: text(j["plan_type"])}
		resets := record(j["rate_limit_reset_credits"])
		if count, ok := finiteNumber(resets["available_count"]); ok && count > 0 {
			resetAuth := auth
			resetAuth.Headers = make(map[string]string, len(auth.Headers)+1)
			for key, value := range auth.Headers {
				resetAuth.Headers[key] = value
			}
			if id := text(record(j["account"])["account_id"]); id != "" && resetAuth.Headers["chatgpt-account-id"] == "" {
				resetAuth.Headers["chatgpt-account-id"] = id
			}
			out.Resets = s.codexResetCredits(ctx, strings.TrimSuffix(base, "/codex"), resetAuth, int(count))
		}
		rate := record(j["rate_limit"])
		for _, pair := range [][2]string{{"primary_window", "codex-primary"}, {"secondary_window", "codex-secondary"}} {
			if w := codexLiveWindow(pair[1], rate[pair[0]]); w != nil {
				out.Windows = append(out.Windows, *w)
			}
		}
		credits := record(j["credits"])
		if b := text(credits["balance"]); b != "" {
			out.Note = "credits " + b
		} else if credits["unlimited"] == true {
			out.Note = "unlimited credits"
		}
		return out, nil
	case "opencode":
		out := liveQuota{}
		usage := record(j["usage"])
		for _, pair := range [][2]string{{"rolling", "5h"}, {"weekly", "week"}, {"monthly", "month"}} {
			w := record(usage[pair[0]])
			if pct, ok := finiteNumber(w["percent"]); ok {
				out.Windows = append(out.Windows, Window{ID: pair[1], Label: pair[1], UsedPercent: clampPercent(pct), ResetsAt: toISO(w["resetsAt"]), Status: text(w["status"])})
			}
		}
		return out, nil
	case "antigravity":
		return antigravityLive(record(j["models"])), nil
	case "deepseek":
		infos, _ := j["balance_infos"].([]any)
		if len(infos) == 0 {
			return liveQuota{}, nil
		}
		info := record(infos[0])
		amount, err := strconv.ParseFloat(text(info["total_balance"]), 64)
		if err != nil || math.IsNaN(amount) || math.IsInf(amount, 0) {
			return liveQuota{}, nil
		}
		currency := text(info["currency"])
		if currency == "" {
			currency = "USD"
		}
		return liveQuota{Balance: &Balance{Amount: round6(amount), Currency: currency}}, nil
	case "openrouter":
		data := record(j["data"])
		total, ok := finiteNumber(data["total_credits"])
		used, ok2 := finiteNumber(data["total_usage"])
		if !ok || !ok2 {
			return liveQuota{}, nil
		}
		return liveQuota{Balance: &Balance{Amount: round6(math.Max(0, total-used)), Currency: "USD"}}, nil
	case "moonshot":
		data := record(j["data"])
		raw := data["available_balance"]
		if raw == nil {
			raw = data["balance"]
		}
		amount, ok := finiteNumber(raw)
		if !ok {
			return liveQuota{}, nil
		}
		currency := "USD"
		if u, parseErr := url.Parse(p.BaseURL); parseErr == nil && strings.HasSuffix(strings.ToLower(u.Hostname()), "moonshot.cn") {
			currency = "CNY"
		}
		return liveQuota{Balance: &Balance{Amount: round6(amount), Currency: currency}}, nil
	}
	return liveQuota{}, nil
}

func (s *Service) codexResetCredits(ctx context.Context, base string, auth oauth.AuthResolution, count int) *ResetCredits {
	out := &ResetCredits{Count: count}
	headers := map[string]string{}
	for key, value := range auth.Headers {
		headers[key] = value
	}
	if accountID := auth.Headers["chatgpt-account-id"]; accountID != "" {
		headers["chatgpt-account-id"] = accountID
	}
	j, status, err := s.requestJSON(ctx, http.MethodGet, base+"/wham/rate-limit-reset-credits", headers, nil)
	if err != nil || status < 200 || status >= 300 {
		return out
	}
	credits, _ := j["credits"].([]any)
	for _, raw := range credits {
		credit := record(raw)
		if text(credit["status"]) != "available" || text(credit["id"]) == "" {
			continue
		}
		reset := toISO(credit["expires_at"])
		item := ResetCredit{ExpiresAt: reset}
		out.Each = append(out.Each, item)
		if reset != "" && (out.Until == "" || reset < out.Until) {
			out.Until = reset
		}
	}
	if len(out.Each) != count {
		out.Each = nil
	}
	return out
}

func (s *Service) cursorUsage(ctx context.Context, token string) (liveQuota, error) {
	const target = "https://api2.cursor.sh/aiserver.v1.DashboardService/GetCurrentPeriodUsage"
	j, status, err := s.requestJSON(ctx, http.MethodPost, target, map[string]string{
		"authorization":            "Bearer " + token,
		"connect-protocol-version": "1",
	}, map[string]any{})
	if err != nil {
		return liveQuota{}, err
	}
	if status < 200 || status >= 300 {
		return liveQuota{}, fmt.Errorf("Cursor usage request failed (%d)", status)
	}
	usage := record(j["planUsage"])
	if len(usage) == 0 {
		return liveQuota{}, nil
	}
	reset := ""
	if value := text(j["billingCycleEnd"]); value != "" {
		if ms, parseErr := strconv.ParseInt(value, 10, 64); parseErr == nil && ms > 0 {
			reset = time.UnixMilli(ms).UTC().Format(isoMillis)
		}
	}
	out := liveQuota{}
	for _, item := range []struct{ key, label string }{{"autoPercentUsed", "Cursor Models"}, {"apiPercentUsed", "Other Models"}, {"totalPercentUsed", "Total"}} {
		if pct, ok := finiteNumber(usage[item.key]); ok {
			id := strings.ToLower(strings.ReplaceAll(item.label, " ", "-"))
			out.Windows = append(out.Windows, Window{ID: "cursor-" + id, Label: item.label, UsedPercent: clampPercent(pct), ResetsAt: reset})
		}
	}
	return out, nil
}

func (s *Service) requestJSON(ctx context.Context, method, target string, headers map[string]string, body any) (map[string]any, int, error) {
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("accept", "application/json")
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	client := s.opts.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()
	// Never include upstream bodies in error strings: they can echo credentials.
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, response.StatusCode, nil
	}
	var parsed map[string]any
	dec := json.NewDecoder(io.LimitReader(response.Body, 2<<20))
	if err = dec.Decode(&parsed); err != nil {
		return nil, response.StatusCode, fmt.Errorf("quota endpoint returned invalid JSON: %w", err)
	}
	return parsed, response.StatusCode, nil
}
func originOf(base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil || u == nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("quota: invalid provider base URL")
	}
	return u.Scheme + "://" + u.Host, nil
}
func probeHTTPError(kind string, status int) error {
	names := map[string]string{"claude": "Claude", "codex": "Codex", "opencode": "OpenCode Go", "commandcode": "Command Code", "antigravity": "Antigravity", "deepseek": "DeepSeek", "openrouter": "OpenRouter", "moonshot": "Moonshot"}
	name := names[kind]
	if kind == "claude" && status == 429 {
		return fmt.Errorf("Claude usage endpoint is rate limited; using the last snapshot.")
	}
	if kind == "opencode" && status == 403 {
		return fmt.Errorf("No OpenCode Go subscription on this key.")
	}
	if status == 401 && kind != "claude" && kind != "codex" {
		credential := "key"
		if kind == "antigravity" {
			credential = "token"
		}
		return fmt.Errorf("%s rejected the %s.", name, credential)
	}
	operation := "usage"
	if kind == "deepseek" || kind == "moonshot" {
		operation = "balance"
	}
	if kind == "openrouter" {
		operation = "credits"
	}
	return fmt.Errorf("%s %s request failed (%d)", name, operation, status)
}
func record(v any) map[string]any {
	m, _ := v.(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}
func text(v any) string { s, _ := v.(string); return s }
func finiteNumber(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok && !math.IsNaN(f) && !math.IsInf(f, 0)
}
func toISO(v any) string {
	if n, ok := finiteNumber(v); ok {
		return epochISO(n)
	}
	if s, ok := v.(string); ok {
		if at, valid := parseISO(s); valid {
			return at.Format(isoMillis)
		}
	}
	return ""
}
func codexLiveWindow(id string, raw any) *Window {
	w := record(raw)
	pct, ok := finiteNumber(w["used_percent"])
	if !ok {
		return nil
	}
	seconds, ok := finiteNumber(w["limit_window_seconds"])
	if !ok {
		minutes, _ := finiteNumber(w["window_minutes"])
		seconds = minutes * 60
	}
	reset := w["reset_at"]
	if reset == nil {
		reset = w["resets_at"]
	}
	return &Window{ID: id, Label: windowMinutesLabel(seconds / 60), UsedPercent: clampPercent(pct), ResetsAt: toISO(reset), Status: text(w["status"])}
}
func moneyWindow(id string, raw any) *Window {
	w := record(raw)
	used, ok := finiteNumber(w["used"])
	cap, ok2 := finiteNumber(w["cap"])
	if !ok || !ok2 || cap <= 0 {
		return nil
	}
	used = round6(used)
	out := &Window{ID: id, Label: id, UsedUSD: &used, LimitUSD: &cap, UsedPercent: clampPercent(100 * used / cap)}
	if reset, ok := finiteNumber(w["resetAt"]); ok && reset > 0 {
		out.ResetsAt = toISO(reset)
	}
	if w["exceeded"] == true {
		out.Status = "exceeded"
	}
	return out
}
func (s *Service) commandCode(ctx context.Context, headers map[string]string) (liveQuota, error) {
	base := s.opts.CommandCodeBaseURL
	if base == "" {
		base = os.Getenv("JEVONIAN_COMMANDCODE_BASE_URL")
	}
	if base == "" {
		base = "https://api.commandcode.ai"
	}
	base = strings.TrimRight(base, "/")
	j, status, err := s.requestJSON(ctx, http.MethodGet, base+"/alpha/billing/credits", headers, nil)
	if err != nil {
		return liveQuota{}, err
	}
	if status < 200 || status >= 300 {
		return liveQuota{}, probeHTTPError("commandcode", status)
	}
	out := liveQuota{}
	limits := record(j["windowLimits"])
	for _, pair := range [][2]string{{"fiveHour", "5h"}, {"weekly", "week"}} {
		if w := moneyWindow(pair[1], limits[pair[0]]); w != nil {
			out.Windows = append(out.Windows, *w)
		}
	}
	// Optional enrichments share the provider deadline and run sequentially to
	// retain the service-wide network concurrency ceiling.
	summary, _, _ := s.requestJSON(ctx, http.MethodGet, base+"/alpha/usage/summary", headers, nil)
	subscription, _, _ := s.requestJSON(ctx, http.MethodGet, base+"/alpha/billing/subscriptions", headers, nil)
	remaining, haveRemaining := finiteNumber(record(j["credits"])["monthlyCredits"])
	used, haveUsed := finiteNumber(summary["totalMonthlyCredits"])
	if haveRemaining && haveUsed && used+remaining > 0 {
		cap := round6(used + remaining)
		used = round6(used)
		out.Windows = append(out.Windows, Window{ID: "month", Label: "month", UsedUSD: &used, LimitUSD: &cap, UsedPercent: clampPercent(100 * used / cap), ResetsAt: toISO(record(subscription["data"])["currentPeriodEnd"])})
	}
	if plan := text(record(subscription["data"])["planId"]); plan != "" {
		out.Plan = strings.ToUpper(strings.TrimPrefix(plan, "individual-"))
	}
	if haveRemaining {
		out.Note = fmt.Sprintf("$%.2f credits left", remaining)
	}
	return out, nil
}
func antigravityLive(models map[string]any) liveQuota {
	type counter struct {
		remaining float64
		reset     string
	}
	counters := map[string]counter{}
	keys := make([]string, 0, len(models))
	for key := range models {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	unknown := 0
	for _, key := range keys {
		model := record(models[key])
		quota := record(model["quotaInfo"])
		fraction, ok := finiteNumber(quota["remainingFraction"])
		if !ok {
			continue
		}
		provider := text(model["modelProvider"])
		label := ""
		switch provider {
		case "MODEL_PROVIDER_GOOGLE":
			label = "gemini"
		case "MODEL_PROVIDER_ANTHROPIC":
			label = "claude"
		case "MODEL_PROVIDER_OPENAI":
			label = "openai"
		default:
			if strings.HasPrefix(provider, "MODEL_PROVIDER_") {
				label = strings.ToLower(strings.TrimPrefix(provider, "MODEL_PROVIDER_"))
			} else {
				label = fmt.Sprintf("counter-%d", unknown)
			}
		}
		unknown++
		reset := toISO(quota["resetTime"])
		existing, exists := counters[label]
		if !exists || fraction < existing.remaining {
			if reset == "" {
				reset = existing.reset
			}
			counters[label] = counter{fraction, reset}
		} else if existing.reset == "" && reset != "" {
			existing.reset = reset
			counters[label] = existing
		}
	}
	labels := make([]string, 0, len(counters))
	for label := range counters {
		labels = append(labels, label)
	}
	rank := func(s string) int {
		switch s {
		case "gemini":
			return 0
		case "claude":
			return 1
		case "openai":
			return 2
		}
		return 3
	}
	sort.Slice(labels, func(i, j int) bool {
		if rank(labels[i]) != rank(labels[j]) {
			return rank(labels[i]) < rank(labels[j])
		}
		return labels[i] < labels[j]
	})
	out := liveQuota{}
	for _, label := range labels {
		c := counters[label]
		out.Windows = append(out.Windows, Window{ID: "antigravity-" + label, Label: label, UsedPercent: clampPercent((1 - c.remaining) * 100), ResetsAt: c.reset})
	}
	if len(out.Windows) > 1 {
		out.Note = fmt.Sprintf("%d quota counters", len(out.Windows))
	}
	return out
}
