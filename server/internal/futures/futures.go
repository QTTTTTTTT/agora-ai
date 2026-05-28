// Package futures 实现 Sprint 3 / L3 期货模拟盘的最小核心：
//
//   1. continuous contract (滚动连续合约) — 把同一品种的多个到期合约
//      (CL2606, CL2607, CL2608, ...) 按 roll calendar 拼成一根连续
//      价格序列；
//   2. roll calendar — 告诉 portfolio "在 X 日把 CL2606 平掉、开
//      CL2607" 的换月时点；
//   3. mark-to-market valuation — 给定当前持仓 + 当前 mark price，
//      算未实现盈亏；
//   4. variation margin (VM) — 按昨结到今结的差额计提保证金调整；
//   5. funding rate stub — perpetual swap 风格的资金费率影响（按
//      day rate 累计现金，留 hook 给后续真实接 source）。
//
// 设计要点：
//
//  * 我们故意 NOT 引入 broker connectivity / clearinghouse 接口 —
//    平台是模拟盘，broker layer 走平台自带的 PaperBroker。本 pkg
//    给 PaperBroker 喂"今日合约的 mark price + variation 现金"，
//    其他 NAV / position 路径不变。
//  * 连续合约只在 prompt / charting / signal generation 里用 — 实
//    际交易仍下到具体到期合约（front/back 月），交割也走具体合约
//    才有意义。这是行业惯例。
//  * variation margin 计提走 cash 端而不是 position 端 — 保证 NAV
//    路径与现货一致。这样 nav_snapshots 不需要新增 column。
//
// 范围内 / 不在范围内 (Sprint 3 / L3)：
//
//  * 范围内：rule-based roll (volume 或 fixed-N-days-before-expiry)、
//    mark-to-market on closing price、variation margin (=positions ×
//    (todayMark - yesterdayMark) × multiplier)。
//  * 不在范围内（留后续 PR）：真正的清算所 IM 表、tiered margin、
//    physical delivery、basis trading 报告、跨保证金抵销。
package futures

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Contract 是一根具体到期合约。Symbol 是平台内 canonical 名，
// ExpiresOn 是该合约的最后交易日 / 强平日。Multiplier 是 1 张
// 合约对应多少标的（e.g. CME WTI 原油 = 1000 桶）。
type Contract struct {
	Symbol      string    // e.g. CL2606
	Root        string    // e.g. CL  (用于连续合约 / 跨期排序)
	ExpiresOn   time.Time // 最后交易日
	Multiplier  float64
	TickSize    float64 // 最小价格变动
	TickValue   float64 // 一个 tick 对应多少 base currency
	Currency    string
	// MarginInitial 单张合约初始保证金（base currency）。
	MarginInitial float64
}

// Validate 简单一致性检查 — 仅 cover obvious bugs。
func (c Contract) Validate() error {
	if strings.TrimSpace(c.Symbol) == "" {
		return errors.New("futures: contract symbol empty")
	}
	if strings.TrimSpace(c.Root) == "" {
		return errors.New("futures: contract root empty")
	}
	if c.Multiplier <= 0 {
		return fmt.Errorf("futures: contract %s multiplier must be > 0", c.Symbol)
	}
	if c.ExpiresOn.IsZero() {
		return fmt.Errorf("futures: contract %s expiresOn unset", c.Symbol)
	}
	return nil
}

// RollEvent 告诉 portfolio 在 RolledOn 日把 FromSymbol 平掉，
// 开 ToSymbol。模拟盘 PaperBroker 执行这两笔的撮合，资金费 / 滑点
// 在 PaperBroker 里另算 — 本 pkg 不负责。
type RollEvent struct {
	RolledOn   time.Time
	FromSymbol string
	ToSymbol   string
}

// RollPolicy 描述如何选 "前月 → 次月" 的换月时点。
type RollPolicy struct {
	// DaysBeforeExpiry 在合约到期前 N 个日历日触发 roll。
	// 默认 5 — 实际平台普遍用 3-7 之间。
	DaysBeforeExpiry int
}

// DefaultRollPolicy 返回经验默认 — 实盘 5 天前。
func DefaultRollPolicy() RollPolicy {
	return RollPolicy{DaysBeforeExpiry: 5}
}

