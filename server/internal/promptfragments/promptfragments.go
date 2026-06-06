// Package promptfragments owns the closed-loop prompt-evolution
// data layer: variants per slot, lifecycle, and the A/B router.
//
// MOTIVATION
// ----------
// See migration 096_prompt_fragments.sql for the schema-level
// motivation. This package is the in-memory model + selector
// that the prompt-build path consults.
//
// SCOPE
// -----
//   * Owns Fragment, Status, Selector, Statistics, Router types.
//   * Pure / deterministic given a fixed RNG source. Tests
//     supply an explicit Source so the result is reproducible.
//   * Does NOT own database access. The persistence repo lives
//     in a separate file (or repository package) and feeds
//     Snapshot()s into this in-memory model.
package promptfragments

import (
	"errors"
	"sort"
	"sync"
	"time"
)

// Status mirrors the migration 096 prompt_fragments.status check.
type Status string

const (
	StatusDraft    Status = "draft"
	StatusShadow   Status = "shadow"
	StatusActive   Status = "active"
	StatusArchived Status = "archived"
)

// Fragment is one variant.
type Fragment struct {
	SlotKey       string    `json:"slotKey"`
	VariantID     string    `json:"variantId"`
	Body          string    `json:"body"`
	Status        Status    `json:"status"`
	Weight        int       `json:"weight"`
	Notes         string    `json:"notes,omitempty"`
	AuthorUserID  string    `json:"authorUserId,omitempty"`
	ParentVariant string    `json:"parentVariant,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

// Statistics is the rolling outcome snapshot for one variant.
type Statistics struct {
	SlotKey    string    `json:"slotKey"`
	VariantID  string    `json:"variantId"`
	Uses       int       `json:"uses"`
	Hits       int       `json:"hits"`
	SumAlpha   float64   `json:"sumAlpha"`
	MeanAlpha  float64   `json:"meanAlpha"`
	HitRate    float64   `json:"hitRate"`
	LastUsedAt time.Time `json:"lastUsedAt,omitempty"`
}

// Source is the minimal random/sample interface so tests can
// inject deterministic order. In production the wiring layer
// supplies a math/rand-backed source.
type Source interface {
	// Float64 returns a value in [0, 1).
	Float64() float64
}

// SelectionPolicy controls how the router picks between shadow
// variants and the active variant.
type SelectionPolicy struct {
	// ShadowFraction ∈ [0, 1] is the probability of routing
	// to a shadow variant when shadows exist. Default 0.10.
	ShadowFraction float64
	// FallbackToBody is the literal string returned when no
	// active or draft variant is found and no caller default
	// is supplied. Default empty.
	FallbackToBody string
}

// DefaultPolicy is the production-safe baseline.
func DefaultPolicy() SelectionPolicy {
	return SelectionPolicy{ShadowFraction: 0.10}
}

// Selector is the in-memory routing model.
//
// Hold all fragments for one slot in memory; on Pick() either
// return the active (with prob 1−shadowFraction) or one of the
// shadow variants (weighted by their Weight). Returns
// "no candidate" if neither active nor shadow is available.
type Selector struct {
	mu   sync.RWMutex
	cfg  SelectionPolicy
	bySlot map[string][]Fragment
}

// New returns an empty Selector.
func New(cfg SelectionPolicy) *Selector {
	return &Selector{
		cfg:    normalisePolicy(cfg),
		bySlot: make(map[string][]Fragment),
	}
}

// Load replaces the in-memory state from a snapshot. Ordering
// inside each slot is canonicalised (active first, then shadows
// by descending weight).
func (s *Selector) Load(snap []Fragment) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bySlot = make(map[string][]Fragment)
	for _, f := range snap {
		s.bySlot[f.SlotKey] = append(s.bySlot[f.SlotKey], f)
	}
	for slot := range s.bySlot {
		canonicalise(s.bySlot[slot])
	}
}

// canonicalise sorts fragments in-place: active first, then
// shadows by descending weight, then drafts/archived last.
func canonicalise(fs []Fragment) {
	sort.SliceStable(fs, func(i, j int) bool {
		ri, rj := lifecycleRank(fs[i].Status), lifecycleRank(fs[j].Status)
		if ri != rj {
			return ri < rj
		}
		if fs[i].Weight != fs[j].Weight {
			return fs[i].Weight > fs[j].Weight
		}
		return fs[i].VariantID < fs[j].VariantID
	})
}

func lifecycleRank(s Status) int {
	switch s {
	case StatusActive:
		return 0
	case StatusShadow:
		return 1
	case StatusDraft:
		return 2
	default:
		return 3
	}
}

// Pick returns the variant body to use for the given slot.
// When shadows exist, src.Float64() is consulted to decide
// between active and shadow.
func (s *Selector) Pick(slotKey string, src Source) (Fragment, error) {
	if s == nil {
		return Fragment{}, errors.New("promptfragments: nil selector")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	fs := s.bySlot[slotKey]
	if len(fs) == 0 {
		return Fragment{}, errors.New("promptfragments: no fragments for slot")
	}
	var active *Fragment
	var shadows []Fragment
	for i := range fs {
		f := fs[i]
		switch f.Status {
		case StatusActive:
			if active == nil {
				active = &f
			}
		case StatusShadow:
			shadows = append(shadows, f)
		}
	}
	if active == nil && len(shadows) == 0 {
		return Fragment{}, errors.New("promptfragments: no active or shadow variant")
	}
	if active == nil {
		// Shadow-only — pick weighted shadow.
		return weightedPick(shadows, src), nil
	}
	if len(shadows) == 0 {
		return *active, nil
	}
	roll := 0.0
	if src != nil {
		roll = src.Float64()
	}
	if roll < s.cfg.ShadowFraction {
		return weightedPick(shadows, src), nil
	}
	return *active, nil
}

func weightedPick(shadows []Fragment, src Source) Fragment {
	// Sum positive weights; fall back to uniform when all
	// weights are zero.
	totalWeight := 0
	for _, f := range shadows {
		if f.Weight > 0 {
			totalWeight += f.Weight
		}
	}
	if totalWeight == 0 || src == nil {
		// Uniform-deterministic: first by canonical order.
		// Guarantees Pick is reproducible across calls if no
		// Source is provided.
		return shadows[0]
	}
	roll := src.Float64() * float64(totalWeight)
	cum := 0.0
	for _, f := range shadows {
		cum += float64(maxInt(f.Weight, 0))
		if roll < cum {
			return f
		}
	}
	return shadows[len(shadows)-1]
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// Slots returns the list of slot keys currently loaded, sorted.
func (s *Selector) Slots() []string {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.bySlot))
	for slot := range s.bySlot {
		out = append(out, slot)
	}
	sort.Strings(out)
	return out
}

// PromoteDraftToShadow flips a draft → shadow. Returns an error
// if the variant doesn't exist or isn't currently 'draft'.
func (s *Selector) PromoteDraftToShadow(slotKey, variantID string) error {
	return s.transition(slotKey, variantID, StatusDraft, StatusShadow)
}

// PromoteShadowToActive flips a shadow → active. Demotes any
// existing active for the slot to archived (so the slot has
// at most one active variant — matching the migration's
// partial unique index).
func (s *Selector) PromoteShadowToActive(slotKey, variantID string) error {
	if s == nil {
		return errors.New("promptfragments: nil selector")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	fs := s.bySlot[slotKey]
	if len(fs) == 0 {
		return errors.New("promptfragments: slot empty")
	}
	target := -1
	current := -1
	for i := range fs {
		if fs[i].VariantID == variantID {
			target = i
		}
		if fs[i].Status == StatusActive {
			current = i
		}
	}
	if target == -1 {
		return errors.New("promptfragments: variant not found")
	}
	if fs[target].Status != StatusShadow {
		return errors.New("promptfragments: target must be shadow to promote")
	}
	if current >= 0 {
		fs[current].Status = StatusArchived
		fs[current].UpdatedAt = time.Now().UTC()
	}
	fs[target].Status = StatusActive
	fs[target].UpdatedAt = time.Now().UTC()
	canonicalise(fs)
	s.bySlot[slotKey] = fs
	return nil
}

// Archive flips any variant to archived (terminal).
func (s *Selector) Archive(slotKey, variantID string) error {
	if s == nil {
		return errors.New("promptfragments: nil selector")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	fs := s.bySlot[slotKey]
	for i := range fs {
		if fs[i].VariantID == variantID {
			fs[i].Status = StatusArchived
			fs[i].UpdatedAt = time.Now().UTC()
			canonicalise(fs)
			s.bySlot[slotKey] = fs
			return nil
		}
	}
	return errors.New("promptfragments: variant not found")
}

func (s *Selector) transition(slotKey, variantID string, from, to Status) error {
	if s == nil {
		return errors.New("promptfragments: nil selector")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	fs := s.bySlot[slotKey]
	for i := range fs {
		if fs[i].VariantID == variantID {
			if fs[i].Status != from {
				return errors.New("promptfragments: invalid transition")
			}
			fs[i].Status = to
			fs[i].UpdatedAt = time.Now().UTC()
			canonicalise(fs)
			s.bySlot[slotKey] = fs
			return nil
		}
	}
	return errors.New("promptfragments: variant not found")
}

func normalisePolicy(p SelectionPolicy) SelectionPolicy {
	if p.ShadowFraction < 0 {
		p.ShadowFraction = 0
	}
	if p.ShadowFraction > 1 {
		p.ShadowFraction = 1
	}
	return p
}
