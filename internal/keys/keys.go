// Package keys loads and validates Jevonian API keys (sk-jev-…) from the data dir.
//
// File format matches src/keys.ts: a JSON array of hashed records at
// <dataDir>/keys.json (mode 0600). Plaintext is shown once at create time.
package keys

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/xinyao27/jevonian/internal/ledger"
	"github.com/xinyao27/jevonian/internal/paths"
)

// Record is one stored (hashed) Jevonian key.
type Record struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Prefix     string   `json:"prefix"`
	Hash       string   `json:"hash"`
	CreatedAt  string   `json:"createdAt"`
	LastUsedAt string   `json:"lastUsedAt,omitempty"`
	Requests   int      `json:"requests"`
	LimitUSD   *float64 `json:"limitUsd"`
}

// Summary is the non-secret view of a key (optionally with spend).
type Summary struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Prefix          string   `json:"prefix"`
	CreatedAt       string   `json:"createdAt"`
	LastUsedAt      string   `json:"lastUsedAt,omitempty"`
	Requests        int      `json:"requests"`
	LimitUSD        *float64 `json:"limitUsd"`
	SpendUSD        *float64 `json:"spendUsd,omitempty"`
	SubscriptionUSD *float64 `json:"subscriptionUsd,omitempty"`
}

// DenyReason explains why AllowKey rejected a request.
type DenyReason string

const (
	DenyNone        DenyReason = ""
	DenyMissing     DenyReason = "missing"
	DenyInvalid     DenyReason = "invalid"
	DenyCreditLimit DenyReason = "credit_limit_exceeded"
)

// Decision is the /v1 auth outcome for a presented token.
type Decision struct {
	Allowed bool
	// Open is true when no keys exist yet (first-run accepts unauthenticated traffic).
	Open   bool
	Key    *Record
	Reason DenyReason
}

// usageFlushDelay batches request counters so the auth hot path never writes
// keys.json. src/keys.ts rewrote the file synchronously on every request.
const usageFlushDelay = 2 * time.Second

// Store reads and writes keys.json under a data directory.
//
// Records are cached in memory and reloaded when the file's mtime/size changes
// (so `jevonian keys create` from another process is picked up). Per-request
// usage (requests, lastUsedAt) is accumulated in memory and flushed in the
// background; Flush/Close persist anything pending.
type Store struct {
	path string
	db   *ledger.DB // optional; used for spend / credit limits

	mu      sync.Mutex
	cache   []Record
	byHash  map[string]int
	stamp   fileStamp
	loaded  bool
	pending map[string]usageDelta // keyed by record ID
	timer   *time.Timer
}

type fileStamp struct {
	mod  time.Time
	size int64
	ok   bool
}

type usageDelta struct {
	requests int
	lastUsed string
}

// Path returns the keys.json location under dataDir (default: paths.DataDir()).
func Path(dataDir string) string {
	if dataDir == "" {
		dataDir = paths.DataDir()
	}
	return filepath.Join(dataDir, "keys.json")
}

// Open opens a key store at dataDir/keys.json. db may be nil (credit checks skipped).
func Open(dataDir string, db *ledger.DB) *Store {
	return &Store{path: Path(dataDir), db: db}
}

// HasKeys reports whether any Jevonian keys are stored.
func (s *Store) HasKeys() bool {
	recs, err := s.load()
	return err == nil && len(recs) > 0
}

// List returns summaries sorted by createdAt (no spend).
func (s *Store) List() ([]Summary, error) {
	s.mu.Lock()
	recs, err := s.loadUnlocked()
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	out := make([]Summary, 0, len(recs))
	for _, r := range recs {
		if d, ok := s.pending[r.ID]; ok {
			r.Requests += d.requests
			r.LastUsedAt = d.lastUsed
		}
		out = append(out, summaryOf(r))
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt < out[j].CreatedAt
	})
	return out, nil
}