// BuildRollCalendar 给定 root 下的所有合约 + roll policy，按到期
// 时间排序并产出一串 RollEvent。返回的事件按 RolledOn asc 排序。
// 单合约 series 返回空切片（没有可换的次月）。
func BuildRollCalendar(contracts []Contract, policy RollPolicy) ([]RollEvent, error) {
	if len(contracts) == 0 {
		return nil, nil
	}
	if policy.DaysBeforeExpiry <= 0 {
		policy = DefaultRollPolicy()
	}
	root := ""
	cs := make([]Contract, 0, len(contracts))
	for _, c := range contracts {
		if err := c.Validate(); err != nil {
			return nil, err
		}
		r := strings.ToUpper(strings.TrimSpace(c.Root))
		if root == "" {
			root = r
		} else if root != r {
			return nil, fmt.Errorf("futures: roll calendar mixes roots %q and %q", root, r)
		}
		cs = append(cs, c)
	}
	sort.Slice(cs, func(i, j int) bool {
		return cs[i].ExpiresOn.Before(cs[j].ExpiresOn)
	})
	if len(cs) < 2 {
		return nil, nil
	}
	out := make([]RollEvent, 0, len(cs)-1)
	for i := 0; i < len(cs)-1; i++ {
		front := cs[i]
		back := cs[i+1]
		rollDate := front.ExpiresOn.AddDate(0, 0, -policy.DaysBeforeExpiry)
		out = append(out, RollEvent{
			RolledOn:   rollDate,
			FromSymbol: front.Symbol,
			ToSymbol:   back.Symbol,
		})
	}
	return out, nil
}

// ActiveContract 返回某日的"主力 / 持仓应在的"合约 — i.e. 离当日
// 最近的、尚未触发 roll 的合约。callsite 用它做：
//
//   * continuous-series 拼接 (今日用 ActiveContract 的 mark)；
//   * 模拟开新仓时的合约 mapping。
//
// 找不到合适合约时返回 (Contract{}, false)。
func ActiveContract(contracts []Contract, policy RollPolicy, asOf time.Time) (Contract, bool) {
	if len(contracts) == 0 {
		return Contract{}, false
	}
	if policy.DaysBeforeExpiry <= 0 {
		policy = DefaultRollPolicy()
	}
	cs := make([]Contract, 0, len(contracts))
	for _, c := range contracts {
		if c.Validate() != nil {
			continue
		}
		cs = append(cs, c)
	}
	if len(cs) == 0 {
		return Contract{}, false
	}
	sort.Slice(cs, func(i, j int) bool {
		return cs[i].ExpiresOn.Before(cs[j].ExpiresOn)
	})
	asOf = stripTime(asOf)
	for _, c := range cs {
		rollDate := stripTime(c.ExpiresOn.AddDate(0, 0, -policy.DaysBeforeExpiry))
		if !asOf.Before(rollDate) {
			continue
		}
		return c, true
	}
	// 全部已 roll → 用最后一个合约（极端情况：链断了）。
	return cs[len(cs)-1], true
}

// ContinuousPoint 是连续合约时间序列的一点。Source 是当日实际使用
// 的合约 symbol；调用方画 chart 时可在 Source 切换处画 roll 标记。
type ContinuousPoint struct {
	Date   time.Time
	Symbol string
	Price  float64
}

// BarFetcher 在测试 / 适配 ohlc.Fetcher 时实现。返回的 Price 应是
// 当日 close。Asof 用作"截至日"。
type BarFetcher interface {
	Close(symbol string, day time.Time) (price float64, ok bool)
}

// BuildContinuousSeries 给一段日期范围内逐日 stitch 连续合约。
// 因为每日只取该日 active contract 的 close，所以多日 series 自动
// 跨过 roll boundary（注意 roll 当日会产生跳价 — 这就是行业 "未
// 调整连续合约"；如果要 back-adjusted series 调用方还要做一次差额
// 平移，留给后续 PR）。
//
// 缺数据的日子静默跳过（不报错）— typical 节假日处理。
func BuildContinuousSeries(contracts []Contract, policy RollPolicy, fetcher BarFetcher, from, to time.Time) []ContinuousPoint {
	if len(contracts) == 0 || fetcher == nil {
		return nil
	}
	if to.Before(from) {
		return nil
	}
	from = stripTime(from)
	to = stripTime(to)
	out := make([]ContinuousPoint, 0, dayDiff(from, to)+1)
	for cur := from; !cur.After(to); cur = cur.AddDate(0, 0, 1) {
		ac, ok := ActiveContract(contracts, policy, cur)
		if !ok {
			continue
		}
		price, ok := fetcher.Close(ac.Symbol, cur)
		if !ok || price <= 0 {
			continue
		}
		out = append(out, ContinuousPoint{
			Date:   cur,
			Symbol: ac.Symbol,
			Price:  price,
		})
	}
	return out
}

// MarkPosition 描述持仓的标的合约 + 张数 + 入场 mark 价。Direction
// long = +Qty, short 用负 Qty 编码（与 spot 路径一致，省一个字段）。
type MarkPosition struct {
	Symbol     string
	Qty        float64 // 张数，long 正 / short 负
	EntryMark  float64 // 入场标记价（per unit, not per contract）
}

// MarkSnapshot 是给定 mark prices 后的持仓 valuation。
type MarkSnapshot struct {
	Symbol          string
	Qty             float64
	EntryMark       float64
	CurrentMark     float64
	NotionalValue   float64 // |Qty| * Multiplier * CurrentMark
	UnrealizedPnL   float64 // Qty * Multiplier * (CurrentMark - EntryMark)
	Multiplier      float64
}

