package catalogsync

import (
	"sort"

	"github.com/xinyao27/jevonian/internal/config"
	"github.com/xinyao27/jevonian/internal/routing"
)

// IdentitySameModel is one configured model that shares the requested model's
// catalog identity. src/models.ts identityGaps.
type IdentitySameModel struct {
	Provider string
	Model    string
	Official bool
}

// IdentityGap is one routed model the catalog knows under a different spelling.
type IdentityGap struct {
	// Model is the routed (declared) model id.
	Model string
	// Identity is what the catalog says about it.
	Identity ModelIdentity
	// SameModel lists every configured model with the same identity.
	SameModel []IdentitySameModel
	// Official lists the same-model entries served by the owning vendor.
	Official []IdentitySameModel
	// Suggestion is the modelAliases line that would pin the official spelling.
	Suggestion string
}

// IdentityGaps reports routed models the catalog can resolve under a different
// id. src/models.ts identityGaps. `models` must be sorted by the caller; the
// output preserves that order.
func IdentityGaps(cfg *config.Config, models []string) []IdentityGap {
	idx := NewIdentity()
	out := []IdentityGap{}
	if cfg == nil {
		return out
	}
	for _, model := range models {
		identity := idx.(Identity).Of(model)
		if identity.Label == "" {
			continue
		}
		key := identity.Key()
		same := []IdentitySameModel{}
		official := []IdentitySameModel{}
		for _, p := range cfg.Providers {
			for _, entry := range p.Models {
				if idx.IdentityKey(entry.ID) != key {
					continue
				}
				item := IdentitySameModel{
					Provider: p.Name,
					Model:    entry.ID,
					Official: idx.IsOfficial(cfg, p.Name, model),
				}
				same = append(same, item)
				if item.Official {
					official = append(official, item)
				}
			}
		}
		if len(same) == 0 {
			continue
		}
		out = append(out, IdentityGap{
			Model:      model,
			Identity:   identity,
			SameModel:  same,
			Official:   official,
			Suggestion: suggestionFor(cfg, idx, model, official),
		})
	}
	return out
}

// suggestionFor mirrors src/models.ts suggestionFor: only suggest a pin when an
// official spelling exists and no alias or automatic variant already reaches it.
func suggestionFor(cfg *config.Config, idx routing.IdentityIndex, model string, official []IdentitySameModel) string {
	if len(official) == 0 {
		return ""
	}
	target := official[0]
	exact := target.Provider + "/" + target.Model
	for _, pinned := range cfg.ModelAliases[model] {
		if pinned == exact {
			return ""
		}
	}
	for _, variant := range routing.CanonicalVariants(cfg, model, "", idx) {
		if variant.Provider == target.Provider && variant.Model == target.Model {
			return ""
		}
	}
	return `modelAliases: { "` + model + `": ["` + exact + `"] }`
}

// RedundantAliases reports canonical ids whose configured modelAliases entry no
// longer changes the candidate set or its priority. src/cli.ts doctor.
func RedundantAliases(cfg *config.Config) []string {
	if cfg == nil || len(cfg.ModelAliases) == 0 {
		return nil
	}
	idx := NewIdentity()
	without := *cfg
	without.ModelAliases = nil
	out := []string{}
	for canonical := range cfg.ModelAliases {
		configured := routing.CanonicalVariants(cfg, canonical, "", idx)
		automatic := routing.CanonicalVariants(&without, canonical, "", idx)
		if len(configured) == 0 || len(configured) != len(automatic) {
			continue
		}
		same := true
		for i := range configured {
			if configured[i].Provider != automatic[i].Provider || configured[i].Model != automatic[i].Model {
				same = false
				break
			}
		}
		if same {
			out = append(out, canonical)
		}
	}
	sort.Strings(out)
	return out
}