// ListWithUsage returns summaries with all-time API / subscription spend from the ledger.
func (s *Store) ListWithUsage() ([]Summary, error) {
	out, err := s.List()
	if err != nil {
		return nil, err
	}
	if s.db == nil {
		return out, nil
	}
	for i := range out {
		total, err := s.db.KeyAllTime(out[i].ID)
		if err != nil {
			return nil, err
		}
		api := round6(total.APIUsd)
		sub := round6(total.SubscriptionUsd)
		out[i].SpendUSD = &api
		out[i].SubscriptionUSD = &sub
	}
	return out, nil
}

// UpdateOptions is a partial key mutation. HasLimit distinguishes clearing a
// credit limit from leaving it untouched.
type UpdateOptions struct {
	Name     *string
	LimitUSD *float64
	HasLimit bool
}

// Update changes a key under the same lock used by request-counter flushes.
// Pending usage remains attached to the key instead of racing a separate file
// read/modify/write transaction in the admin layer.
func (s *Store) Update(id string, patch UpdateOptions) (Summary, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	recs, err := s.loadUnlocked()
	if err != nil {
		return Summary{}, false, err
	}
	out := append([]Record(nil), recs...)
	for i := range out {
		if out[i].ID != id {
			continue
		}
		if patch.Name != nil && *patch.Name != "" {
			out[i].Name = *patch.Name
		}
		if patch.HasLimit {
			out[i].LimitUSD = nil
			if patch.LimitUSD != nil && *patch.LimitUSD > 0 && !math.IsNaN(*patch.LimitUSD) && !math.IsInf(*patch.LimitUSD, 0) {
				value := *patch.LimitUSD
				out[i].LimitUSD = &value
			}
		}
		if err := s.saveUnlocked(out); err != nil {
			return Summary{}, false, err
		}
		rec := out[i]
		if d, ok := s.pending[id]; ok {
			rec.Requests += d.requests
			rec.LastUsedAt = d.lastUsed
		}
		return summaryOf(rec), true, nil
	}
	return Summary{}, false, nil
}

// Revoke removes a key atomically with respect to authentication and flushing.
func (s *Store) Revoke(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	recs, err := s.loadUnlocked()
	if err != nil {
		return false, err
	}
	out := make([]Record, 0, len(recs))
	found := false
	for _, rec := range recs {
		if rec.ID == id {
			found = true
		} else {
			out = append(out, rec)
		}
	}
	if !found {
		return false, nil
	}
	if err := s.saveUnlocked(out); err != nil {
		return false, err
	}
	delete(s.pending, id)
	return true, nil
}

// KeySpendUSD returns lifetime API spend for a key (subscription excluded).
func (s *Store) KeySpendUSD(keyID string) (float64, error) {
	if s.db == nil {
		return 0, nil
	}
	total, err := s.db.KeyAllTime(keyID)
	if err != nil {
		return 0, err
	}
	return round6(total.APIUsd), nil
}

// CreateOptions configure a new key.
type CreateOptions struct {
	Name     string
	LimitUSD *float64 // nil or <=0 → no limit
}

// CreateResult is the one-time plaintext key plus its stored summary.
type CreateResult struct {
	Key    string
	Record Summary
}

// Create issues a new sk-jev-… key (plaintext returned once) and persists its hash.
func (s *Store) Create(opts CreateOptions) (CreateResult, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return CreateResult{}, fmt.Errorf("keys: random: %w", err)
	}
	plaintext := "sk-jev-" + hex.EncodeToString(raw)

	idRaw := make([]byte, 6)
	if _, err := rand.Read(idRaw); err != nil {
		return CreateResult{}, fmt.Errorf("keys: id: %w", err)
	}
	name := strings.TrimSpace(opts.Name)
	if name == "" {
		name = "default"
	}
	var limit *float64
	if opts.LimitUSD != nil && *opts.LimitUSD > 0 {
		v := *opts.LimitUSD
		limit = &v
	}
	rec := Record{
		ID:        hex.EncodeToString(idRaw),
		Name:      name,
		Prefix:    plaintext[:11],
		Hash:      hashKey(plaintext),
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Requests:  0,
		LimitUSD:  limit,
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	recs, err := s.loadUnlocked()
	if err != nil {
		return CreateResult{}, err
	}
	recs = append(append([]Record(nil), recs...), rec)
	if err := s.saveUnlocked(recs); err != nil {
		return CreateResult{}, err
	}
	return CreateResult{Key: plaintext, Record: summaryOf(rec)}, nil
}