// MarkToMarket 按 marks (symbol → 现价) 计算 unrealized PnL。
// 缺 contract / mark 时给出 0 + warning（caller 检查 NotionalValue
// == 0 既可判别）。
func MarkToMarket(positions []MarkPosition, contracts map[string]Contract, marks map[string]float64) []MarkSnapshot {
	if len(positions) == 0 {
		return nil
	}
	out := make([]MarkSnapshot, 0, len(positions))
	for _, p := range positions {
		c, ok := contracts[p.Symbol]
		if !ok || c.Multiplier <= 0 {
			out = append(out, MarkSnapshot{Symbol: p.Symbol, Qty: p.Qty, EntryMark: p.EntryMark})
			continue
		}
		mark, ok := marks[p.Symbol]
		if !ok || mark <= 0 {
			out = append(out, MarkSnapshot{Symbol: p.Symbol, Qty: p.Qty, EntryMark: p.EntryMark, Multiplier: c.Multiplier})
			continue
		}
		notional := absFloat(p.Qty) * c.Multiplier * mark
		upnl := p.Qty * c.Multiplier * (mark - p.EntryMark)
		out = append(out, MarkSnapshot{
			Symbol:        p.Symbol,
			Qty:           p.Qty,
			EntryMark:     p.EntryMark,
			CurrentMark:   mark,
			NotionalValue: notional,
			UnrealizedPnL: upnl,
			Multiplier:    c.Multiplier,
		})
	}
	return out
}

// VariationMargin 是当日按昨结到今结的差额计提的现金调整。Long
// 持仓在今结高于昨结时收 cash，反之付 cash。clearinghouse 行话：
// VM > 0 = credit (account gets credited), VM < 0 = debit。
// 调用方拿到 VM 后 push 到 cash 端，对应 NAV 变动自动正确。
type VariationMargin struct {
	Symbol string
	Qty    float64
	// CashDelta 单位 base currency，可正可负。
	CashDelta float64
}

// ComputeVariationMargin 给定 yesterdayMark + todayMark + positions，
// 算 per-position VM。空 mark / 缺 contract 静默 zero — 调用方应保证
// 数据完整再调用。
func ComputeVariationMargin(positions []MarkPosition, contracts map[string]Contract, yesterdayMarks, todayMarks map[string]float64) []VariationMargin {
	if len(positions) == 0 {
		return nil
	}
	out := make([]VariationMargin, 0, len(positions))
	for _, p := range positions {
		c, ok := contracts[p.Symbol]
		if !ok || c.Multiplier <= 0 {
			out = append(out, VariationMargin{Symbol: p.Symbol, Qty: p.Qty})
			continue
		}
		y, okY := yesterdayMarks[p.Symbol]
		t, okT := todayMarks[p.Symbol]
		if !okY || !okT || y <= 0 || t <= 0 {
			out = append(out, VariationMargin{Symbol: p.Symbol, Qty: p.Qty})
			continue
		}
		delta := p.Qty * c.Multiplier * (t - y)
		out = append(out, VariationMargin{
			Symbol:    p.Symbol,
			Qty:       p.Qty,
			CashDelta: delta,
		})
	}
	return out
}

// FundingAccrual 是 perpetual swap 风格的资金费率结果。Rate 是日利率
// (e.g. 0.0001 = 1bp/day, 8h funding rate 简单聚合)，长仓 rate > 0
// 时付现金给空仓，反之收。
type FundingAccrual struct {
	Symbol    string
	Qty       float64
	CashDelta float64
}

// AccrueFunding — 仅在合约配有 perpetual funding rate 时调用。多数
// 标准期货品种没有 funding（替代品是基差），调用方判定不调即可。
//
// rate 单位：每日浮点（0.0001 = 1bp/day）。signedRate > 0 表示 long
// 付钱给 short。
func AccrueFunding(positions []MarkPosition, contracts map[string]Contract, marks map[string]float64, rates map[string]float64) []FundingAccrual {
	if len(positions) == 0 || len(rates) == 0 {
		return nil
	}
	out := make([]FundingAccrual, 0, len(positions))
	for _, p := range positions {
		c, ok := contracts[p.Symbol]
		if !ok || c.Multiplier <= 0 {
			continue
		}
		rate, okR := rates[p.Symbol]
		mark, okM := marks[p.Symbol]
		if !okR || !okM || mark <= 0 {
			continue
		}
		// long 付（CashDelta < 0）当 rate > 0；short 收。
		cash := -p.Qty * c.Multiplier * mark * rate
		if cash == 0 {
			continue
		}
		out = append(out, FundingAccrual{
			Symbol:    p.Symbol,
			Qty:       p.Qty,
			CashDelta: cash,
		})
	}
	return out
}

func stripTime(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

func dayDiff(a, b time.Time) int {
	return int(b.Sub(a).Hours() / 24)
}

func absFloat(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
