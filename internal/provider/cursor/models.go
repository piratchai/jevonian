package cursor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xinyao27/jevonian/internal/paths"
)

// Model is one entry from `cursor-agent models`.
type Model struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Context int    `json:"context"`
}

var modelLine = regexp.MustCompile(`^([A-Za-z0-9][\w.:-]*) - (.+)$`)

var spaceRuns = regexp.MustCompile(`\s+`)

// ParseModels reads `cursor-agent models` output ("id - Name", one a line).
func ParseModels(out string) []Model {
	var models []Model
	for _, raw := range strings.FieldsFunc(ansiStrip(out), func(r rune) bool { return r == '\n' || r == '\r' }) {
		m := modelLine.FindStringSubmatch(strings.TrimSpace(raw))
		if m == nil {
			continue
		}
		// Names come with zero-width spaces and doubled ones.
		name := strings.ReplaceAll(m[2], "\u200b", "")
		name = strings.Join(strings.Fields(name), " ")
		name = strings.TrimSpace(defaultSuffix.ReplaceAllString(name, ""))
		models = append(models, Model{ID: m[1], Name: name, Context: Context(m[1], name)})
	}
	return models
}

var defaultSuffix = regexp.MustCompile(`(?i)\s*\((?:default|current)\)$`)

// DefaultContext is what Cursor lets a model hold when its name does not say.
const DefaultContext = 200_000

var millionContext = regexp.MustCompile(`\b(\d+)M\b`)

// Context is the context Cursor lets a model hold, read from its name
// ("Claude Opus 5.5 1M"). Every other id — effort/fast/thinking variants and
// "auto" alike — gets Cursor's default window.
func Context(id, name string) int {
	if m := millionContext.FindStringSubmatch(name); m != nil {
		n, err := strconv.Atoi(m[1])
		if err == nil {
			return n * 1_000_000
		}
	}
	return DefaultContext
}

var cursorEfforts = []struct {
	word  string
	level string
	label string
}{
	{"extra-high", "xhigh", "Extra High"},
	{"xhigh", "xhigh", "Extra High"},
	{"minimal", "minimal", "Minimal"},
	{"none", "none", "None"},
	{"low", "low", "Low"},
	{"medium", "medium", "Medium"},
	{"high", "high", "High"},
	{"max", "max", "Max"},
}

// SplitID splits a Cursor id into its family and its effort ("" when none is
// named). `-fast` and `-thinking` move onto the family: they describe a
// speed/think variant of the model, not an effort.
func SplitID(id string) (family, effort string) {
	s := id
	fast := false
	thinking := false
	if strings.HasSuffix(s, "-fast") && len(s) > len("-fast") {
		s = s[:len(s)-len("-fast")]
		fast = true
	}
	if strings.HasSuffix(s, "-thinking") && len(s) > len("-thinking") {
		s = s[:len(s)-len("-thinking")]
		thinking = true
	}
	for _, entry := range cursorEfforts {
		suffix := "-" + entry.word
		if strings.HasSuffix(s, suffix) && len(s) > len(suffix) {
			s = s[:len(s)-len(suffix)]
			effort = entry.level
			break
		}
	}
	if !thinking && strings.HasSuffix(s, "-thinking") && len(s) > len("-thinking") {
		s = s[:len(s)-len("-thinking")]
		thinking = true
	}
	if thinking {
		s += "-thinking"
	}
	if fast {
		s += "-fast"
	}
	return s, effort
}

func effortLabel(level string) string {
	for _, entry := range cursorEfforts {
		if entry.level == level {
			return entry.label
		}
	}
	return ""
}

// withoutWords removes the first run of words from name.
func withoutWords(name, words string) (string, bool) {
	ns := strings.Fields(name)
	ws := strings.Fields(words)
	if len(ws) == 0 {
		return name, false
	}
	for i := 0; i+len(ws) <= len(ns); i++ {
		match := true
		for k, word := range ws {
			if ns[i+k] != word {
				match = false
				break
			}
		}
		if match {
			out := append(append([]string{}, ns[:i]...), ns[i+len(ws):]...)
			return strings.Join(out, " "), true
		}
	}
	return name, false
}

type family struct {
	id       string
	variants []Model
	efforts  []string
}