// AllowKey validates a Bearer / x-api-key token for /v1.
//
// When no keys exist, traffic is allowed (Open=true) so first-run stays usable.
// When keys exist, a matching hash is required; API spend at or above LimitUSD
// yields DenyCreditLimit (subscription spend does not count).
func (s *Store) AllowKey(token string) (Decision, error) {
	token = strings.TrimSpace(token)

	s.mu.Lock()
	recs, err := s.loadUnlocked()
	if err != nil {
		s.mu.Unlock()
		return Decision{}, err
	}
	if len(recs) == 0 {
		s.mu.Unlock()
		return Decision{Allowed: true, Open: true}, nil
	}
	if token == "" {
		s.mu.Unlock()
		return Decision{Allowed: false, Reason: DenyMissing}, nil
	}
	idx, ok := s.byHash[hashKey(token)]
	if !ok {
		s.mu.Unlock()
		return Decision{Allowed: false, Reason: DenyInvalid}, nil
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	key := recs[idx]
	d := s.pending[key.ID]
	d.requests++
	d.lastUsed = now
	s.pending[key.ID] = d
	key.Requests += d.requests
	key.LastUsedAt = now
	s.scheduleFlushLocked()
	s.mu.Unlock()

	// The spend query runs outside the lock: it hits SQLite, not keys.json.
	if key.LimitUSD != nil && *key.LimitUSD > 0 && s.db != nil {
		spend, err := s.db.KeyAllTime(key.ID)
		if err != nil {
			return Decision{}, err
		}
		if spend.APIUsd >= *key.LimitUSD {
			return Decision{Allowed: false, Key: &key, Reason: DenyCreditLimit}, nil
		}
	}
	return Decision{Allowed: true, Key: &key}, nil
}

// TokenFromHeaders extracts a Bearer or x-api-key token (case-insensitive Bearer).
func TokenFromHeaders(authorization, xAPIKey string) string {
	auth := strings.TrimSpace(authorization)
	if len(auth) >= 7 && strings.EqualFold(auth[:7], "bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	return strings.TrimSpace(xAPIKey)
}

// Flush persists pending usage counters now.
func (s *Store) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flushLocked()
}

// Close flushes pending usage and stops the background timer.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	return s.flushLocked()
}

func (s *Store) scheduleFlushLocked() {
	if s.timer != nil {
		return
	}
	s.timer = time.AfterFunc(usageFlushDelay, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.timer = nil
		_ = s.flushLocked()
	})
}

func (s *Store) flushLocked() error {
	if len(s.pending) == 0 {
		return nil
	}
	// Re-read from disk so a concurrent create/revoke by the CLI is not lost.
	s.loaded = false
	recs, err := s.loadUnlocked()
	if err != nil {
		return err
	}
	next := append([]Record(nil), recs...)
	for i := range next {
		if d, ok := s.pending[next[i].ID]; ok {
			next[i].Requests += d.requests
			next[i].LastUsedAt = d.lastUsed
		}
	}
	if err := s.saveUnlocked(next); err != nil {
		return err
	}
	s.pending = nil
	return nil
}

func (s *Store) load() ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadUnlocked()
}

// loadUnlocked returns the cached records, re-reading keys.json only when its
// mtime or size changed. Callers must not mutate the returned slice.
func (s *Store) loadUnlocked() ([]Record, error) {
	if s.pending == nil {
		s.pending = make(map[string]usageDelta)
	}
	st := statFile(s.path)
	if s.loaded && st == s.stamp {
		return s.cache, nil
	}
	recs, err := s.readFile()
	if err != nil {
		return nil, err
	}
	s.setCacheLocked(recs, st)
	return s.cache, nil
}

