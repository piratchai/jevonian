// Package catalogsync keeps the models.dev pricing and leaderboard snapshots
// fresh on disk and reports their cache status for the dashboard. Port of
// src/catalog-sync.ts + src/leaderboard.ts.
//
// Snapshots live under the data dir (pricing.json / leaderboard.json) so the
// CLI, the admin API, and long-lived serve processes share them without
// coordination. Refresh writes are atomic; readers never see a torn snapshot.
package catalogsync

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/xinyao27/jevonian/internal/paths"
	"github.com/xinyao27/jevonian/internal/routing"
)

// CacheTTL is the shared 12h freshness window for pricing and benchmarks.
const CacheTTL = 12 * time.Hour

// ModelsDevModelsURL is the unified benchmark catalog endpoint.
// JEVONIAN_MODELS_DEV_MODELS_URL overrides it for tests/mirrors.
func ModelsDevModelsURL() string {
	if v := os.Getenv("JEVONIAN_MODELS_DEV_MODELS_URL"); v != "" {
		return v
	}
	return "https://models.dev/models.json"
}

// LeaderboardSnapshot mirrors src/leaderboard.ts LeaderboardSnapshot.
type LeaderboardSnapshot struct {
	FetchedAt string                           `json:"fetchedAt"`
	Source    string                           `json:"source"`
	Models    map[string]CatalogBenchmarkModel `json:"models"`
}

// CatalogBenchmarkModel is one unified models.dev row with benchmark scores.
type CatalogBenchmarkModel struct {
	ID             string            `json:"id"`
	Name           string            `json:"name"`
	NormalizedName string            `json:"normalizedName"`
	Benchmarks     []BenchmarkRecord `json:"benchmarks"`
}

// BenchmarkRecord is one score on one board.
type BenchmarkRecord struct {
	BoardID string  `json:"boardId"`
	Name    string  `json:"name"`
	Score   float64 `json:"score"`
	Metric  string  `json:"metric,omitempty"`
	Variant string  `json:"variant,omitempty"`
	Effort  string  `json:"effort,omitempty"`
}

// LoadLeaderboard reads the benchmark snapshot (nil when absent/corrupt).
// Mirrors loadLeaderboardSnapshot: older HF/arena files without `models` are ignored.
func LoadLeaderboard() *LeaderboardSnapshot {
	b, err := os.ReadFile(paths.LeaderboardPath())
	if err != nil {
		return nil
	}
	var snap LeaderboardSnapshot
	if json.Unmarshal(b, &snap) != nil || snap.Models == nil {
		return nil
	}
	return &snap
}

// SaveLeaderboard writes the snapshot atomically.
func SaveLeaderboard(s *LeaderboardSnapshot) error {
	return atomicJSON(paths.LeaderboardPath(), s, 0o644)
}

func (s *LeaderboardSnapshot) fresh(now time.Time) bool {
	if s == nil || s.FetchedAt == "" {
		return false
	}
	at, err := time.Parse(time.RFC3339Nano, s.FetchedAt)
	return err == nil && now.Sub(at) < CacheTTL
}

