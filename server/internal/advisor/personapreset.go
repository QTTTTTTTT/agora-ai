// Package advisor implements the /advisor consultation surface —
// the "professional team to help users decide on a stock" mode that
// lives next to (and is isolated from) the existing fund / team
// system. See migration 098 for the data model.
//
// Three core types defined across this package:
//
//   - PersonaPreset      one row in advisor_persona_presets, holds
//                        which masters / tactics fan out for a given
//                        preset_key ("conservative" → buffett+munger+
//                        graham, "cn_short" → tail_sniper+...).
//   - Service            the request-time orchestrator. Resolves
//                        preset → runs MasterPanel / TacticPanel →
//                        persists.
//   - Repo               persistence façade backing the four
//                        advisor_* tables.
//
// Phase 0 ships only PersonaPreset + Repo skeletons so the new
// migrations and embed packages compile; Phase 1 fills in Service.
package advisor

import (
	"context"
	"errors"
	"strings"
)

// PersonaPreset mirrors one row of advisor_persona_presets. The
// service layer reads these to resolve "preset_key → list of master
// keys / tactic keys to fan out". Admins can add new presets via
// SQL (or, later, an admin UI) without code changes.
type PersonaPreset struct {
	Key            string
	LabelZh        string
	LabelEn        string
	DescriptionZh  string
	DescriptionEn  string
	MasterKeys     []string
	TacticKeys     []string
	Enabled        bool
	SortOrder      int
}

// PresetKind classifies a preset by its agent population — used by
// the service to pick the right panel (master vs tactic) without
// re-introspecting MasterKeys / TacticKeys.
type PresetKind string

const (
	PresetKindMasters PresetKind = "masters"
	PresetKindTactics PresetKind = "tactics"
	PresetKindMixed   PresetKind = "mixed"
	PresetKindEmpty   PresetKind = "empty"
)

// Kind returns the high-level classification of a preset based on
// which agent lists it carries. A `custom` preset starts as Empty;
// the service will read user-supplied master/tactic keys at request
// time and route accordingly.
func (p PersonaPreset) Kind() PresetKind {
	hasMaster := len(p.MasterKeys) > 0
	hasTactic := len(p.TacticKeys) > 0
	switch {
	case hasMaster && hasTactic:
		return PresetKindMixed
	case hasMaster:
		return PresetKindMasters
	case hasTactic:
		return PresetKindTactics
	default:
		return PresetKindEmpty
	}
}

// ErrPresetNotFound is returned by PresetLookup.Get when the key is
// unknown or the preset row is disabled.
var ErrPresetNotFound = errors.New("advisor: preset not found")

// PresetLookup is the read-side interface the Service depends on.
// The concrete implementation is Repo; tests substitute a map-backed
// fake.
type PresetLookup interface {
	Get(ctx context.Context, key string) (PersonaPreset, error)
	List(ctx context.Context, enabledOnly bool) ([]PersonaPreset, error)
}

// NormalizePresetKey trims + lowercases the key the way the service
// expects to look it up. Centralised so wire-shapes and DB rows
// agree on casing.
func NormalizePresetKey(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
