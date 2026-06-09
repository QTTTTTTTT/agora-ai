// Package cnintraday is the Stage-5 A-share intraday signal
// engine. The product story (from the dual-track plan):
//
//	"本地服务器 (无外网暴露) → 分钟数据流 → 5 类因子 → 规则引擎
//	 → 飞书 webhook → 手机 → 人工下单"
//
// Differences from the US-equity SaaS track:
//
//   - This stack is SELF-HOSTED, no public web surface, no
//     payment integration. The HTTP endpoints we expose are for
//     the OPERATOR (the trader) to dry-run signals / verify
//     factor calculations.
//   - A-share market microstructure is different:
//       * T+1 settlement (can't sell same-day buys)
//       * ±10% daily price limit (ST: ±5%, ChiNext/STAR: ±20%)
//       * Discrete 100-share lots
//       * 0.1% stamp tax on sells
//   - Signals are time-of-day filtered: 9:30-9:35 open auction
//     and 14:55-15:00 close auction are excluded.
//   - The decision artefact is a NOTIFICATION ("BUY AAPL @
//     reasons / target / stop") not an order — the trader keys
//     it into their broker app.
//
// This file holds the core domain types. Implementation files:
//
//   - factor.go   — 5 intraday factors (price-only computation
//                   plus volume + order-book proxies)
//   - rules.go    — rule engine that converts factor scores
//                   into TradeSignal events
//   - feishu.go   — webhook client + signal formatter
//   - simulator.go — minute-bar in-process simulator for unit
//                   tests; NOT a production backtester (that's
//                   what rqalpha is for; we shell out to the
//                   Python script in cmd/rqalpha-runner)
package cnintraday

import (
	"time"
)

// Market identifies the A-share market segment a symbol trades
// on. Determines the daily price-limit (±10% / ±20% / ±5%) and
// the minimum lot size (100 shares for all segments).
type Market string

const (
	MarketMainBoard Market = "main_board"  // 主板, ±10%
	MarketChinext   Market = "chinext"     // 创业板, ±20%
	MarketSTAR      Market = "star"        // 科创板, ±20%
	MarketST        Market = "st"          // ST 股, ±5%
	MarketBSE       Market = "bse"         // 北交所, ±30%
)

// SymbolInfo is the per-symbol metadata the engine needs to apply
// market-specific rules (price limit, sector membership).
type SymbolInfo struct {
	Symbol  string `json:"symbol"`  // canonical form: "000300.SH" / "300750.SZ"
	Name    string `json:"name"`    // 海康威视
	Market  Market `json:"market"`
	Sector  string `json:"sector,omitempty"` // 申万一级行业, e.g. "电子"
}

// PriceLimit returns the ±limit fraction for the symbol's market.
// e.g. MarketMainBoard → 0.10 means yesterday's close ± 10%.
func (i SymbolInfo) PriceLimit() float64 {
	switch i.Market {
	case MarketMainBoard:
		return 0.10
	case MarketChinext, MarketSTAR:
		return 0.20
	case MarketST:
		return 0.05
	case MarketBSE:
		return 0.30
	}
	return 0.10
}

// MinLot returns the minimum buy-side lot size. All A-share
// segments use 100 shares for buys; sells can be any quantity.
func (i SymbolInfo) MinLot() int { return 100 }

// MinuteBar is one minute's OHLC observation for one symbol.
// Volume + Amount let the engine compute resampled stats without
// re-fetching. BidAskRatio + BigOrderNet are optional snapshots
// at the bar's close — present when the data provider exposes
// the L1 order book (baostock doesn't; akshare does via 行情接口).
type MinuteBar struct {
	Symbol         string    `json:"symbol"`
	Timestamp      time.Time `json:"timestamp"` // bar OPEN time (e.g. 09:30, 09:31, ...)
	Open           float64   `json:"open"`
	High           float64   `json:"high"`
	Low            float64   `json:"low"`
	Close          float64   `json:"close"`
	Volume         float64   `json:"volume"`  // in shares
	Amount         float64   `json:"amount"`  // 成交额, in CNY
	BidAskRatio    float64   `json:"bidAskRatio,omitempty"`    // 委买/委卖, snapshot at bar close
	BigOrderNet    float64   `json:"bigOrderNet,omitempty"`    // 大单净流入 (CNY), bar-only
}

