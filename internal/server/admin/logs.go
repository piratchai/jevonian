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
func logFilter(r *http.Request) LogFilter {
	phase := r.URL.Query().Get("phase")
	if phase == "all" {
		phase = ""
	}
	model := strings.TrimSpace(r.URL.Query().Get("model"))
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	return LogFilter{Phase: phase, Model: model, Query: query}
}
func matcher(r *http.Request) func(LogRecord) bool {
	filter := logFilter(r)
	phase, model, query := filter.Phase, filter.Model, filter.Query
	return func(rec LogRecord) bool {
		if rec["kind"] == "brain" {
			return false
		}
		p := text(rec["phase"])
		if p == "" {
			p = "-"
		}
		if phase != "" && phase != p {
			return false
		}
		if model != "" && model != text(rec["model"]) {
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
	for i := range buckets {
		buckets[i] = map[string]any{"start": iso(start.Add(time.Duration(float64(i)*width) * time.Millisecond)), "requests": 0, "errors": 0, "costUsd": 0.0, "avgLatencyMs": 0}
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
		}
	}
	for i, b := range buckets {
		b["costUsd"] = round6(number(b["costUsd"]))
		if n := number(b["requests"]); n > 0 {
			b["avgLatencyMs"] = math.Round(latency[i] / n)
		}
	}
	send(w, 200, map[string]any{"minutes": minutes, "buckets": buckets})
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