// BoardIDs lists the unique board ids in the snapshot (status/diagnostics).
func BoardIDs(s *LeaderboardSnapshot) []string {
	if s == nil {
		return nil
	}
	set := map[string]bool{}
	for _, m := range s.Models {
		for _, b := range m.Benchmarks {
			set[b.BoardID] = true
		}
	}
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

var (
	boardIDNoise = regexp.MustCompile(`[^a-z0-9]+`)
	effortTail   = regexp.MustCompile(`(?i)(^|[^a-z])(xhigh|ultra|max|high|medium|low|minimal|none)([^a-z]|$)`)
)

// BoardIDFromName slugifies a benchmark display name (src boardIdFromName).
func BoardIDFromName(name string) string {
	s := strings.ToLower(name)
	s = strings.ReplaceAll(s, "'", "")
	s = strings.ReplaceAll(s, "’", "")
	s = strings.ReplaceAll(s, "τ", "tau")
	s = boardIDNoise.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

func effortFromVariant(variant string) string {
	m := effortTail.FindStringSubmatch(strings.ToLower(variant))
	if len(m) < 3 {
		return ""
	}
	return m[2]
}

// NormalizeName mirrors src/identity.ts normalizeModelName closely enough for
// benchmark soft matching: lowercase, strip decorations.
func NormalizeName(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = boardIDNoise.ReplaceAllString(s, " ")
	return strings.Join(strings.Fields(s), " ")
}

// MapModelsDevBenchmarks mirrors src/leaderboard.ts mapModelsDevBenchmarks:
// keeps only models carrying at least one numeric score.
func MapModelsDevBenchmarks(raw map[string]any) map[string]CatalogBenchmarkModel {
	out := map[string]CatalogBenchmarkModel{}
	for key, v := range raw {
		item, _ := v.(map[string]any)
		id, _ := item["id"].(string)
		if id == "" {
			id = key
		}
		name, _ := item["name"].(string)
		if name == "" {
			name = id
		}
		list, _ := item["benchmarks"].([]any)
		var benchmarks []BenchmarkRecord
		for _, e := range list {
			row, _ := e.(map[string]any)
			benchName, _ := row["name"].(string)
			score, ok := row["score"].(float64)
			if benchName == "" || !ok {
				continue
			}
			variant, _ := row["variant"].(string)
			rec := BenchmarkRecord{BoardID: BoardIDFromName(benchName), Name: benchName, Score: score}
			if s, _ := row["metric"].(string); s != "" {
				rec.Metric = s
			}
			if variant != "" {
				rec.Variant = variant
			}
			if e := effortFromVariant(variant); e != "" {
				rec.Effort = e
			}
			benchmarks = append(benchmarks, rec)
		}
		if len(benchmarks) == 0 {
			continue
		}
		out[id] = CatalogBenchmarkModel{ID: id, Name: name, NormalizedName: NormalizeName(name), Benchmarks: benchmarks}
	}
	return out
}

// FetchBenchmarks downloads the unified benchmark catalog.
func FetchBenchmarks(ctx context.Context, client *http.Client) (map[string]CatalogBenchmarkModel, error) {
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ModelsDevModelsURL(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("accept", "application/json")
	req.Header.Set("user-agent", "jevonian-catalog-sync")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("models.dev models.json responded with HTTP %d", resp.StatusCode)
	}
	var raw map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&raw); err != nil {
		return nil, err
	}
	return MapModelsDevBenchmarks(raw), nil
}

// RefreshLeaderboard mirrors src/leaderboard.ts refreshLeaderboard: fresh
// snapshots are reused unless force; a failed fetch keeps the prior snapshot.
func RefreshLeaderboard(ctx context.Context, client *http.Client, force bool) (snap *LeaderboardSnapshot, cached bool, err error) {
	now := time.Now().UTC()
	existing := LoadLeaderboard()
	if !force && existing.fresh(now) {
		return existing, true, nil
	}
	models, err := FetchBenchmarks(ctx, client)
	if err != nil || len(models) == 0 {
		if existing != nil && len(existing.Models) > 0 {
			if err == nil {
				err = fmt.Errorf("models.dev models.json returned no benchmarked models")
			}
			return existing, true, err
		}
		if err == nil {
			err = fmt.Errorf("models.dev models.json returned no benchmarked models")
		}
		return nil, false, err
	}
	snap = &LeaderboardSnapshot{FetchedAt: now.Format(time.RFC3339Nano), Source: ModelsDevModelsURL(), Models: models}
	if werr := SaveLeaderboard(snap); werr != nil {
		return snap, false, werr
	}
	return snap, false, nil
}

var noiseTokens = map[string]bool{
	"mxfp4": true, "gguf": true, "awq": true, "gptq": true,
	"int4": true, "int8": true, "fp8": true, "fp16": true, "bf16": true,
	"autoround": true, "mixed": true, "preview": true, "exp": true,
	"latest": true, "instruct": true, "chat": true, "base": true,
	"hf": true, "mlx": true, "ggml": true,
}

func tokensOf(label string) []string {
	return strings.Fields(strings.TrimSpace(label))
}

func sameTokenSet(a, b []string) bool {
	if len(a) == 0 || len(a) != len(b) {
		return false
	}
	set := map[string]bool{}
	for _, t := range b {
		set[t] = true
	}
	for _, t := range a {
		if !set[t] {
			return false
		}
	}
	return true
}

// SoftNormalizeLabel strips quant/packaging noise tokens (src softNormalizeLabel).
func SoftNormalizeLabel(label string) string {
	var out []string
	for _, t := range tokensOf(label) {
		if noiseTokens[t] {
			continue
		}
		if matched, _ := regexp.MatchString(`^\d{8}$`, t); matched {
			continue
		}
		out = append(out, t)
	}
	return strings.Join(out, " ")
}

type matchKind int

const (
	matchNone   matchKind = -1
	matchExact  matchKind = 0
	matchTokens matchKind = 1
	matchSoft   matchKind = 2
)

func matchQuality(entryLabel, query string) matchKind {
	if entryLabel == query {
		return matchExact
	}
	if sameTokenSet(tokensOf(entryLabel), tokensOf(query)) {
		return matchTokens
	}
	se, sq := SoftNormalizeLabel(entryLabel), SoftNormalizeLabel(query)
	if se != "" && sq != "" && (se == sq || sameTokenSet(tokensOf(se), tokensOf(sq))) {
		return matchSoft
	}
	return matchNone
}

func bare(id string) string { return routing.BareModelID(id) }

// FindBenchmarkModel resolves a model id/label into the benchmark catalog,
// mirroring src/leaderboard.ts findCatalogBenchmarkModel.
func FindBenchmarkModel(snap *LeaderboardSnapshot, model string) (*CatalogBenchmarkModel, matchKind) {
	if snap == nil {
		return nil, matchNone
	}
	bareID := bare(model)
	ids := []string{model, bareID}
	if strings.Contains(model, "/") && model != bareID {
		ids = append(ids, model)
	}
	for _, id := range ids {
		if m, ok := snap.Models[id]; ok {
			entry := m
			return &entry, matchExact
		}
	}
	needle := strings.ToLower(bareID)
	for _, id := range ids {
		_ = id
		for key, entry := range snap.Models {
			if strings.ToLower(bare(entry.ID)) == needle || strings.ToLower(bare(key)) == needle {
				e := entry
				return &e, matchExact
			}
		}
		break
	}
	labels := []string{NormalizeName(bareID)}
	if name := NormalizeName(model); name != labels[0] {
		labels = append(labels, name)
	}
	var best *CatalogBenchmarkModel
	bestRank := matchNone
	for _, label := range labels {
		for key := range snap.Models {
			entry := snap.Models[key]
			if mk := matchQuality(entry.NormalizedName, label); mk != matchNone {
				if best == nil || mk < bestRank {
					e := entry
					best = &e
					bestRank = mk
				}
			}
		}
	}
	return best, bestRank
}

func pickHeadline(records []BenchmarkRecord) BenchmarkRecord {
	sorted := append([]BenchmarkRecord{}, records...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Score != sorted[j].Score {
			return sorted[i].Score > sorted[j].Score
		}
		ip, jp := 1, 1
		if sorted[i].Effort == "" && sorted[i].Variant == "" {
			ip = 0
		}
		if sorted[j].Effort == "" && sorted[j].Variant == "" {
			jp = 0
		}
		return ip < jp
	})
	return sorted[0]
}

