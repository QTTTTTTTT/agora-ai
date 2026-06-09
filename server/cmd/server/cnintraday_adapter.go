package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/cnintraday"
)

// cnIntradayAdapter implements api.CNIntradayService against the
// internal/cnintraday package.
//
// Stateless on purpose — every dry-run request is a pure
// function of the request body. No DB, no live data feed, no
// Feishu webhook (those live in cmd/cnintraday-runner).
type cnIntradayAdapter struct{}

func newCNIntradayAdapter() *cnIntradayAdapter { return &cnIntradayAdapter{} }

func (a *cnIntradayAdapter) DryRunSignal(_ string, input api.CNIntradayDryRunInput) (*api.CNIntradayDryRunResult, error) {
	if a == nil {
		return nil, api.ErrCNIntradayUnconfigured
	}
	if strings.TrimSpace(input.Symbol) == "" {
		return nil, fmt.Errorf("symbol required")
	}
	if len(input.Bars) == 0 {
		return nil, fmt.Errorf("at least one bar required")
	}
	if input.PrevClose <= 0 {
		return nil, fmt.Errorf("prevClose must be > 0")
	}

	market := parseMarket(input.Market)
	info := cnintraday.SymbolInfo{
		Symbol: input.Symbol,
		Name:   input.Name,
		Market: market,
	}

	bars := make([]cnintraday.MinuteBar, 0, len(input.Bars))
	for i, b := range input.Bars {
		ts, err := parseMinuteTimestamp(b.Timestamp)
		if err != nil {
			return nil, fmt.Errorf("bars[%d]: %w", i, err)
		}
		bars = append(bars, cnintraday.MinuteBar{
			Symbol:      input.Symbol,
			Timestamp:   ts,
			Open:        b.Open,
			High:        b.High,
			Low:         b.Low,
			Close:       b.Close,
			Volume:      b.Volume,
			Amount:      b.Amount,
			BidAskRatio: b.BidAskRatio,
			BigOrderNet: b.BigOrderNet,
		})
	}
	window := &cnintraday.MinuteWindow{Symbol: input.Symbol, Info: info, Bars: bars}

	factors := cnintraday.ComputeFactors(window)
	// Patch in the operator-supplied sector rank if provided
	// (engine defaults to 0.5 neutral).
	if input.SectorRank > 0 {
		factors.SectorRank = input.SectorRank
	}

	nowBeijing := bars[len(bars)-1].Timestamp
	if input.NowBeijing != "" {
		if t, err := parseMinuteTimestamp(input.NowBeijing); err == nil {
			nowBeijing = t
		}
	}

	rules := cnintraday.ConservativeRuleSet()
	if strings.EqualFold(input.RuleSet, "aggressive") {
		rules = cnintraday.AggressiveRuleSet()
	}
	signal := cnintraday.Evaluate(cnintraday.EvaluateInput{
		Symbol:     input.Symbol,
		Info:       info,
		PrevClose:  input.PrevClose,
		LastBar:    bars[len(bars)-1],
		Factors:    factors,
		NowBeijing: nowBeijing,
	}, rules)

	result := &api.CNIntradayDryRunResult{
		FactorScores: api.CNIntradayFactorTuple{
			Breakout:       factors.Breakout,
			VolumeSurge:    factors.VolumeSurge,
			BigInflow:      factors.BigInflow,
			OrderImbalance: factors.OrderImbalance,
			SectorRank:     factors.SectorRank,
		},
	}
	if signal != nil {
		result.Signal = &api.CNIntradaySignalView{
			Timestamp:         signal.Timestamp,
			Symbol:            signal.Symbol,
			Name:              signal.Name,
			Type:              string(signal.Type),
			Price:             signal.Price,
			Confidence:        signal.Confidence,
			SuggestedPosition: signal.SuggestedPosition,
			TargetPrice:       signal.TargetPrice,
			StopLoss:          signal.StopLoss,
			Reasons:           signal.Reasons,
			RiskWarnings:      signal.RiskWarnings,
		}
		// Render the Feishu card so the operator sees what the
		// bot would push.
		msg := cnintraday.RenderSignal(signal)
		if zh, ok := msg.Content.Post["zh_cn"]; ok {
			lines := make([]string, 0, len(zh.Content))
			for _, row := range zh.Content {
				if len(row) > 0 {
					lines = append(lines, row[0].Text)
				}
			}
			result.Feishu = &api.CNIntradayFeishuPreview{
				Title: zh.Title,
				Lines: lines,
			}
		}
	}
	return result, nil
}

func parseMarket(s string) cnintraday.Market {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "chinext":
		return cnintraday.MarketChinext
	case "star":
		return cnintraday.MarketSTAR
	case "st":
		return cnintraday.MarketST
	case "bse":
		return cnintraday.MarketBSE
	default:
		return cnintraday.MarketMainBoard
	}
}

func parseMinuteTimestamp(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, fmt.Errorf("timestamp required")
	}
	// Try RFC3339 first.
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		loc = time.FixedZone("CST", 8*3600)
	}
	// Then "YYYY-MM-DD HH:MM" (assumed Beijing).
	if t, err := time.ParseInLocation("2006-01-02 15:04", s, loc); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02 15:04:05", s, loc); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02T15:04", s, loc); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("unrecognised timestamp %q (want RFC3339 or 'YYYY-MM-DD HH:MM')", s)
}
