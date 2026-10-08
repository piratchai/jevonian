package admin

import (
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/xinyao27/jevonian/internal/catalogsync"
	"github.com/xinyao27/jevonian/internal/routing"
)

// Stats includes brain calls in totals and grouping, but excludes them from
// API/subscription savings. Activity excludes them everywhere (TS semantics).
func (h *Handler) scanRecords(w http.ResponseWriter, r *http.Request, query LogRange, visit func(LogRecord) error) bool {
	if source, ok := h.deps.Ledger.(LogRanger); ok {
		if err := source.ScanLogRange(r.Context(), query, visit); err != nil {
			failure(w, 500, err.Error())
			return false
		}
		return true
	}
	records, ok := h.records(w)
	if !ok {
		return false
	}
	for _, rec := range records {
		if err := visit(rec); err != nil {
			failure(w, 500, err.Error())
			return false
		}
	}
	return true
}
func (h *Handler) stats(w http.ResponseWriter, r *http.Request) {
	c := h.current()
	baseline := c.Routing.BaselineModel
	if baseline == "" {
		ts := tiers(deriveRoutings(c, h.deps.Prices))
		if len(ts.Plan) > 0 {
			baseline = ts.Plan[0]
		}
	}
	if baseline == "" && h.deps.Prices != nil {
		best := -1.0
		if !h.scanRecords(w, r, LogRange{}, func(rec LogRecord) error {
			model := text(rec["model"])
			if p := h.deps.Prices(model, ""); p != nil && p.Output > best {
				best = p.Output
				baseline = model
			}
			return nil
		}) {
			return
		}
	}
	provider := ""
	for _, p := range c.Providers {
		for _, m := range p.Models {
			if m.ID == baseline {
				provider = p.Name
				break
			}
		}
		if provider != "" {
			break
		}
	}
	out := map[string]any{}
	for _, k := range []string{"requests", "sessions", "costUsd", "apiUsd", "subscriptionUsd", "subscriptionRequests", "brainUsd", "brainRequests", "baselineUsd", "apiBaselineUsd", "subscriptionBaselineUsd", "savingsUsd", "savingsPct", "cacheHitRate", "savedTokens", "unpriced"} {
		out[k] = 0.0
	}
	out["requests"] = 0
	if baseline != "" {
		out["baselineModel"] = baseline
	}
	models := map[string]map[string]any{}
	phases := map[string]map[string]any{}
	sessions := map[string]bool{}
	cache := 0.0
	uncached := 0.0
	add := func(m map[string]any, k string, n float64) { m[k] = number(m[k]) + n }
	if !h.scanRecords(w, r, LogRange{}, func(rec LogRecord) error {
		add(out, "requests", 1)
		model := canonical(text(rec["model"]))
		row := models[model]
		if row == nil {
			row = map[string]any{"model": model, "label": model, "requests": 0, "promptTokens": 0, "completionTokens": 0, "cacheReadTokens": 0, "costUsd": 0.0, "unpriced": 0}
			models[model] = row
		}
		add(row, "requests", 1)
		for _, k := range []string{"promptTokens", "completionTokens", "cacheReadTokens"} {
			add(row, k, number(rec[k]))
		}
		cost := number(rec["costUsd"])
		if rec["costUsd"] == nil {
			add(row, "unpriced", 1)
			add(out, "unpriced", 1)
		} else {
			add(row, "costUsd", cost)
			add(out, "costUsd", cost)
		}
		if rec["kind"] == "brain" {
			add(out, "brainRequests", 1)
			add(out, "brainUsd", cost)
		} else if rec["billing"] == "subscription" {
			add(out, "subscriptionRequests", 1)
			add(out, "subscriptionUsd", cost)
		} else {
			add(out, "apiUsd", cost)
		}
		phase := text(rec["phase"])
		if phase == "" {
			phase = "-"
		}
		pr := phases[phase]
		if pr == nil {
			pr = map[string]any{"phase": phase, "requests": 0, "costUsd": 0.0}
			phases[phase] = pr
		}
		add(pr, "requests", 1)
		add(pr, "costUsd", cost)
		sessions[text(rec["session"])] = true
		cache += number(rec["cacheReadTokens"])
		if u, known := uncachedInputTokens(rec); known {
			uncached += u
		}
		add(out, "savedTokens", number(rec["savedTokens"]))
		if baseline != "" && rec["kind"] != "brain" && h.deps.Prices != nil {
			at, _ := time.Parse(time.RFC3339Nano, text(rec["ts"]))
			estimate, known := routing.CostOf(h.deps.Prices(baseline, provider), routing.Usage{Input: int(number(rec["promptTokens"])), Output: int(number(rec["completionTokens"])), CacheRead: int(number(rec["cacheReadTokens"])), CacheWrite: int(number(rec["cacheWriteTokens"]))}, at)
			if known {
				add(out, "baselineUsd", estimate)
				if rec["billing"] == "subscription" {
					add(out, "subscriptionBaselineUsd", estimate)
				} else {
					add(out, "apiBaselineUsd", estimate)
				}
			}
		}
		return nil
	}) {
		return
	}
	out["sessions"] = len(sessions)
	savings := number(out["apiBaselineUsd"]) - number(out["apiUsd"])
	out["savingsUsd"] = savings
	if base := number(out["apiBaselineUsd"]); base > 0 {
		out["savingsPct"] = savings / base * 100
	}
	if cache+uncached > 0 {
		out["cacheHitRate"] = cache / (cache + uncached)
	}
	byModel := []map[string]any{}
	for _, m := range models {
		byModel = append(byModel, m)
	}
	sort.SliceStable(byModel, func(i, j int) bool { return number(byModel[i]["costUsd"]) > number(byModel[j]["costUsd"]) })
	byPhase := []map[string]any{}
	for _, p := range phases {
		byPhase = append(byPhase, p)
	}
	sort.SliceStable(byPhase, func(i, j int) bool { return number(byPhase[i]["costUsd"]) > number(byPhase[j]["costUsd"]) })
	out["byModel"] = byModel
	out["byPhase"] = byPhase
	send(w, 200, out)
}
func midnight(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}
func (h *Handler) activity(w http.ResponseWriter, r *http.Request) {
	rangeID := r.URL.Query().Get("range")
	if rangeID != "today" && rangeID != "24h" && rangeID != "7d" && rangeID != "30d" && rangeID != "all" {
		rangeID = "30d"
	}
	keyID := strings.TrimSpace(r.URL.Query().Get("keyId"))
	if keyID == "" {
		keyID = "all"
	}
	now := h.deps.Now()
	start := midnight(now.Add(-30 * 24 * time.Hour))
	width := 24 * time.Hour
	switch rangeID {
	case "today":
		start = midnight(now)
		width = time.Hour
	case "24h":
		start = now.Add(-24 * time.Hour)
		width = time.Hour
	case "7d":
		start = midnight(now.Add(-7 * 24 * time.Hour))
	case "all":
		start = midnight(now.Add(-90 * 24 * time.Hour))
		var first LogRecord
		if source, ok := h.deps.Ledger.(LogRanger); ok {
			var err error
			first, err = source.FirstLog(r.Context())
			if err != nil {
				failure(w, 500, err.Error())
				return
			}
		} else {
			all, ok := h.records(w)
			if !ok {
				return
			}
			if len(all) > 0 {
				first = all[0]
			}
		}
		if first != nil {
			at, err := time.Parse(time.RFC3339Nano, text(first["ts"]))
			if err == nil && at.Before(start) {
				start = midnight(at.In(now.Location()))
			}
		}
	}
	summary := map[string]any{}
	for _, k := range []string{"totalSpendUsd", "apiSpendUsd", "subscriptionValueUsd", "totalTokens", "promptTokens", "completionTokens", "cacheReadTokens", "totalRequests", "successfulRequests", "errorRequests", "avgLatencyMs"} {
		summary[k] = 0.0
	}
	series := []map[string]any{}
	for at := start; !at.After(now); at = at.Add(width) {
		label := at.Format("Jan 2")
		if width == time.Hour {
			label = at.Format("15:00")
		}
		series = append(series, map[string]any{"timestamp": iso(at), "label": label, "spendUsd": 0.0, "subscriptionUsd": 0.0, "promptTokens": 0, "completionTokens": 0, "cacheReadTokens": 0, "totalTokens": 0, "requests": 0, "errorRequests": 0})
	}
	add := func(m map[string]any, k string, n float64) { m[k] = number(m[k]) + n }
	models := map[string]map[string]any{}
	modelOrder := []string{}
	keyStats := map[string]map[string]any{}
	keyOrder := []string{}
	latency := 0.0
	if !h.scanRecords(w, r, LogRange{Start: start, End: now, RequestsOnly: true}, func(rec LogRecord) error {
		if rec["kind"] == "brain" {
			return nil
		}
		at, err := time.Parse(time.RFC3339Nano, text(rec["ts"]))
		if err != nil || at.Before(start) || at.After(now) {
			return nil
		}
		cost := number(rec["costUsd"])
		sub := rec["billing"] == "subscription"
		spendKey := "spendUsd"
		if sub {
			spendKey = "subscriptionUsd"
		}
		id := text(rec["keyId"])
		if id != "" {
			ks := keyStats[id]
			if ks == nil {
				name := text(rec["keyName"])
				if name == "" {
					name = id
				}
				ks = map[string]any{"id": id, "name": name, "requests": 0, "spendUsd": 0.0, "subscriptionUsd": 0.0}
				keyStats[id] = ks
				keyOrder = append(keyOrder, id)
			}
			add(ks, "requests", 1)
			add(ks, spendKey, cost)
		}
		if keyID != "all" && keyID != id {
			return nil
		}
		add(summary, "totalRequests", 1)
		if sub {
			add(summary, "subscriptionValueUsd", cost)
		} else {
			add(summary, "apiSpendUsd", cost)
		}
		tokens := number(rec["promptTokens"]) + number(rec["completionTokens"]) + number(rec["cacheReadTokens"])
		for _, k := range []string{"promptTokens", "completionTokens", "cacheReadTokens"} {
			add(summary, k, number(rec[k]))
		}
		add(summary, "totalTokens", tokens)
		latency += number(rec["latencyMs"])
		success := number(rec["status"]) >= 200 && number(rec["status"]) < 400 && text(rec["error"]) == ""
		if success {
			add(summary, "successfulRequests", 1)
		} else {
			add(summary, "errorRequests", 1)
		}
		model := canonical(text(rec["model"]))
		m := models[model]
		if m == nil {
			m = map[string]any{"model": model, "label": modelLabel(model, text(rec["model"])), "variants": []string{}, "requests": 0, "promptTokens": 0, "completionTokens": 0, "cacheReadTokens": 0, "totalTokens": 0, "spendUsd": 0.0, "subscriptionUsd": 0.0, "percentSpend": 0.0}
			models[model] = m
			modelOrder = append(modelOrder, model)
		}
		// Upgrade the label when a later spelling resolves a catalog name the first did not.
		if m["label"] == model {
			m["label"] = modelLabel(model, text(rec["model"]))
		}
		variants := m["variants"].([]string)
		spelling := text(rec["model"])
		if spelling == "" {
			spelling = "unknown"
		}
		found := false
		for _, v := range variants {
			found = found || v == spelling
		}
		if !found {
			m["variants"] = append(variants, spelling)
		}
		add(m, "requests", 1)
		add(m, spendKey, cost)
		for _, k := range []string{"promptTokens", "completionTokens", "cacheReadTokens"} {
			add(m, k, number(rec[k]))
		}
		add(m, "totalTokens", tokens)
		index := int(at.Sub(start) / width)
		if index >= 0 && index < len(series) {
			pt := series[index]
			pt[spendKey] = round6(number(pt[spendKey]) + cost)
			add(pt, "requests", 1)
			for _, k := range []string{"promptTokens", "completionTokens", "cacheReadTokens"} {
				add(pt, k, number(rec[k]))
			}
			add(pt, "totalTokens", tokens)
			if !success {
				add(pt, "errorRequests", 1)
			}
		}
		return nil
	}) {
		return
	}
	api := round6(number(summary["apiSpendUsd"]))
	sub := round6(number(summary["subscriptionValueUsd"]))
	total := round6(api + sub)
	summary["apiSpendUsd"] = api
	summary["subscriptionValueUsd"] = sub
	summary["totalSpendUsd"] = total
	if n := number(summary["totalRequests"]); n > 0 {
		summary["avgLatencyMs"] = math.Round(latency / n)
	}
	modelList := []map[string]any{}
	for _, id := range modelOrder {
		m := models[id]
		m["spendUsd"] = round6(number(m["spendUsd"]))
		m["subscriptionUsd"] = round6(number(m["subscriptionUsd"]))
		if total > 0 {
			m["percentSpend"] = math.Round((number(m["spendUsd"])+number(m["subscriptionUsd"]))/total*1000) / 10
		}
		modelList = append(modelList, m)
	}
	sort.SliceStable(modelList, func(i, j int) bool {
		a := number(modelList[i]["spendUsd"]) + number(modelList[i]["subscriptionUsd"])
		b := number(modelList[j]["spendUsd"]) + number(modelList[j]["subscriptionUsd"])
		if a == b {
			return number(modelList[i]["requests"]) > number(modelList[j]["requests"])
		}
		return a > b
	})
	keyList := []map[string]any{}
	for _, id := range keyOrder {
		k := keyStats[id]
		k["spendUsd"] = round6(number(k["spendUsd"]))
		k["subscriptionUsd"] = round6(number(k["subscriptionUsd"]))
		keyList = append(keyList, k)
	}
	sort.SliceStable(keyList, func(i, j int) bool {
		a := number(keyList[i]["spendUsd"])
		b := number(keyList[j]["spendUsd"])
		if a == b {
			return number(keyList[i]["requests"]) > number(keyList[j]["requests"])
		}
		return a > b
	})
	send(w, 200, map[string]any{"range": rangeID, "keyId": keyID, "startTime": iso(start), "endTime": iso(now), "summary": summary, "series": series, "models": modelList, "keys": keyList})
}

// modelLabel is modelGroupOf().label in src/models.ts: the catalog's display name for the
// canonical id (else for the raw spelling), falling back to the canonical id.
func modelLabel(key, raw string) string {
	identity := catalogsync.Identity{}
	if name := identity.Of(key).DisplayName; name != "" {
		return name
	}
	if name := identity.Of(strings.TrimSpace(raw)).DisplayName; name != "" {
		return name
	}
	return key
}