func families(raw []Model) []*family {
	var out []*family
	by := map[string]*family{}
	for _, model := range raw {
		fid, effort := SplitID(model.ID)
		entry := by[fid]
		if entry == nil {
			entry = &family{id: fid}
			by[fid] = entry
			out = append(out, entry)
		}
		entry.variants = append(entry.variants, model)
		entry.efforts = append(entry.efforts, effort)
	}
	return out
}

// familyByEffort is the id for each effort, "" being the one Cursor picks by
// default.
func familyByEffort(f *family) map[string]string {
	out := map[string]string{}
	def := indexOf(f.efforts, "")
	for i, variant := range f.variants {
		effort := f.efforts[i]
		if _, has := out[effort]; !has {
			out[effort] = variant.ID
		}
		if def < 0 && effort != "" {
			if _, said := withoutWords(variant.Name, effortLabel(effort)); !said {
				def = i
			}
		}
	}
	if def < 0 {
		def = indexOf(f.efforts, "medium")
		if def < 0 {
			def = 0
		}
	}
	out[""] = f.variants[def].ID
	return out
}

func indexOf(list []string, want string) int {
	for i, v := range list {
		if v == want {
			return i
		}
	}
	return -1
}

// CollapseModels collapses each family to one model: named as its default,
// with the efforts it has.
func CollapseModels(raw []Model) []Model {
	var out []Model
	for _, f := range families(raw) {
		if len(f.variants) == 1 {
			out = append(out, f.variants[0])
			continue
		}
		def := familyByEffort(f)[""]
		name := f.id
		context := 0
		for i, variant := range f.variants {
			if variant.ID == def {
				name, _ = withoutWords(variant.Name, effortLabel(f.efforts[i]))
			}
			if variant.Context > 0 && (context == 0 || variant.Context < context) {
				context = variant.Context
			}
		}
		if context == 0 {
			context = DefaultContext
		}
		out = append(out, Model{ID: f.id, Name: name, Context: context})
	}
	return out
}

var errNotInstalled = errors.New("cursor-agent is not installed")

