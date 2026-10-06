package devin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xinyao27/jevonian/internal/paths"
	"github.com/xinyao27/jevonian/internal/wire"
)

// Price is USD per 1M tokens.
type Price struct {
	Input     float64  `json:"input"`
	Output    float64  `json:"output"`
	CacheRead *float64 `json:"cacheRead,omitempty"`
}

// Model is one GetCliModelConfigs entry.
type Model struct {
	ID            string
	Label         string
	Vendor        string
	Disabled      bool
	ContextWindow int
	MaxOutput     int
	Price         *Price
}

var providerVendors = map[uint64]string{
	1: "cognition",
	2: "openai",
	3: "anthropic",
	4: "google",
	7: "moonshot",
	9: "zhipu",
}

// roundPrice rounds float32 prices ("0.2000000029") back to the decimal the
// server meant (toPrecision(6)).
func roundPrice(value float64) float64 {
	v, _ := strconv.ParseFloat(strconv.FormatFloat(value, 'g', 6, 64), 64)
	return v
}

func modelPrice(fields []pbField) *Price {
	rows := map[string]float64{}
	for _, f := range fields {
		if f.num != 32 {
			continue
		}
		row := tryDecodePb(f.bytes)
		if row == nil {
			continue
		}
		label, _ := pbString(row, 1)
		label = strings.ToLower(strings.TrimSpace(label))
		value, ok := pbFloat32(row, 2)
		if label != "" && ok && !math.IsNaN(value) && !math.IsInf(value, 0) {
			rows[label] = roundPrice(value)
		}
	}
	input, okIn := rows["input"]
	output, okOut := rows["output"]
	if !okIn || !okOut {
		return nil
	}
	p := &Price{Input: input, Output: output}
	if cacheRead, ok := rows["cached input"]; ok {
		p.CacheRead = &cacheRead
	}
	return p
}

// ParseModels decodes GetCliModelConfigs; entries without a selector (#22) are dropped.
func ParseModels(raw []byte) []Model {
	var models []Model
	for _, entry := range tryDecodePb(raw) {
		if entry.num != 1 {
			continue
		}
		fields := tryDecodePb(entry.bytes)
		if fields == nil {
			continue
		}
		id, _ := pbString(fields, 22)
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		info := pbSub(fields, 23)
		label, _ := pbString(fields, 1)
		label = strings.TrimSpace(label)
		if label == "" {
			label = id
		}
		m := Model{ID: id, Label: label, Price: modelPrice(fields)}
		if provider, ok := pbInt(fields, 10); ok {
			m.Vendor = providerVendors[provider]
		}
		if disabled, ok := pbInt(fields, 4); ok && disabled == 1 {
			m.Disabled = true
		}
		if v, ok := pbInt(info, 4); ok {
			m.ContextWindow = int(v)
		}
		if v, ok := pbInt(info, 13); ok {
			m.MaxOutput = int(v)
		}
		models = append(models, m)
	}
	return models
}

// UserStatus is the plan and remaining daily/weekly allowance from GetUserStatus.
type UserStatus struct {
	Plan                   string
	DailyRemainingPercent  *float64
	WeeklyRemainingPercent *float64
	DailyResetsAt          time.Time
	WeeklyResetsAt         time.Time
}

func unixTime(fields []pbField, num int) time.Time {
	if v, ok := pbInt(fields, num); ok && v > 0 {
		return time.Unix(int64(v), 0).UTC()
	}
	return time.Time{}
}

// ParseUserStatus decodes GetUserStatus: #1 user_status → #13 plan_status.
func ParseUserStatus(raw []byte) UserStatus {
	status := pbSub(tryDecodePb(raw), 1)
	plan := pbSub(status, 13)
	if plan == nil {
		return UserStatus{}
	}
	out := UserStatus{
		DailyResetsAt:  unixTime(plan, 17),
		WeeklyResetsAt: unixTime(plan, 18),
	}
	if info := pbSub(plan, 1); info != nil {
		name, _ := pbString(info, 2)
		out.Plan = strings.TrimSpace(name)
	}
	if v, ok := pbInt(plan, 14); ok {
		f := float64(v)
		out.DailyRemainingPercent = &f
	}
	if v, ok := pbInt(plan, 15); ok {
		f := float64(v)
		out.WeeklyRemainingPercent = &f
	}
	return out
}

// QuotaWindow is one live allowance window, in the shape the TS quota module
// reports (`devin-daily` / `devin-weekly`).
type QuotaWindow struct {
	ID          string
	Label       string
	UsedPercent float64
	ResetsAt    time.Time
}

// Windows maps remaining percentages into used-percent windows. A window whose
// percent is absent is skipped rather than read as 0. Port of devinUsage in
// src/quota.ts.
func (s UserStatus) Windows() []QuotaWindow {
	var out []QuotaWindow
	push := func(id, label string, remaining *float64, resets time.Time) {
		if remaining == nil || math.IsNaN(*remaining) {
			return
		}
		left := math.Min(100, math.Max(0, *remaining))
		out = append(out, QuotaWindow{ID: id, Label: label, UsedPercent: 100 - left, ResetsAt: resets})
	}
	push("devin-daily", "day", s.DailyRemainingPercent, s.DailyResetsAt)
	push("devin-weekly", "week", s.WeeklyRemainingPercent, s.WeeklyResetsAt)
	return out
}

