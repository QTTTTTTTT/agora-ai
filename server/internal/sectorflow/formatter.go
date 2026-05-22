package sectorflow

import (
	"fmt"
	"strings"
)

// FormatForPrompt produces a compact multi-line block summarising
// the sector rotation:
//
//   Top 3 sectors (1d): Technology +1.8%, Real Estate +1.2%, Industrials +0.9%
//   Bottom 3 sectors (1d): Energy -2.3%, Utilities -1.5%, Materials -1.1%
//
// Optionally appends a 5d top/bottom line and main-net-inflow
// numbers (only for A-share where Akshare publishes them).
//
// topN/bottomN are clamped to len(sectors)/2 to avoid duplicating
// rows in degenerate cases.
func (s *Snapshot) FormatForPrompt(topN, bottomN int) string {
	if s == nil || len(s.Sectors) == 0 {
		return ""
	}
	if topN < 0 {
		topN = 0
	}
	if bottomN < 0 {
		bottomN = 0
	}
	half := len(s.Sectors) / 2
	if half == 0 {
		half = 1
	}
	if topN > half {
		topN = half
	}
	if bottomN > half {
		bottomN = half
	}

	lines := []string{}
	if topN > 0 {
		tops := s.Sectors[:topN]
		lines = append(lines, "Top "+itoa(len(tops))+" sectors (1d): "+joinSectorLine(tops, false))
	}
	if bottomN > 0 {
		bots := s.Sectors[len(s.Sectors)-bottomN:]
		lines = append(lines, "Bottom "+itoa(len(bots))+" sectors (1d): "+joinSectorLine(reversed(bots), false))
	}
	if hasReturn5d(s.Sectors) {
		sorted := sortedByReturn5d(s.Sectors)
		if topN > 0 && len(sorted) >= topN {
			lines = append(lines, "Top "+itoa(topN)+" sectors (5d): "+joinSectorLine(sorted[:topN], true))
		}
	}
	if hasNetInflow(s.Sectors) {
		sorted := sortedByInflow(s.Sectors)
		if topN > 0 && len(sorted) >= topN {
			lines = append(lines, "Strongest net inflow: "+joinInflowLine(sorted[:topN]))
		}
		if bottomN > 0 && len(sorted) >= bottomN {
			lines = append(lines, "Weakest net inflow: "+joinInflowLine(reversed(sorted[len(sorted)-bottomN:])))
		}
	}
	return strings.Join(lines, "\n")
}

func joinSectorLine(sectors []Sector, useFiveD bool) string {
	parts := make([]string, 0, len(sectors))
	for _, sec := range sectors {
		ret := sec.Return1d
		if useFiveD {
			ret = sec.Return5d
		}
		parts = append(parts, fmt.Sprintf("%s %+.2f%%", sec.Name, ret*100))
	}
	return strings.Join(parts, ", ")
}

func joinInflowLine(sectors []Sector) string {
	parts := make([]string, 0, len(sectors))
	for _, sec := range sectors {
		parts = append(parts, fmt.Sprintf("%s %s", sec.Name, formatInflow(sec.NetInflow, sec.Currency)))
	}
	return strings.Join(parts, ", ")
}

// formatInflow renders a currency value with K/M/B/T suffixes,
// preserving sign (+/-).
func formatInflow(value float64, currency string) string {
	if value == 0 {
		return "0"
	}
	abs := value
	sign := "+"
	if value < 0 {
		abs = -value
		sign = "-"
	}
	var unit string
	var scaled float64
	switch {
	case abs >= 1e12:
		unit = "T"
		scaled = abs / 1e12
	case abs >= 1e9:
		unit = "B"
		scaled = abs / 1e9
	case abs >= 1e6:
		unit = "M"
		scaled = abs / 1e6
	case abs >= 1e3:
		unit = "K"
		scaled = abs / 1e3
	default:
		unit = ""
		scaled = abs
	}
	out := fmt.Sprintf("%s%.2f%s", sign, scaled, unit)
	if currency = strings.TrimSpace(currency); currency != "" {
		out += " " + strings.ToUpper(currency)
	}
	return out
}

// itoa is a tiny strconv.Itoa wrapper that lets us avoid a strconv
// import in this file (the package is already pulling strconv via
// the akshare parser; just keeping things local & readable).
func itoa(i int) string {
	return fmt.Sprintf("%d", i)
}

func hasReturn5d(sectors []Sector) bool {
	for _, s := range sectors {
		if s.Return5d != 0 {
			return true
		}
	}
	return false
}

func hasNetInflow(sectors []Sector) bool {
	for _, s := range sectors {
		if s.NetInflow != 0 {
			return true
		}
	}
	return false
}

func sortedByReturn5d(sectors []Sector) []Sector {
	out := append([]Sector(nil), sectors...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Return5d > out[j-1].Return5d; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func sortedByInflow(sectors []Sector) []Sector {
	out := append([]Sector(nil), sectors...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].NetInflow > out[j-1].NetInflow; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func reversed(s []Sector) []Sector {
	out := make([]Sector, len(s))
	for i, v := range s {
		out[len(s)-1-i] = v
	}
	return out
}