// FetchModels runs `cursor-agent models`.
func FetchModels(ctx context.Context) ([]Model, error) {
	path := Executable()
	if path == "" {
		return nil, errNotInstalled
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := execCommand(ctx, path, "models")
	if err != nil {
		return nil, err
	}
	return ParseModels(out), nil
}

// ─── catalog file (src/cursor-catalog.ts) ───────────────────────────────────

// CatalogFile is Cursor's own model list, kept on disk. The raw list is what
// maps a chosen effort onto the id Cursor expects; the collapsed list is what
// the picker shows.
type CatalogFile struct {
	FetchedAt string  `json:"fetchedAt"`
	Raw       []Model `json:"raw"`
	Models    []Model `json:"models"`
}

// CatalogPath is <dataDir>/cursor-models.json.
func CatalogPath() string { return filepath.Join(paths.DataDir(), "cursor-models.json") }

var catalogCache struct {
	sync.Mutex
	path string
	file *CatalogFile
}

func modelOf(raw map[string]any) (Model, bool) {
	id, _ := raw["id"].(string)
	if id == "" {
		return Model{}, false
	}
	name, _ := raw["name"].(string)
	if name == "" {
		name = id
	}
	context, ok := raw["context"].(float64)
	if !ok || context <= 0 {
		return Model{ID: id, Name: name, Context: Context(id, name)}, true
	}
	return Model{ID: id, Name: name, Context: int(context)}, true
}

func parseCatalogFile(data []byte) *CatalogFile {
	var raw struct {
		FetchedAt string           `json:"fetchedAt"`
		Raw       []map[string]any `json:"raw"`
		Models    []map[string]any `json:"models"`
	}
	if json.Unmarshal(data, &raw) != nil {
		return nil
	}
	var file CatalogFile
	file.FetchedAt = raw.FetchedAt
	for _, entry := range raw.Raw {
		if m, ok := modelOf(entry); ok {
			file.Raw = append(file.Raw, m)
		}
	}
	if len(file.Raw) == 0 {
		return nil
	}
	for _, entry := range raw.Models {
		if m, ok := modelOf(entry); ok {
			file.Models = append(file.Models, m)
		}
	}
	if len(file.Models) == 0 {
		file.Models = CollapseModels(file.Raw)
	}
	return &file
}

// LoadCatalog reads the saved model list, or nil when none was fetched yet.
func LoadCatalog() *CatalogFile {
	path := CatalogPath()
	catalogCache.Lock()
	defer catalogCache.Unlock()
	if catalogCache.path == path && catalogCache.file != nil {
		return catalogCache.file
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	file := parseCatalogFile(data)
	if file == nil {
		return nil
	}
	catalogCache.path, catalogCache.file = path, file
	return file
}

// SaveCatalog persists what `cursor-agent models` listed, both as-is and
// collapsed for the picker. A write failure is ignored (the catalog is a
// picker/pricing hint).
func SaveCatalog(raw []Model) *CatalogFile {
	file := &CatalogFile{
		FetchedAt: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		Raw:       raw,
		Models:    CollapseModels(raw),
	}
	path := CatalogPath()
	if data, err := json.MarshalIndent(file, "", "  "); err == nil {
		// The catalog is a picker/pricing hint; failing to cache it must not fail discovery.
		_ = writeFileAtomic(path, append(data, '\n'))
	}
	catalogCache.Lock()
	catalogCache.path, catalogCache.file = path, file
	catalogCache.Unlock()
	return file
}

// ResetCatalog drops the in-memory copy so the next lookup re-reads disk. For tests.
func ResetCatalog() {
	catalogCache.Lock()
	catalogCache.path, catalogCache.file = "", nil
	catalogCache.Unlock()
}

var effortOrder = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

func effortIndex(level string) int {
	for i, v := range effortOrder {
		if v == level {
			return i
		}
	}
	return -1
}

// ModelID is Cursor's id for a model the picker offers, at the effort asked
// for: the family's variant at it; else, for an effort between the ones it
// has, the one Cursor picks by default; else the nearest. Fast, when asked,
// comes from the family's fast variant. An id Cursor itself listed is kept,
// its effort and fast taken as it has them.
func ModelID(model string, effort string, fast bool) string {
	file := LoadCatalog()
	if file == nil {
		return model
	}
	byFamily := map[string]map[string]string{}
	for _, entry := range file.Raw {
		fid, level := SplitID(entry.ID)
		by := byFamily[fid]
		if by == nil {
			by = map[string]string{}
			byFamily[fid] = by
		}
		if _, has := by[level]; !has {
			by[level] = entry.ID
		}
	}
	at := func(f, level string) string { return byFamily[f][level] }

	// An id Cursor listed is used as it is, but its effort and speed follow the request.
	ownFamily, ownEffort := SplitID(model)
	if ownFamily != model {
		fam := ownFamily
		if fast && !strings.HasSuffix(fam, "-fast") {
			if _, has := byFamily[fam+"-fast"]; has {
				fam = fam + "-fast"
			}
		}
		level := effort
		if level == "" {
			level = ownEffort
		}
		if level != "" {
			if id := at(fam, level); id != "" {
				return id
			}
		}
		if id := at(fam, ""); id != "" {
			return id
		}
		return model
	}

	fam := model
	if fast && !strings.HasSuffix(fam, "-fast") {
		if _, has := byFamily[fam+"-fast"]; has {
			fam = fam + "-fast"
		}
	}
	by := byFamily[fam]
	if by == nil {
		return model
	}
	if effort == "" {
		if id := by[""]; id != "" {
			return id
		}
		return model
	}
	if exact := by[effort]; exact != "" {
		return exact
	}
	// Nearest level that exists, preferring the model's own default.
	var levels []string
	for _, level := range effortOrder {
		if by[level] != "" {
			levels = append(levels, level)
		}
	}
	if len(levels) == 0 {
		if id := by[""]; id != "" {
			return id
		}
		return model
	}
	target := effortIndex(effort)
	best := levels[0]
	bestDistance := int(^uint(0) >> 1)
	for _, level := range levels {
		d := effortIndex(level) - target
		if d < 0 {
			d = -d
		}
		if d < bestDistance {
			bestDistance = d
			best = level
		}
	}
	if id := by[best]; id != "" {
		return id
	}
	if id := by[""]; id != "" {
		return id
	}
	return model
}

func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + "." + strconv.Itoa(os.Getpid()) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
