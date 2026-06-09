// tactic_types.go — shared type definitions for the A-share
// short-term tactic agents. The agent body + panel runner live in
// tactic_agent.go / tactic_panel.go.

package agent

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/fundai/server/internal/cnmarketstructure"
)

// TacticInput is what one tactic agent receives. Mirrors the spirit
// of MasterInput but carries A-share-specific structure data fetched
// from cnmarketstructure.Provider — the wiring layer is responsible
// for populating Intraday / Regime / Sectors before calling Run.
type TacticInput struct {
	Symbol      string
	// Name is the issuer's short Chinese / English name (e.g.
	// "德科立"). Optional — when present the tactic prompt
	// includes it so the LLM reasons by company name rather
	// than only by ticker code.
	Name        string
	Market      string
	AsOf        time.Time
	PriceLast   float64
	PriceChange float64
	Notes       string

	// Intraday is the per-symbol snapshot (limit-up state, seal
	// amount, MA distances, turnover, …). nil-safe: agents that
	// need structural data will SKIP with reason data_unavailable.
	Intraday *cnmarketstructure.IntradaySnapshot
	// Regime is today's cross-market activity (limit-up count,
	// fried-board rate, Shanghai index intraday change). Agents
	// gate their must_have / red_lines off these values.
	Regime *cnmarketstructure.MarketRegime
	// Sectors is today's sector ranking, used by tactics with a
	// "属于当日涨幅榜前 N 的强势板块" condition. Optional.
	Sectors []cnmarketstructure.SectorStrength
	// HardRiskFailures is the pre-computed list of ST / monitoring
	// flags the wiring layer pulls from the existing risk
	// package. Non-empty triggers an automatic SKIP for every
	// tactic.
	HardRiskFailures []string
}

// TacticReport is the per-tactic output. Distinct from MasterReport
// because tactics output entry / stop / target prices and an
// expected holding window rather than a verdict on long-term value.
type TacticReport struct {
	TacticKey           string
	TacticNameZh        string
	TacticNameEn        string
	Symbol              string
	// SymbolName mirrors MasterReport.SymbolName — issuer's short
	// name, optional, empty when unknown.
	SymbolName          string
	AsOf                time.Time
	GeneratedAt         time.Time
	Verdict             string
	Confidence          int
	Thesis              string
	EntryPriceLow       *float64
	EntryPriceHigh      *float64
	StopLossPrice       *float64
	TargetT1            *float64
	TargetT3            *float64
	ExpectedHoldingDays *int
	Score               float64
	KeyReasons          []string
	KeyRisks            []string
	RedLinesHit         []string
	MarketRegimePass    bool
	MarketRegimeReason  string
}

// TacticPanelReport bundles every per-tactic report plus an aggregate.
type TacticPanelReport struct {
	Symbol      string
	AsOf        time.Time
	GeneratedAt time.Time
	Reports     []TacticReport
	Aggregate   TacticAggregateView
}

// TacticAggregateView is the panel-level rollup. The advisor.Service
// uses Verdict / Confidence to populate the headline numbers when a
// preset is tactic-only.
type TacticAggregateView struct {
	Verdict    string
	Confidence int
	Consensus  float64

	BuyCount  int
	WaitCount int
	SkipCount int
}

// ErrTacticNotReady is returned when the panel is asked to run
// without any wired agents.
var ErrTacticNotReady = errors.New("agent: tactic panel has no agents")

// helper used by both the agent and panel — kept here so tactic_types.go
// is the single source of "import context/errors/strings/time" for
// the tactic surface.
var _ = func() context.Context { _ = strings.TrimSpace; _ = time.Now; return context.Background() }
