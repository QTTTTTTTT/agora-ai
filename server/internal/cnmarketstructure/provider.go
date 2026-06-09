// Package cnmarketstructure exposes A-share-specific intraday market
// structure data: 涨停板状态 (limit-up seal, reopen count, seal amount,
// consecutive limit-up streak), 龙虎榜 (Dragon-Tiger list seats),
// 北向资金 (Northbound capital net flow), and 大盘活跃度 (limit-up /
// limit-down / fried-board counts at the market level).
//
// None of this maps cleanly onto the existing OHLC / fundamental /
// sentiment providers — those are designed for "give me a quote /
// give me a metric" semantics whereas the tactic agents need a
// composite intraday snapshot. So we keep a separate package with
// its own Provider interface and provider chain.
//
// Provider implementations live in provider_akshare.go (Phase 3).
// In Phase 0 we only ship the interface + types so internal/agent
// can compile against them.
package cnmarketstructure

import (
	"context"
	"errors"
	"time"
)

// ErrNoData signals that no provider returned data for the
// requested symbol/scope. Soft signal — callers (the tactic agent)
// should treat it as "SKIP with reason=data_unavailable", not as a
// hard error.
var ErrNoData = errors.New("cnmarketstructure: no data")

// ErrNotConfigured signals that no provider is wired (e.g. the
// service is running without MCP_AKSHARE_URL). Distinct from
// ErrNoData so operators can detect a misconfigured deployment.
var ErrNotConfigured = errors.New("cnmarketstructure: no provider configured")

// IntradaySnapshot is the per-symbol structural view a tactic
// agent needs to evaluate must-have / red-line conditions. Every
// optional field is a pointer so "field not reported" is
// distinguishable from "field reported as zero" — the difference
// matters for thresholds like SealAmountYi < 0.5 (cannot evaluate
// vs. failing the check).
type IntradaySnapshot struct {
	Symbol               string
	AsOf                 time.Time
	LimitUpToday         bool
	LimitUpTime          time.Time
	LimitUpReopenCount   int     // 炸板次数 (0 = sealed cleanly)
	SealAmountYi         float64 // 封单金额 (亿元)
	SealToFloatCapRatio  float64 // 封单 / 流通市值
	ConsecutiveLimitUps  int     // 连板数
	TurnoverRatePct      float64 // 换手率
	VolumeRatio          float64 // 量比
	UpperShadowPct       float64 // 上影线占比
	PullbackFromHighPct  float64 // 回撤幅度
	DistanceToMA10Pct    float64
	DistanceToMA20Pct    float64
	DistanceToMA60Pct    float64
	NorthboundNetInflow  float64 // 当日北向资金净流入 (元)
	IsST                 bool    // ST / *ST / 退市风险警示
	FloatMarketCapYi     float64 // 流通市值 (亿元)
	DailyGainPct         float64 // 当日涨跌幅
	OpenGapPct           float64 // 开盘缺口 vs. 昨收
	SectorName           string  // 所属板块 (用于板块强度过滤)
	Source               string  // 提供方 Name() — 用于审计
}

// DragonTigerEntry is one 龙虎榜 row. A symbol can have multiple
// entries on a single day if it hits more than one billboard rule.
type DragonTigerEntry struct {
	Date    time.Time
	Symbol  string
	Reason  string     // 上榜原因 (eg "日涨幅偏离值达到7%")
	Seats   []SeatInfo // 买卖席位
	Source  string
}

// SeatInfo is one buy / sell seat on the 龙虎榜.
type SeatInfo struct {
	Name      string  // 席位名称 (eg "中信证券上海溧阳路")
	Tag       string  // 已识别的游资标签 (eg "章盟主"); empty when unknown
	BuyYuan   float64 // 买入金额 (元)
	SellYuan  float64 // 卖出金额 (元)
	NetYuan   float64 // 净买入 = Buy - Sell
}

// MarketRegime is the cross-market snapshot a tactic agent uses
// to decide whether the trading-day environment allows its play.
// All counts are TODAY's intraday count (live during session,
// frozen at close).
type MarketRegime struct {
	AsOf                   time.Time
	LimitUpCount           int     // 涨停家数
	LimitDownCount         int     // 跌停家数
	FriedBoardCount        int     // 炸板家数
	FriedBoardRatePct      float64 // 炸板率 = 炸板 / (涨停 + 炸板)
	ShanghaiIndexChangePct float64 // 上证指数日内涨跌
	SentimentIndex         float64 // 0-100, 综合活跃度
	Source                 string
}

// SectorStrength carries today's per-sector ranking. Tactic
// agents use this to evaluate "属于当日涨幅榜前 3 的强势板块"
// type conditions.
type SectorStrength struct {
	SectorName     string
	ChangePct      float64
	RankToday      int  // 1 = top sector by today's gain
	LimitUpCount   int  // 涨停家数 in this sector
	NetInflowYuan  float64
}

// Provider is the per-source adapter shape. Implementations
// (akshare in Phase 3.2; optional tushare later) live in
// provider_*.go files alongside this one.
type Provider interface {
	// Name is a short identifier for logging / health metrics
	// (eg "akshare", "tushare").
	Name() string

	// FetchIntraday returns the per-symbol structural snapshot
	// for `symbol` as of the current trading session.
	FetchIntraday(ctx context.Context, symbol string) (*IntradaySnapshot, error)

	// FetchDragonTiger returns 龙虎榜 entries for `symbol` over
	// the last `lookbackDays` calendar days. Empty slice + nil
	// error means "no billboard appearance" — distinct from
	// ErrNoData ("upstream didn't answer").
	FetchDragonTiger(ctx context.Context, symbol string, lookbackDays int) ([]DragonTigerEntry, error)

	// FetchMarketRegime returns today's cross-market activity
	// snapshot. Cheap to call (single upstream row).
	FetchMarketRegime(ctx context.Context) (*MarketRegime, error)

	// FetchSectorStrength returns today's per-sector strength
	// ranking, ordered by ChangePct DESC. Capped at top N.
	FetchSectorStrength(ctx context.Context, topN int) ([]SectorStrength, error)
}