func round2(v float64) float64 { return float64(int(v*100+0.5)) / 100 }

// LeaderboardView returns the compact benchmark payload the brain sees,
// mirroring src/leaderboard.ts leaderboardViewFor. Nil when unknown.
func LeaderboardView(model string) map[string]any {
	snap := LoadLeaderboard()
	entry, mk := FindBenchmarkModel(snap, model)
	if entry == nil {
		return nil
	}
	byName := map[string][]BenchmarkRecord{}
	for _, rec := range entry.Benchmarks {
		byName[rec.BoardID] = append(byName[rec.BoardID], rec)
	}
	byBoard := map[string]float64{}
	byEffort := map[string]map[string]float64{}
	for boardID, records := range byName {
		headline := round2(pickHeadline(records).Score)
		byBoard[boardID] = headline
		bestByEffort := map[string]float64{}
		for _, rec := range records {
			if rec.Effort == "" {
				continue
			}
			s := round2(rec.Score)
			if old, ok := bestByEffort[rec.Effort]; !ok || s > old {
				bestByEffort[rec.Effort] = s
			}
		}
		informative := len(bestByEffort) > 1
		if !informative {
			for _, s := range bestByEffort {
				if s != headline {
					informative = true
				}
			}
		}
		if !informative {
			continue
		}
		for effort, s := range bestByEffort {
			if byEffort[effort] == nil {
				byEffort[effort] = map[string]float64{}
			}
			byEffort[effort][boardID] = s
		}
	}
	out := map[string]any{"by_board": byBoard}
	if mk != matchExact {
		out["match"] = map[matchKind]string{matchTokens: "tokens", matchSoft: "soft"}[mk]
	}
	if len(byEffort) > 0 {
		out["by_effort"] = byEffort
	}
	return out
}

