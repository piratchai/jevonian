package admin

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xinyao27/jevonian/internal/keys"
	"github.com/xinyao27/jevonian/internal/paths"
	"github.com/xinyao27/jevonian/internal/routing"
)

func (h *Handler) records(w http.ResponseWriter) ([]LogRecord, bool) {
	if h.deps.Ledger == nil {
		failure(w, 503, "Ledger is unavailable.")
		return nil, false
	}
	records, err := h.deps.Ledger.Records()
	if err != nil {
		failure(w, 500, err.Error())
		return nil, false
	}
	return records, true
}

// values normalizes a repeatable query parameter: trimmed, de-duplicated, and
// with empty entries dropped. dropAll also removes the phase sentinel "all".
// Order is preserved.
func values(r *http.Request, name string, dropAll bool) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, raw := range r.URL.Query()[name] {
		v := strings.TrimSpace(raw)
		if v == "" || (dropAll && v == "all") || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}
func logFilter(r *http.Request) LogFilter {
	return LogFilter{
		Phases:    values(r, "phase", true),
		Models:    values(r, "model", false),
		Providers: values(r, "provider", false),
		Status:    values(r, "status", false),
		Query:     strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q"))),
		Session:   strings.TrimSpace(r.URL.Query().Get("session")),
	}
}

// statuses maps the two accepted status values to a predicate. Unknown values
// are ignored; when nothing remains the status filter is inactive.
func statuses(list []string) (ok, err bool) {
	for _, s := range list {
		switch s {
		case "ok":
			ok = true
		case "error":
			err = true
		}
	}
	return ok, err
}

// statusMatch reports whether a record's status satisfies the ok/error filter.
// A missing status reads as 0, so it counts as ok, matching number().
func statusMatch(ok, wantErr bool, status float64) bool {
	switch {
	case ok && wantErr:
		return true
	case ok:
		return status < 400
	case wantErr:
		return status >= 400
	}
	return true
}