// unary is a unary Connect call: the raw protobuf body is ClientMetadata only;
// errors come back as JSON.
func unary(ctx context.Context, client *http.Client, token, baseURL, path string) ([]byte, error) {
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, normalizeBaseURL(baseURL)+path,
		bytes.NewReader(BytesField(1, clientMetadata(token))))
	if err != nil {
		return nil, err
	}
	req.Header = Headers(token, HeadersUnary)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxFrameBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		classified := ClassifyError(resp.StatusCode, string(raw), token)
		method := path[strings.LastIndex(path, "/")+1:]
		return nil, fmt.Errorf("Devin %s failed (HTTP %d, %s): %s", method, resp.StatusCode, classified.Kind, classified.Message)
	}
	return raw, nil
}

// FetchModels calls GetCliModelConfigs.
func FetchModels(ctx context.Context, client *http.Client, token, baseURL string) ([]Model, error) {
	raw, err := unary(ctx, client, token, baseURL, modelsPath)
	if err != nil {
		return nil, err
	}
	return ParseModels(raw), nil
}

// FetchUserStatus calls GetUserStatus.
func FetchUserStatus(ctx context.Context, client *http.Client, token, baseURL string) (UserStatus, error) {
	raw, err := unary(ctx, client, token, baseURL, userStatusPath)
	if err != nil {
		return UserStatus{}, err
	}
	return ParseUserStatus(raw), nil
}

// ─── on-disk model metadata (src/devin-catalog.ts) ──────────────────────────

var adaptivePrefix = regexp.MustCompile(`(?i)^adaptive`)

// IsRoutable reports whether a catalog entry is a model a router should offer.
// Devin also lists hundreds of `fusion-*` combos and server-side routers
// (`adaptive*`, `arena-*`) that are not a single model with a stable price or
// window.
func IsRoutable(m Model) bool {
	if m.Disabled || m.ID == "" {
		return false
	}
	if strings.HasPrefix(m.ID, "fusion-") || strings.HasPrefix(m.ID, "arena-") || adaptivePrefix.MatchString(m.ID) {
		return false
	}
	return true
}

// ModelMeta is per-model metadata kept on disk so pricing and capability
// lookups work for Devin-only ids that models.dev does not list.
type ModelMeta struct {
	Label         string `json:"label,omitempty"`
	ContextWindow int    `json:"contextWindow,omitempty"`
	MaxOutput     int    `json:"maxOutput,omitempty"`
	Price         *Price `json:"price,omitempty"`
}

type catalogFile struct {
	FetchedAt string               `json:"fetchedAt"`
	Models    map[string]ModelMeta `json:"models"`
}

// ModelsPath is <dataDir>/devin-models.json.
func ModelsPath() string { return filepath.Join(paths.DataDir(), "devin-models.json") }

var metaCache struct {
	sync.Mutex
	path   string
	models map[string]ModelMeta
}

func metaOf(m Model) ModelMeta {
	meta := ModelMeta{Label: m.Label}
	if m.ContextWindow > 0 {
		meta.ContextWindow = m.ContextWindow
	}
	if m.MaxOutput > 0 {
		meta.MaxOutput = m.MaxOutput
	}
	if p := m.Price; p != nil && p.Input >= 0 && p.Output >= 0 {
		price := *p
		if price.CacheRead != nil && *price.CacheRead < 0 {
			price.CacheRead = nil
		}
		meta.Price = &price
	}
	return meta
}

// sanitizeMeta drops non-positive windows and negative prices read from disk.
func sanitizeMeta(meta ModelMeta) ModelMeta {
	if meta.ContextWindow < 0 {
		meta.ContextWindow = 0
	}
	if meta.MaxOutput < 0 {
		meta.MaxOutput = 0
	}
	if p := meta.Price; p != nil {
		if p.Input < 0 || p.Output < 0 {
			meta.Price = nil
		} else if p.CacheRead != nil && *p.CacheRead < 0 {
			p.CacheRead = nil
		}
	}
	return meta
}

// SaveModelMeta persists catalog metadata for models, replacing the previous
// snapshot. A write failure is ignored: metadata is a hint and must not fail
// discovery.
func SaveModelMeta(models []Model) {
	entries := make(map[string]ModelMeta, len(models))
	for _, m := range models {
		entries[m.ID] = metaOf(m)
	}
	path := ModelsPath()
	file := catalogFile{FetchedAt: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), Models: entries}
	if data, err := json.MarshalIndent(file, "", "  "); err == nil {
		_ = writeFileAtomic(path, append(data, '\n'))
	}
	metaCache.Lock()
	metaCache.path, metaCache.models = path, entries
	metaCache.Unlock()
}

// LoadModelMeta is the cached catalog metadata keyed by Devin model id, read
// from disk once per data dir. A corrupt cache reads as empty.
func LoadModelMeta() map[string]ModelMeta {
	path := ModelsPath()
	metaCache.Lock()
	defer metaCache.Unlock()
	if metaCache.models != nil && metaCache.path == path {
		return metaCache.models
	}
	models := map[string]ModelMeta{}
	if data, err := os.ReadFile(path); err == nil {
		var file struct {
			Models map[string]json.RawMessage `json:"models"`
		}
		if json.Unmarshal(data, &file) == nil {
			for id, raw := range file.Models {
				var meta ModelMeta
				if json.Unmarshal(raw, &meta) == nil {
					models[id] = sanitizeMeta(meta)
				}
			}
		}
	}
	metaCache.path, metaCache.models = path, models
	return models
}

// LookupModelMeta is the metadata for one Devin model id (a `provider/` prefix
// is ignored).
func LookupModelMeta(model string) (ModelMeta, bool) {
	models := LoadModelMeta()
	if meta, ok := models[model]; ok {
		return meta, true
	}
	meta, ok := models[wire.BareModelID(model)]
	return meta, ok
}

// ResetModelMeta drops the in-memory copy so the next lookup re-reads disk. For tests.
func ResetModelMeta() {
	metaCache.Lock()
	metaCache.path, metaCache.models = "", nil
	metaCache.Unlock()
}

func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
