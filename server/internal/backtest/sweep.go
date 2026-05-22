package backtest

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ErrSweepTooLarge is returned when the axes' Cartesian product
// exceeds MaxSweepCells. We hard-cap to prevent operators from
// accidentally launching a 1000-job sweep that would saturate
// the LLM budget or DB.
var ErrSweepTooLarge = errors.New("sweep: too many cells (max 25)")

// ErrSweepEmpty is returned when a spec has zero axes or any
// axis has zero values. The fan-out would be ill-defined.
var ErrSweepEmpty = errors.New("sweep: axes empty")

// ErrSweepAxisUnknown is returned when an axis name is not in
// the allow-list. We restrict to a finite set because each name
// has to be wired into the runner's request override path.
var ErrSweepAxisUnknown = errors.New("sweep: unknown axis name")

// ErrSweepAxisValue is returned when an axis value can't be
// coerced to the field's underlying type (e.g. "abc" for
// slippageBps which expects a float).
var ErrSweepAxisValue = errors.New("sweep: invalid axis value")

// MaxSweepCells caps the Cartesian product. 25 = 5 × 5 or
// 5 × 5 × 1; chosen so a sweep can't accidentally trigger a
// 100-job LLM run.
const MaxSweepCells = 25

// MaxSweepAxes caps how many axes a single sweep may vary.
// Two is the sweet spot for a 2D heat-map; three would force a
// 3D table the UI can't render cleanly.
const MaxSweepAxes = 2

// SweepAxis describes one varying dimension. Name is the
// canonical Request field tag (see allowedSweepAxes); Values is
// the list of values to substitute. Values are kept as raw
// strings so the JSON wire format isn't forced into a single
// scalar type — applySweepAxis does the parse + coerce.
type SweepAxis struct {
	Name   string   `json:"name"`
	Values []string `json:"values"`
}

// SweepSpec is the input to a fan-out submission. Base is the
// "template" Request that's cloned for every cell; Axes are the
// per-cell overrides.
type SweepSpec struct {
	Base  Request     `json:"base"`
	Axes  []SweepAxis `json:"axes"`
	Name  string      `json:"name,omitempty"`
}

// SweepCell is one fanned-out child: the resolved Request plus a
// human-readable map of which axis value was applied. AxisValues
// is kept on the wire so the UI can render the grid row/col
// headers without re-parsing the spec.
type SweepCell struct {
	Request    Request           `json:"-"`
	AxisValues map[string]string `json:"axisValues"`
}

// ExpandSweep validates the spec and produces the Cartesian
// product of axis values, applying each combination on top of a
// fresh copy of Base. Returns ErrSweep* on validation failure.
// The order of the returned slice is stable: it varies the
// LAST axis fastest (row-major), which is what the UI's
// "rows = axis[0] values, cols = axis[1] values" mental model
// expects.
func ExpandSweep(spec SweepSpec) ([]SweepCell, error) {
	if len(spec.Axes) == 0 {
		return nil, ErrSweepEmpty
	}
	if len(spec.Axes) > MaxSweepAxes {
		return nil, fmt.Errorf("%w: %d axes (max %d)", ErrSweepTooLarge, len(spec.Axes), MaxSweepAxes)
	}
	total := 1
	for _, ax := range spec.Axes {
		if strings.TrimSpace(ax.Name) == "" {
			return nil, ErrSweepEmpty
		}
		if _, ok := allowedSweepAxes[ax.Name]; !ok {
			return nil, fmt.Errorf("%w: %q", ErrSweepAxisUnknown, ax.Name)
		}
		if len(ax.Values) == 0 {
			return nil, ErrSweepEmpty
		}
		total *= len(ax.Values)
	}
	if total > MaxSweepCells {
		return nil, fmt.Errorf("%w: %d cells", ErrSweepTooLarge, total)
	}

	// Build the cells by indexing through the Cartesian product.
	// indices[i] tracks the current position on axis i.
	indices := make([]int, len(spec.Axes))
	cells := make([]SweepCell, 0, total)
	for {
		req := spec.Base
		// Clone slices so cells don't share backing arrays —
		// otherwise mutating one cell's Symbols would smear into
		// siblings. (Not currently mutated, but defensive.)
		if len(spec.Base.Symbols) > 0 {
			req.Symbols = append([]string(nil), spec.Base.Symbols...)
		}
		if len(spec.Base.InitialPositions) > 0 {
			req.InitialPositions = append([]InitialPosition(nil), spec.Base.InitialPositions...)
		}
		axisVals := make(map[string]string, len(spec.Axes))
		for i, ax := range spec.Axes {
			val := ax.Values[indices[i]]
			if err := applySweepAxis(&req, ax.Name, val); err != nil {
				return nil, err
			}
			axisVals[ax.Name] = val
		}
		cells = append(cells, SweepCell{Request: req, AxisValues: axisVals})
		// Increment row-major (last axis varies fastest).
		k := len(indices) - 1
		for k >= 0 {
			indices[k]++
			if indices[k] < len(spec.Axes[k].Values) {
				break
			}
			indices[k] = 0
			k--
		}
		if k < 0 {
			break
		}
	}
	return cells, nil
}