// matchFilter is the single in-memory source of truth for request filtering.
// A nil skip group leaves that group's filter unapplied, which faceting uses.
func matchFilter(f LogFilter, skip string) func(LogRecord) bool {
	phases, models, providers := f.Phases, f.Models, f.Providers
	ok, wantErr := statuses(f.Status)
	query, session := f.Query, f.Session
	switch skip {
	case "phase":
		phases = nil
	case "model":
		models = nil
	case "provider":
		providers = nil
	case "status":
		ok, wantErr = false, false
	}
	return func(rec LogRecord) bool {
		if rec["kind"] == "brain" {
			return false
		}
		p := text(rec["phase"])
		if p == "" {
			p = "-"
		}
		if len(phases) > 0 && !contains(phases, p) {
			return false
		}
		if len(models) > 0 && !contains(models, text(rec["model"])) {
			return false
		}
		if len(providers) > 0 && !contains(providers, text(rec["provider"])) {
			return false
		}
		if !statusMatch(ok, wantErr, number(rec["status"])) {
			return false
		}
		// Session is an exact, case-sensitive match on the stored id.
		if session != "" && session != text(rec["session"]) {
			return false
		}
		if query == "" {
			return true
		}
		parts := []string{}
		for _, k := range []string{"model", "provider", "phase", "session", "reason", "effort"} {
			parts = append(parts, text(rec[k]))
		}
		if number(rec["retries"]) != 0 {
			parts = append(parts, "retry")
		}
		if number(rec["failovers"]) != 0 {
			parts = append(parts, "failover")
		}
		for _, v := range slice(rec["tries"]) {
			cause := text(object(v)["cause"])
			if cause == "retry" || cause == "failover" {
				parts = append(parts, cause)
			}
		}
		return strings.Contains(strings.ToLower(strings.Join(parts, " ")), query)
	}
}
func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
func matcher(r *http.Request) func(LogRecord) bool { return matchFilter(logFilter(r), "") }
func intQuery(r *http.Request, name string, fallback, min, max int) int {
	n, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil || n == 0 {
		n = fallback
	}
	if n < min {
		n = min
	}
	if n > max {
		n = max
	}
	return n
}
func (h *Handler) logs(w http.ResponseWriter, r *http.Request) {
	if source, ok := h.deps.Ledger.(LogQuerier); ok {
		q := LogQuery{Filter: logFilter(r), Limit: intQuery(r, "limit", 200, 1, 1000)}
		if n, err := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64); err == nil {
			q.Before = &n
		}
		page, err := source.QueryLogs(r.Context(), q)
		if err != nil {
			failure(w, 500, err.Error())
			return
		}
		send(w, 200, map[string]any{"logs": page.Logs, "total": page.Total, "nextBefore": page.NextBefore})
		return
	}
	all, ok := h.records(w)
	if !ok {
		return
	}
	match := matcher(r)
	indices := []int{}
	for i, rec := range all {
		if match(rec) {
			indices = append(indices, i)
		}
	}
	upper := len(all)
	if n, err := strconv.Atoi(r.URL.Query().Get("before")); err == nil {
		upper = n
	}
	limit := intQuery(r, "limit", 200, 1, 1000)
	selected := []LogRecord{}
	oldest := -1
	for i := len(indices) - 1; i >= 0 && len(selected) < limit; i-- {
		index := indices[i]
		if index < upper {
			selected = append(selected, all[index])
			oldest = index
		}
	}
	var next any
	if oldest >= 0 && len(indices) > 0 && indices[0] < oldest {
		next = oldest
	}
	send(w, 200, map[string]any{"logs": selected, "total": len(indices), "nextBefore": next})
}
func (h *Handler) logSeries(w http.ResponseWriter, r *http.Request) {
	minutes := intQuery(r, "minutes", 60, 5, 1440)
	count := intQuery(r, "buckets", 30, 6, 120)
	now := h.deps.Now()
	start := now.Add(-time.Duration(minutes) * time.Minute)
	width := float64(minutes*60000) / float64(count)
	buckets := make([]map[string]any, count)
	latency := make([]float64, count)
	cacheRead := make([]float64, count)
	prompt := make([]float64, count)
	uncached := make([]float64, count)
	for i := range buckets {
		buckets[i] = map[string]any{"start": iso(start.Add(time.Duration(float64(i)*width) * time.Millisecond)), "requests": 0, "errors": 0, "costUsd": 0.0, "avgLatencyMs": 0, "cacheCoverage": nil}
	}
	if source, ok := h.deps.Ledger.(LogSeriesQuerier); ok {
		rows, err := source.QueryLogSeries(r.Context(), logFilter(r), start, now, count)
		if err != nil {
			failure(w, 500, err.Error())
			return
		}
		for i, row := range rows {
			buckets[i]["requests"] = row.Requests
			buckets[i]["errors"] = row.Errors
			buckets[i]["costUsd"] = row.CostUSD
			latency[i] = row.LatencyMS
			cacheRead[i] = float64(row.CacheReadTokens)
			prompt[i] = float64(row.PromptTokens)
			uncached[i] = float64(row.UncachedInputTokens)
		}
	} else {
		all, ok := h.records(w)
		if !ok {
			return
		}
		match := matcher(r)
		for _, rec := range all {
			if !match(rec) {
				continue
			}
			at, err := time.Parse(time.RFC3339Nano, text(rec["ts"]))
			if err != nil || at.Before(start) || at.After(now) {
				continue
			}
			slot := int(float64(at.Sub(start).Milliseconds()) / width)
			if slot >= count {
				slot = count - 1
			}
			b := buckets[slot]
			b["requests"] = int(number(b["requests"])) + 1
			if number(rec["status"]) >= 400 {
				b["errors"] = int(number(b["errors"])) + 1
			}
			b["costUsd"] = number(b["costUsd"]) + number(rec["costUsd"])
			latency[slot] += number(rec["latencyMs"])
			cacheRead[slot] += number(rec["cacheReadTokens"])
			prompt[slot] += number(rec["promptTokens"])
			if u, known := uncachedInputTokens(rec); known {
				uncached[slot] += u
			}
		}
	}
	for i, b := range buckets {
		b["costUsd"] = round6(number(b["costUsd"]))
		if n := number(b["requests"]); n > 0 {
			b["avgLatencyMs"] = math.Round(latency[i] / n)
		}
		// Coverage = cache reads over the full input. The denominator is cache
		// reads plus the uncached input, resolved per wire convention.
		if total := cacheRead[i] + uncached[i]; total > 0 {
			b["cacheCoverage"] = round6(cacheRead[i] / total)
		}
	}
	// Window totals let the chart header state the whole-window coverage, not
	// just the hovered bucket.
	totalRead, totalPrompt, totalUncached := 0.0, 0.0, 0.0
	for i := range buckets {
		totalRead += cacheRead[i]
		totalPrompt += prompt[i]
		totalUncached += uncached[i]
	}
	var windowCoverage any
	if total := totalRead + totalUncached; total > 0 {
		windowCoverage = round6(totalRead / total)
	}
	send(w, 200, map[string]any{
		"minutes":         minutes,
		"buckets":         buckets,
		"cacheCoverage":   windowCoverage,
		"cacheReadTokens": int64(totalRead),
		"promptTokens":    int64(totalPrompt),
	})
}