// MinuteWindow is the engine's "last N bars" workspace for one
// symbol. The signal generator inspects the tail of this slice;
// the data layer is responsible for pruning to a max length so
// the in-process memory stays bounded.
type MinuteWindow struct {
	Symbol string
	Info   SymbolInfo
	Bars   []MinuteBar // ascending by Timestamp
}

// TradeSignal is the engine's output: a structured notification
// the operator forwards to their broker app. Mirrors the schema
// in the user's plan (Section 3.4):
//
//   - Type:               BUY / ADD / SELL / WARNING
//   - Confidence:         0.0-1.0
//   - SuggestedPosition:  fraction of total NAV to allocate
//   - TargetPrice / StopLoss: derived from intraday volatility
//   - Reasons:            human-readable why-list (rendered into
//                         the 飞书 card)
//   - RiskWarnings:       gating cautions (limit-up too close,
//                         ST stock, etc.)
type TradeSignal struct {
	Timestamp         time.Time   `json:"timestamp"`
	Symbol            string      `json:"symbol"`
	Name              string      `json:"name"`
	Type              SignalType  `json:"type"`
	Price             float64     `json:"price"`
	Confidence        float64     `json:"confidence"`
	SuggestedPosition float64     `json:"suggestedPosition"`
	TargetPrice       float64     `json:"targetPrice"`
	StopLoss          float64     `json:"stopLoss"`
	Reasons           []string    `json:"reasons"`
	RiskWarnings      []string    `json:"riskWarnings"`
	FactorScores      FactorTuple `json:"factorScores"`
}

type SignalType string

const (
	SignalBuy     SignalType = "BUY"
	SignalAdd     SignalType = "ADD"
	SignalSell    SignalType = "SELL"
	SignalWarning SignalType = "WARNING"
)

// FactorTuple is the per-symbol factor cross-section at signal
// emission time. The frontend renders this as a horizontal
// "evidence bar" so the operator can sanity-check which factor
// triggered.
type FactorTuple struct {
	Breakout         float64 `json:"breakout"`         // 突破前 60min 高点 (z-score)
	VolumeSurge      float64 `json:"volumeSurge"`      // 当前量 / 5日均量
	BigInflow        float64 `json:"bigInflow"`        // 大单净流入 (本身就是 5min 累计 CNY)
	OrderImbalance   float64 `json:"orderImbalance"`   // 委买/委卖
	SectorRank       float64 `json:"sectorRank"`       // 个股在板块内排名百分位
}

// MinuteProvider is the seam to a real-time minute data feed.
// The MVP ships TWO implementations:
//
//   - LiveBaostockProvider:    real baostock pull, used in
//                              production. NOT in this package
//                              (lives in cmd/cnintraday-runner
//                              so the unit tests can stay pure).
//   - FixtureMinuteProvider:   replays a deterministic minute
//                              series; used by tests + the
//                              dry-run HTTP endpoint.
type MinuteProvider interface {
	// LatestWindow returns the most recent N bars (newest last)
	// for the given symbol as of asOf. nil + nil = no data yet
	// (e.g. pre-market, or symbol not in coverage).
	LatestWindow(symbol string, asOf time.Time, lookbackMinutes int) (*MinuteWindow, error)
}

// SymbolDirectory maps symbol → SymbolInfo (for price-limit
// lookups). MVP ships a tiny in-memory implementation; production
// can swap in a DB-backed version.
type SymbolDirectory interface {
	Lookup(symbol string) (SymbolInfo, bool)
}

// StaticDirectory is the trivial in-memory directory used by
// tests + dry-run endpoint.
type StaticDirectory struct {
	bySymbol map[string]SymbolInfo
}

func NewStaticDirectory(entries ...SymbolInfo) *StaticDirectory {
	d := &StaticDirectory{bySymbol: make(map[string]SymbolInfo, len(entries))}
	for _, e := range entries {
		d.bySymbol[e.Symbol] = e
	}
	return d
}

func (d *StaticDirectory) Lookup(symbol string) (SymbolInfo, bool) {
	if d == nil {
		return SymbolInfo{}, false
	}
	v, ok := d.bySymbol[symbol]
	return v, ok
}