// Status mirrors src/catalog-sync.ts catalogStatus.
func Status(now time.Time, pricing map[string]any) map[string]any {
	board := LoadLeaderboard()
	freshFetched := ""
	if board != nil {
		freshFetched = board.FetchedAt
	}
	return map[string]any{
		"pricing": pricing,
		"leaderboard": map[string]any{
			"present":   board != nil,
			"fresh":     board.fresh(now),
			"fetchedAt": freshFetched,
			"boards":    BoardIDs(board),
			"models":    len(boardOrEmpty(board)),
			"ttlMs":     CacheTTL.Milliseconds(),
		},
	}
}

func boardOrEmpty(s *LeaderboardSnapshot) map[string]CatalogBenchmarkModel {
	if s == nil {
		return map[string]CatalogBenchmarkModel{}
	}
	return s.Models
}

// RefreshResult mirrors CatalogSyncResult.
type RefreshResult struct {
	Pricing     map[string]any `json:"pricing"`
	Leaderboard map[string]any `json:"leaderboard"`
}

// Deps injects the pricing refresh owned by internal/cli (it already speaks
// models.dev api.json). HTTP drives the benchmark fetch.
type Deps struct {
	HTTP           *http.Client
	RefreshPricing func() (map[string]any, error)
	PricingStatus  func() map[string]any
	Now            func() time.Time
}

// Refresh mirrors refreshCatalogCaches: both sources refreshed; partial
// failure keeps the other source's result (and prior on-disk data).
func (d Deps) Refresh(ctx context.Context, force bool) RefreshResult {
	now := time.Now().UTC()
	if d.Now != nil {
		now = d.Now()
	}
	out := RefreshResult{}
	var pricingErr, boardErr error
	var pricing map[string]any
	var board *LeaderboardSnapshot
	var boardCached bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		if d.RefreshPricing != nil {
			pricing, pricingErr = d.RefreshPricing()
		}
	}()
	board, boardCached, boardErr = RefreshLeaderboard(ctx, d.HTTP, force)
	<-done
	if pricingErr != nil {
		status := map[string]any{"cached": true, "error": pricingErr.Error()}
		if d.PricingStatus != nil {
			for k, v := range d.PricingStatus() {
				status[k] = v
			}
			status["cached"] = true
			status["error"] = pricingErr.Error()
		}
		out.Pricing = status
	} else if pricing != nil {
		out.Pricing = pricing
	}
	if boardErr != nil && board == nil {
		existing := LoadLeaderboard()
		out.Leaderboard = map[string]any{
			"boards":    len(BoardIDs(existing)),
			"models":    len(boardOrEmpty(existing)),
			"fetchedAt": fetchedAt(existing),
			"source":    sourceOf(existing),
			"cached":    true,
			"error":     boardErr.Error(),
		}
	} else {
		entry := map[string]any{
			"boards":    len(BoardIDs(board)),
			"models":    len(boardOrEmpty(board)),
			"fetchedAt": fetchedAt(board),
			"source":    sourceOf(board),
			"cached":    boardCached,
		}
		if boardErr != nil {
			entry["error"] = boardErr.Error()
		}
		out.Leaderboard = entry
	}
	_ = now
	return out
}

func fetchedAt(s *LeaderboardSnapshot) string {
	if s == nil {
		return ""
	}
	return s.FetchedAt
}
func sourceOf(s *LeaderboardSnapshot) string {
	if s == nil || s.Source == "" {
		return "models.dev/models.json"
	}
	return s.Source
}

// NeedsRefresh mirrors scheduleCatalogSync's staleness test.
func NeedsRefresh(pricing map[string]any, now time.Time) (pricingStale, boardStale bool) {
	board := LoadLeaderboard()
	boardStale = !board.fresh(now)
	pricingStale = true
	if pricing != nil {
		if fresh, ok := pricing["fresh"].(bool); ok && fresh && pricing["present"] == true {
			pricingStale = false
		}
	}
	return pricingStale, boardStale
}

func atomicJSON(path string, v any, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".catalogsync-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	b, err := json.MarshalIndent(v, "", "  ")
	if err == nil {
		_, err = f.Write(append(b, '\n'))
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	_ = os.Chmod(f.Name(), mode)
	return os.Rename(f.Name(), path)
}