// facetsStart parses the optional minutes window. Only a valid 1..10080 value
// is honored; anything else counts the whole ledger.
func (h *Handler) facetsStart(r *http.Request) *time.Time {
	minutes, err := strconv.Atoi(r.URL.Query().Get("minutes"))
	if err != nil || minutes < 1 || minutes > 10080 {
		return nil
	}
	at := h.deps.Now().Add(-time.Duration(minutes) * time.Minute)
	return &at
}

// logFacets serves grouped value counts for the filter rail. Each group's
// counts apply every current filter except that group's own filter.
func (h *Handler) logFacets(w http.ResponseWriter, r *http.Request) {
	if h.deps.Ledger == nil {
		failure(w, 503, "Ledger is unavailable.")
		return
	}
	filter := logFilter(r)
	start := h.facetsStart(r)
	if source, ok := h.deps.Ledger.(LogFacetQuerier); ok {
		facets, err := source.QueryLogFacets(r.Context(), filter, start)
		if err != nil {
			failure(w, 500, err.Error())
			return
		}
		send(w, 200, facets)
		return
	}
	all, ok := h.records(w)
	if !ok {
		return
	}
	rows := []LogRecord{}
	for _, rec := range all {
		if start != nil {
			at, err := time.Parse(time.RFC3339Nano, text(rec["ts"]))
			if err != nil || at.Before(*start) {
				continue
			}
		}
		rows = append(rows, rec)
	}
	send(w, 200, collectFacets(rows, filter))
}

// collectFacets mirrors the SQLite facet queries over already-read records.
func collectFacets(rows []LogRecord, filter LogFilter) LogFacets {
	total := 0
	for _, rec := range rows {
		if matchFilter(filter, "")(rec) {
			total++
		}
	}
	groups := map[string][]LogFacetValue{}
	collectFacet(rows, filter, "status", func(rec LogRecord) []string {
		if number(rec["status"]) >= 400 {
			return []string{"error"}
		}
		return []string{"ok"}
	}, groups)
	collectFacet(rows, filter, "phase", func(rec LogRecord) []string {
		p := text(rec["phase"])
		if p == "" {
			p = "-"
		}
		return []string{p}
	}, groups)
	collectFacet(rows, filter, "provider", func(rec LogRecord) []string {
		if v := text(rec["provider"]); v != "" {
			return []string{v}
		}
		return nil
	}, groups)
	collectFacet(rows, filter, "model", func(rec LogRecord) []string {
		if v := text(rec["model"]); v != "" {
			return []string{v}
		}
		return nil
	}, groups)
	// status always lists both values, even at zero count.
	counts := map[string]int64{}
	for _, entry := range groups["status"] {
		counts[entry.Value] = entry.Count
	}
	groups["status"] = []LogFacetValue{{Value: "ok", Count: counts["ok"]}, {Value: "error", Count: counts["error"]}}
	return LogFacets{Total: int64(total), Groups: groups}
}

