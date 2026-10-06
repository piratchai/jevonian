package freebuff

import (
	"sort"
	"strings"
)

// Model is one free-mode model: its upstream id and the root agent that runs it.
type Model struct {
	ID    string
	Agent string
}

// builtinModels is the free-mode model → root-agent table, taken from
// CodebuffAI/freebuff (common/src/constants/free-agents.ts). The upstream list
// moves quickly, so config can override or extend it via Options.Agents.
var builtinModels = []Model{
	{"mimo/mimo-v2.5", "base2-free-mimo"},
	{"openai/gpt-6-luna", "base2-free-luna-6"},
	{"openai/gpt-6.1-sol", "base2-free-gpt-6-1-sol"},
	{"stealth/space-bunny-alpha", "base2-free-space-bunny-alpha"},
	{"deepseek/deepseek-v4-flash", "base2-free-deepseek-flash"},
	{"deepseek/deepseek-v4-flash-fast", "base2-free-deepseek-flash-fast"},
	{"deepseek/deepseek-v4.1-flash", "base2-free-deepseek-v4-1-flash"},
	{"z-ai/glm-5.3-flash", "base2-free-glm-5-3-flash"},
	{"z-ai/glm-5.3", "base2-free-glm-5-3"},
	{"crof/kimi-k3-eco", "base2-free-kimi-k3-eco"},
	{"moonshotai/kimi-k3", "base2-free-kimi-k3"},
	{"anthropic/claude-fable-5.1", "base2-free-fable"},
	{"google/gemini-3.8-flash", "base2-free-gemini-3-8-flash"},
	{"qwen/qwen3.8-flash", "base2-free-qwen3-8-flash"},
	{"x-ai/grok-4.7", "base2-free-grok-4-7"},
}

// BuiltinModels lists the built-in model table (a copy).
func BuiltinModels() []Model { return append([]Model(nil), builtinModels...) }

// Bare is the model segment after the vendor prefix (deepseek/deepseek-v4-flash
// → deepseek-v4-flash). Config lists bare ids so the same model routes across
// providers by canonical name.
func Bare(id string) string {
	if i := strings.LastIndex(id, "/"); i >= 0 {
		return id[i+1:]
	}
	return id
}

// ModelIDs are the built-in models as bare ids, sorted. Used as the discovery
// source: the session endpoint cannot list models without taking a session.
func ModelIDs() []string {
	ids := make([]string, 0, len(builtinModels))
	for _, m := range builtinModels {
		ids = append(ids, Bare(m.ID))
	}
	sort.Strings(ids)
	return ids
}

// Resolver maps the model names clients use onto upstream ids and agents.
type Resolver struct {
	byID    map[string]Model
	byAlias map[string]string // normalized alias → upstream id
}

// NewResolver builds a Resolver from the built-in table plus extra agents
// (upstream id → agent id), which override or add entries.
func NewResolver(extra map[string]string) *Resolver {
	r := &Resolver{byID: map[string]Model{}, byAlias: map[string]string{}}
	add := func(m Model) {
		r.byID[m.ID] = m
		for _, alias := range aliasesOf(m.ID) {
			if _, taken := r.byAlias[alias]; !taken {
				r.byAlias[alias] = m.ID
			}
		}
	}
	for _, m := range builtinModels {
		add(m)
	}
	for id, agent := range extra {
		id, agent = strings.TrimSpace(id), strings.TrimSpace(agent)
		if id != "" && agent != "" {
			add(Model{ID: id, Agent: agent})
		}
	}
	return r
}

// Resolve returns the upstream model for a client-facing name.
//
// Accepted spellings: the upstream id (deepseek/deepseek-v4-flash), the bare
// name (deepseek-v4-flash), a freebuff/ prefix on either, and the dotted or
// dashed variants clients mangle (deepseek-v4.1-flash vs deepseek-v4-1-flash).
// ok is false for a name nothing matches; the caller decides the fallback.
func (r *Resolver) Resolve(name string) (Model, bool) {
	name = strings.TrimSpace(name)
	name = strings.TrimPrefix(name, "freebuff/")
	if m, ok := r.byID[name]; ok {
		return m, true
	}
	if id, ok := r.byAlias[normalize(name)]; ok {
		return r.byID[id], true
	}
	return Model{}, false
}

// aliasesOf is every spelling worth indexing for an upstream id.
func aliasesOf(id string) []string {
	bare := id
	if i := strings.LastIndex(id, "/"); i >= 0 {
		bare = id[i+1:]
	}
	return []string{normalize(id), normalize(bare)}
}

// normalize folds the separators clients disagree on.
func normalize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "freebuff/")
	return strings.NewReplacer(".", "-", "_", "-", "/", "-").Replace(s)
}