// allowedSweepAxes is the closed set of Request fields a sweep
// can vary. Restricting this prevents callers from accidentally
// varying FundID (which would break the grouping) or Market
// (which would split the universe semantics).
//
// To add a new sweepable axis: append a key here AND extend the
// switch in applySweepAxis below.
var allowedSweepAxes = map[string]struct{}{
	"slippageBps":     {},
	"commissionBps":   {},
	"maxOrdersPerDay": {},
	"initialCash":     {},
	"engineKind":      {},
}

// SortedAllowedSweepAxes returns the allow-list as a sorted
// slice. Used by the handler to expose the contract to the web
// UI ("which axes can I vary?").
func SortedAllowedSweepAxes() []string {
	out := make([]string, 0, len(allowedSweepAxes))
	for k := range allowedSweepAxes {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// applySweepAxis mutates req to override the named axis with
// the given value. Returns ErrSweepAxisValue if the value
// doesn't parse for the axis' underlying type.
func applySweepAxis(req *Request, name, value string) error {
	switch name {
	case "slippageBps":
		v, err := parseFloat(value)
		if err != nil {
			return fmt.Errorf("%w: %q for %s", ErrSweepAxisValue, value, name)
		}
		req.SlippageBps = v
	case "commissionBps":
		v, err := parseFloat(value)
		if err != nil {
			return fmt.Errorf("%w: %q for %s", ErrSweepAxisValue, value, name)
		}
		req.CommissionBps = v
	case "maxOrdersPerDay":
		v, err := parseInt(value)
		if err != nil || v < 0 {
			return fmt.Errorf("%w: %q for %s", ErrSweepAxisValue, value, name)
		}
		req.MaxOrdersPerDay = v
	case "initialCash":
		v, err := parseFloat(value)
		if err != nil || v <= 0 {
			return fmt.Errorf("%w: %q for %s", ErrSweepAxisValue, value, name)
		}
		req.InitialCash = v
	case "engineKind":
		eng := strings.ToLower(strings.TrimSpace(value))
		switch eng {
		case "fallback", "llm", "llm-debate":
			req.EngineKind = eng
		default:
			return fmt.Errorf("%w: %q for %s (allowed: fallback/llm/llm-debate)", ErrSweepAxisValue, value, name)
		}
	default:
		return fmt.Errorf("%w: %q", ErrSweepAxisUnknown, name)
	}
	return nil
}

func parseFloat(s string) (float64, error) {
	var v float64
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%f", &v); err != nil {
		return 0, err
	}
	return v, nil
}

func parseInt(s string) (int, error) {
	var v int
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &v); err != nil {
		return 0, err
	}
	return v, nil
}