// collectFacet counts one group under all filters except the group's own.
func collectFacet(rows []LogRecord, filter LogFilter, group string, key func(LogRecord) []string, groups map[string][]LogFacetValue) {
	match := matchFilter(filter, group)
	counts := map[string]int64{}
	for _, rec := range rows {
		if !match(rec) {
			continue
		}
		for _, v := range key(rec) {
			counts[v]++
		}
	}
	entries := []LogFacetValue{}
	for value, count := range counts {
		if count > 0 {
			entries = append(entries, LogFacetValue{Value: value, Count: count})
		}
	}
	sortFacets(entries)
	if group == "model" && len(entries) > 50 {
		entries = entries[:50]
	}
	groups[group] = entries
}

const logStreamQueueLimit = 256

func (h *Handler) logStream(w http.ResponseWriter, r *http.Request) {
	if h.deps.Ledger == nil {
		failure(w, 503, "Ledger is unavailable.")
		return
	}
	if _, ok := w.(http.Flusher); !ok {
		failure(w, 500, "Streaming is unavailable.")
		return
	}
	match := matcher(r)
	wake := make(chan struct{}, 1)
	notify := func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
	var mu sync.Mutex
	queue := make([]LogRecord, 0, logStreamQueueLimit)
	overflow := false
	tail, incremental := h.deps.Ledger.(LogTail)
	subscriber, live := h.deps.Ledger.(LogSubscriber)
	live = live && !incremental
	if live {
		unsubscribe := subscriber.Subscribe(func(rec LogRecord) {
			if !match(rec) {
				return
			}
			mu.Lock()
			if !overflow {
				if len(queue) == logStreamQueueLimit {
					overflow = true
					queue = nil
				} else {
					queue = append(queue, rec)
				}
			}
			mu.Unlock()
			notify()
		})
		defer unsubscribe()
	}
	cursor := int64(0)
	if incremental {
		var err error
		cursor, err = tail.TailCursor(r.Context())
		if err != nil {
			failure(w, 500, err.Error())
			return
		}
	} else if !live {
		all, err := h.deps.Ledger.Records()
		if err != nil {
			failure(w, 500, err.Error())
			return
		}
		cursor = int64(len(all))
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(w)
	defer controller.SetWriteDeadline(time.Time{})
	write := func(event string, v any) bool {
		if r.Context().Err() != nil {
			return false
		}
		data, err := json.Marshal(v)
		if err != nil {
			return false
		}
		// A slow socket must not retain a reader or subscription indefinitely.
		_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data); err != nil {
			return false
		}
		return controller.Flush() == nil
	}
	resync := func(reason string) bool {
		if !write("resync", map[string]any{"reason": reason}) {
			return false
		}
		// Existing TS clients already backfill on ready; they need no new event
		// listener to recover. Closing then forces EventSource to reconnect.
		return write("ready", map[string]any{"ok": true, "resync": true})
	}
	if !write("ready", map[string]any{"ok": true}) {
		return
	}
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	poll := time.NewTicker(250 * time.Millisecond)
	defer poll.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-wake:
		case <-heartbeat.C:
			if !write("ping", map[string]any{}) {
				return
			}
		case <-poll.C:
		}
		if incremental {
			batch, err := tail.ReadAfter(r.Context(), cursor, logStreamQueueLimit)
			if err != nil {
				write("error", map[string]any{"error": err.Error()})
				return
			}
			if batch.Reset {
				resync("ledger-reset")
				return
			}
			for _, rec := range batch.Logs {
				if match(rec) && !write("log", rec) {
					return
				}
			}
			cursor = batch.Cursor
			if len(batch.Logs) == logStreamQueueLimit {
				notify()
			}
			continue
		}
		if !live {
			// Compatibility-only sources retain their original full-read path.
			all, err := h.deps.Ledger.Records()
			if err != nil {
				write("error", map[string]any{"error": err.Error()})
				return
			}
			if cursor > int64(len(all)) {
				resync("ledger-reset")
				return
			}
			for _, rec := range all[cursor:] {
				if match(rec) && !write("log", rec) {
					return
				}
			}
			cursor = int64(len(all))
			continue
		}
		mu.Lock()
		pending, lost := queue, overflow
		queue = make([]LogRecord, 0, logStreamQueueLimit)
		mu.Unlock()
		if lost {
			resync("overflow")
			return
		}
		for _, rec := range pending {
			if !write("log", rec) {
				return
			}
		}
	}
}