func (s *Store) setCacheLocked(recs []Record, st fileStamp) {
	s.cache = recs
	s.byHash = make(map[string]int, len(recs))
	for i, r := range recs {
		s.byHash[r.Hash] = i
	}
	s.stamp = st
	s.loaded = true
}

func statFile(path string) fileStamp {
	info, err := os.Stat(path)
	if err != nil {
		return fileStamp{}
	}
	return fileStamp{mod: info.ModTime(), size: info.Size(), ok: true}
}

func (s *Store) readFile() ([]Record, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("keys: read: %w", err)
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		// Corrupt file → empty store (matches TS catch → []).
		return nil, nil
	}
	out := make([]Record, 0, len(raw))
	for _, item := range raw {
		var partial struct {
			ID         string   `json:"id"`
			Name       string   `json:"name"`
			Prefix     string   `json:"prefix"`
			Hash       string   `json:"hash"`
			CreatedAt  string   `json:"createdAt"`
			LastUsedAt string   `json:"lastUsedAt"`
			Requests   int      `json:"requests"`
			LimitUSD   *float64 `json:"limitUsd"`
		}
		if err := json.Unmarshal(item, &partial); err != nil {
			continue
		}
		if partial.ID == "" || partial.Hash == "" {
			continue
		}
		name := partial.Name
		if name == "" {
			name = partial.ID
		}
		prefix := partial.Prefix
		if prefix == "" {
			prefix = "sk-jev-"
		}
		var limit *float64
		if partial.LimitUSD != nil && !isNaN(*partial.LimitUSD) {
			limit = partial.LimitUSD
		}
		out = append(out, Record{
			ID:         partial.ID,
			Name:       name,
			Prefix:     prefix,
			Hash:       partial.Hash,
			CreatedAt:  partial.CreatedAt,
			LastUsedAt: partial.LastUsedAt,
			Requests:   partial.Requests,
			LimitUSD:   limit,
		})
	}
	return out, nil
}

func (s *Store) saveUnlocked(recs []Record) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("keys: mkdir: %w", err)
	}
	// Encode LimitUSD as null when unset (matches TS).
	type wire struct {
		ID         string   `json:"id"`
		Name       string   `json:"name"`
		Prefix     string   `json:"prefix"`
		Hash       string   `json:"hash"`
		CreatedAt  string   `json:"createdAt"`
		LastUsedAt string   `json:"lastUsedAt,omitempty"`
		Requests   int      `json:"requests"`
		LimitUSD   *float64 `json:"limitUsd"`
	}
	wired := make([]wire, len(recs))
	for i, r := range recs {
		wired[i] = wire{
			ID:         r.ID,
			Name:       r.Name,
			Prefix:     r.Prefix,
			Hash:       r.Hash,
			CreatedAt:  r.CreatedAt,
			LastUsedAt: r.LastUsedAt,
			Requests:   r.Requests,
			LimitUSD:   r.LimitUSD,
		}
	}
	data, err := json.MarshalIndent(wired, "", "  ")
	if err != nil {
		return fmt.Errorf("keys: marshal: %w", err)
	}
	data = append(data, '\n')
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("keys: write: %w", err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return fmt.Errorf("keys: chmod: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("keys: rename: %w", err)
	}
	s.setCacheLocked(recs, statFile(s.path))
	return nil
}

func summaryOf(r Record) Summary {
	return Summary{
		ID:         r.ID,
		Name:       r.Name,
		Prefix:     r.Prefix,
		CreatedAt:  r.CreatedAt,
		LastUsedAt: r.LastUsedAt,
		Requests:   r.Requests,
		LimitUSD:   r.LimitUSD,
	}
}

func hashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func round6(v float64) float64 {
	return math.Round(v*1e6) / 1e6
}

func isNaN(v float64) bool {
	return v != v
}