var bodyID = regexp.MustCompile(`^[0-9a-fA-F-]{8,64}$`)

func (h *Handler) loadBody(id string) any {
	if h.deps.LoadBody != nil {
		return h.deps.LoadBody(id)
	}
	if !bodyID.MatchString(id) {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(paths.DataDir(), "bodies", id+".json"))
	if err != nil {
		return nil
	}
	var v any
	if json.Unmarshal(data, &v) != nil {
		return nil
	}
	return v
}
func (h *Handler) logDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var record LogRecord
	var brains []LogRecord
	if source, ok := h.deps.Ledger.(LogQuerier); ok {
		var err error
		record, brains, err = source.LogDetail(r.Context(), id)
		if err != nil {
			failure(w, 500, err.Error())
			return
		}
	} else {
		all, ok := h.records(w)
		if !ok {
			return
		}
		for _, rec := range all {
			if record == nil && text(rec["id"]) == id {
				record = rec
			}
			if rec["kind"] == "brain" && rec["requestId"] == id {
				brains = append(brains, rec)
			}
		}
	}
	if record == nil {
		failure(w, 404, fmt.Sprintf("record %q not found", id))
		return
	}
	calls := []any{}
	for _, rec := range brains {
		if rec["kind"] == "brain" && rec["requestId"] == id {
			row := map[string]any{"record": rec}
			if b := h.loadBody(text(rec["id"])); b != nil {
				row["body"] = b
			}
			calls = append(calls, row)
		}
	}
	out := map[string]any{"record": record, "brainCalls": calls}
	if b := h.loadBody(id); b != nil {
		out["body"] = b
	}
	send(w, 200, out)
}
func limitUSD(v any) *float64 {
	n := number(v)
	if s, ok := v.(string); ok {
		n, _ = strconv.ParseFloat(strings.TrimSpace(s), 64)
	}
	if n > 0 && !math.IsInf(n, 0) && !math.IsNaN(n) {
		return &n
	}
	return nil
}
func (h *Handler) keyAPI(w http.ResponseWriter, r *http.Request) {
	if h.deps.Keys == nil {
		failure(w, 503, "Key store is unavailable.")
		return
	}
	store := h.deps.Keys
	b := body(r)
	switch r.Method {
	case "POST":
		name := "default"
		if s, ok := b["name"].(string); ok {
			name = s
		}
		result, err := store.Create(keys.CreateOptions{Name: name, LimitUSD: limitUSD(b["limitUsd"])})
		if err != nil {
			failure(w, 500, err.Error())
			return
		}
		send(w, 201, map[string]any{"key": result.Key, "record": result.Record})
		return
	case "PUT":
		p := KeyPatch{}
		if s, ok := b["name"].(string); ok {
			s = strings.TrimSpace(s)
			p.Name = &s
		}
		if v, ok := b["limitUsd"]; ok {
			p.HasLimit = true
			p.LimitUSD = limitUSD(v)
		}
		updated, found, err := store.Update(r.PathValue("id"), p)
		if err != nil {
			failure(w, 500, err.Error())
			return
		}
		if !found {
			failure(w, 404, "key not found")
			return
		}
		all, err := store.ListWithUsage()
		if err != nil {
			failure(w, 500, err.Error())
			return
		}
		send(w, 200, map[string]any{"key": updated, "keys": all})
		return
	case "DELETE":
		found, err := store.Revoke(r.PathValue("id"))
		if err != nil {
			failure(w, 500, err.Error())
			return
		}
		if !found {
			failure(w, 404, "key not found")
			return
		}
	}
	all, err := store.ListWithUsage()
	if err != nil {
		failure(w, 500, err.Error())
		return
	}
	send(w, 200, map[string]any{"keys": all})
}
func canonical(model string) string {
	key := routing.CanonicalModelID(strings.TrimSpace(model))
	if key == "" {
		key = "unknown"
	}
	return key
}
func iso(t time.Time) string   { return t.UTC().Format("2006-01-02T15:04:05.000Z") }
func round6(n float64) float64 { return math.Round(n*1e6) / 1e6 }
