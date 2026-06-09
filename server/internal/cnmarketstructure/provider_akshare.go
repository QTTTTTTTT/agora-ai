// provider_akshare.go — akshare-MCP backed implementation of
// cnmarketstructure.Provider. Mirrors the surface area the tactic
// agents need:
//
//   * IntradaySnapshot is composed from three akshare endpoints:
//       - stock_zh_a_spot_em      (real-time spot: gain%, turnover, MA)
//       - stock_zt_pool_em        (today's limit-up pool: seal amount, reopens)
//       - stock_hsgt_north_net_flow_in (northbound flow, optional)
//
//   * DragonTigerEntry comes from stock_lhb_detail_em.
//
//   * MarketRegime aggregates stock_market_activity_legu (overall
//     limit-up/down counts + fried-board rate) plus
//     stock_zh_index_spot_em scoped to "上证指数".
//
//   * SectorStrength comes from stock_board_concept_name_em sorted
//     by today's change.
//
// Different akshare-MCP forks ship the data under slightly
// different paths; we try a small candidate list per call before
// giving up with ErrNoData.

package cnmarketstructure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// AkshareProvider talks to a self-hosted akshare-MCP over HTTP.
type AkshareProvider struct {
	HTTPClient *http.Client
	// BaseURL is the akshare-MCP root URL. Empty disables this
	// provider entirely.
	BaseURL string
}

// Name implements Provider.
func (p *AkshareProvider) Name() string { return "akshare" }

func (p *AkshareProvider) client() *http.Client {
	if p.HTTPClient != nil {
		return p.HTTPClient
	}
	return &http.Client{Timeout: 8 * time.Second}
}

// FetchIntraday implements Provider.
func (p *AkshareProvider) FetchIntraday(ctx context.Context, symbol string) (*IntradaySnapshot, error) {
	if strings.TrimSpace(p.BaseURL) == "" || strings.TrimSpace(symbol) == "" {
		return nil, ErrNoData
	}
	now := time.Now()
	snap := &IntradaySnapshot{Symbol: symbol, AsOf: now}

	spot, err := p.fetchSpotRow(ctx, symbol)
	if err != nil && err != ErrNoData {
		return nil, err
	}
	if spot != nil {
		snap.DailyGainPct = akFloat(spot, "change_pct", "涨跌幅", "pct_chg")
		snap.TurnoverRatePct = akFloat(spot, "turnover_rate", "换手率")
		snap.VolumeRatio = akFloat(spot, "volume_ratio", "量比")
		snap.FloatMarketCapYi = akFloat(spot, "circ_mv_yi", "流通市值")
		if snap.FloatMarketCapYi == 0 {
			// Some MCPs return 流通市值 in yuan; normalise to 亿.
			if raw := akFloat(spot, "circ_market_cap", "circulation_market_value"); raw > 0 {
				snap.FloatMarketCapYi = raw / 1e8
			}
		}
		snap.OpenGapPct = akFloat(spot, "open_gap_pct", "开盘缺口")
		snap.UpperShadowPct = akFloat(spot, "upper_shadow_pct", "上影线占比")
		snap.PullbackFromHighPct = akFloat(spot, "pullback_from_high_pct")
		snap.DistanceToMA10Pct = akFloat(spot, "dist_ma10_pct", "MA10乖离")
		snap.DistanceToMA20Pct = akFloat(spot, "dist_ma20_pct", "MA20乖离")
		snap.DistanceToMA60Pct = akFloat(spot, "dist_ma60_pct", "MA60乖离")
		if sector, ok := spot["sector"].(string); ok {
			snap.SectorName = sector
		} else if sector, ok := spot["所属行业"].(string); ok {
			snap.SectorName = sector
		}
		if name, ok := spot["name"].(string); ok {
			snap.IsST = strings.HasPrefix(strings.ToUpper(name), "ST") || strings.HasPrefix(strings.ToUpper(name), "*ST")
		}
	}

	pool, err := p.fetchLimitUpPoolRow(ctx, symbol)
	if err != nil && err != ErrNoData {
		return nil, err
	}
	if pool != nil {
		snap.LimitUpToday = true
		snap.SealAmountYi = akFloat(pool, "seal_amount_yi", "封板资金", "封板金额")
		if snap.SealAmountYi == 0 {
			if raw := akFloat(pool, "seal_amount", "封单金额"); raw > 0 {
				snap.SealAmountYi = raw / 1e8
			}
		}
		if snap.FloatMarketCapYi > 0 && snap.SealAmountYi > 0 {
			snap.SealToFloatCapRatio = snap.SealAmountYi / snap.FloatMarketCapYi
		}
		snap.LimitUpReopenCount = akInt(pool, "reopen_count", "炸板次数")
		snap.ConsecutiveLimitUps = akInt(pool, "consecutive_count", "连板数")
		if ts, ok := pool["limit_up_time"].(string); ok {
			if t, err := time.Parse("15:04:05", ts); err == nil {
				snap.LimitUpTime = time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), t.Second(), 0, now.Location())
			}
		}
	}

	if northbound, err := p.fetchNorthboundNetFlow(ctx); err == nil {
		snap.NorthboundNetInflow = northbound
	}

	if spot == nil && pool == nil {
		return nil, ErrNoData
	}
	return snap, nil
}

func (p *AkshareProvider) fetchSpotRow(ctx context.Context, symbol string) (map[string]any, error) {
	candidates := []string{"/api/stock_spot", "/stock_spot", "/api/spot", "/spot"}
	for _, path := range candidates {
		endpoint, err := p.buildURL(path, map[string]string{"symbol": symbol})
		if err != nil {
			continue
		}
		row, err := p.getJSONObject(ctx, endpoint, symbol)
		if err == nil && row != nil {
			return row, nil
		}
		if errors.Is(err, ErrUpstreamThrottled) {
			// Throttle is authoritative — stop trying alternates.
			return nil, err
		}
	}
	return nil, ErrNoData
}

func (p *AkshareProvider) fetchLimitUpPoolRow(ctx context.Context, symbol string) (map[string]any, error) {
	candidates := []string{"/api/limit_up_pool", "/limit_up_pool", "/api/zt_pool", "/zt_pool"}
	for _, path := range candidates {
		endpoint, err := p.buildURL(path, map[string]string{"symbol": symbol})
		if err != nil {
			continue
		}
		row, err := p.getJSONObject(ctx, endpoint, symbol)
		if err == nil && row != nil {
			return row, nil
		}
		if errors.Is(err, ErrUpstreamThrottled) {
			return nil, err
		}
	}
	return nil, ErrNoData
}

func (p *AkshareProvider) fetchNorthboundNetFlow(ctx context.Context) (float64, error) {
	candidates := []string{"/api/northbound_net_flow", "/northbound_net_flow", "/api/hsgt_north", "/hsgt_north"}
	for _, path := range candidates {
		endpoint, err := p.buildURL(path, nil)
		if err != nil {
			continue
		}
		row, err := p.getJSONObject(ctx, endpoint, "")
		if err == nil && row != nil {
			if v := akFloat(row, "net_flow", "净流入", "today", "today_net_flow"); v != 0 {
				return v, nil
			}
		}
		if errors.Is(err, ErrUpstreamThrottled) {
			return 0, err
		}
	}
	return 0, ErrNoData
}

// FetchDragonTiger implements Provider.
func (p *AkshareProvider) FetchDragonTiger(ctx context.Context, symbol string, lookbackDays int) ([]DragonTigerEntry, error) {
	if strings.TrimSpace(p.BaseURL) == "" || strings.TrimSpace(symbol) == "" {
		return nil, ErrNoData
	}
	if lookbackDays <= 0 {
		lookbackDays = 30
	}
	candidates := []string{"/api/lhb_detail", "/lhb_detail", "/api/longhubang", "/longhubang"}
	for _, path := range candidates {
		endpoint, err := p.buildURL(path, map[string]string{
			"symbol":   symbol,
			"lookback": strconv.Itoa(lookbackDays),
		})
		if err != nil {
			continue
		}
		rows, err := p.getJSONArray(ctx, endpoint)
		if err != nil || len(rows) == 0 {
			continue
		}
		out := make([]DragonTigerEntry, 0, len(rows))
		for _, row := range rows {
			entry := DragonTigerEntry{
				Symbol: symbol,
				Source: p.Name(),
			}
			if s, ok := row["date"].(string); ok {
				if t, err := time.Parse("2006-01-02", s); err == nil {
					entry.Date = t
				}
			}
			if r, ok := row["reason"].(string); ok {
				entry.Reason = r
			}
			if seats, ok := row["seats"].([]any); ok {
				for _, s := range seats {
					m, ok := s.(map[string]any)
					if !ok {
						continue
					}
					entry.Seats = append(entry.Seats, SeatInfo{
						Name:     asString(m, "name", "席位"),
						Tag:      asString(m, "tag", "标签"),
						BuyYuan:  akFloat(m, "buy", "买入金额"),
						SellYuan: akFloat(m, "sell", "卖出金额"),
						NetYuan:  akFloat(m, "net", "净买入"),
					})
				}
			}
			out = append(out, entry)
		}
		// Sort newest first.
		sort.Slice(out, func(i, j int) bool { return out[i].Date.After(out[j].Date) })
		return out, nil
	}
	return nil, ErrNoData
}

// FetchMarketRegime implements Provider.
func (p *AkshareProvider) FetchMarketRegime(ctx context.Context) (*MarketRegime, error) {
	if strings.TrimSpace(p.BaseURL) == "" {
		return nil, ErrNotConfigured
	}
	candidates := []string{"/api/market_activity", "/market_activity", "/api/market_regime", "/market_regime"}
	for _, path := range candidates {
		endpoint, err := p.buildURL(path, nil)
		if err != nil {
			continue
		}
		row, err := p.getJSONObject(ctx, endpoint, "")
		if err != nil || row == nil {
			continue
		}
		regime := &MarketRegime{
			AsOf:            time.Now(),
			LimitUpCount:    akInt(row, "limit_up", "涨停"),
			LimitDownCount:  akInt(row, "limit_down", "跌停"),
			FriedBoardCount: akInt(row, "fried_board", "炸板"),
		}
		if regime.LimitUpCount+regime.FriedBoardCount > 0 {
			regime.FriedBoardRatePct = float64(regime.FriedBoardCount) /
				float64(regime.LimitUpCount+regime.FriedBoardCount) * 100
		}
		regime.ShanghaiIndexChangePct = akFloat(row, "shanghai_change_pct", "上证涨跌幅", "sh_change_pct")
		regime.SentimentIndex = akFloat(row, "sentiment_index", "情绪指数")
		return regime, nil
	}
	return nil, ErrNoData
}

// FetchSectorStrength implements Provider.
func (p *AkshareProvider) FetchSectorStrength(ctx context.Context, topN int) ([]SectorStrength, error) {
	if strings.TrimSpace(p.BaseURL) == "" {
		return nil, ErrNotConfigured
	}
	if topN <= 0 {
		topN = 20
	}
	candidates := []string{"/api/sector_strength", "/sector_strength", "/api/board_concept_rank", "/board_concept_rank"}
	for _, path := range candidates {
		endpoint, err := p.buildURL(path, map[string]string{"top_n": strconv.Itoa(topN)})
		if err != nil {
			continue
		}
		rows, err := p.getJSONArray(ctx, endpoint)
		if err != nil || len(rows) == 0 {
			continue
		}
		out := make([]SectorStrength, 0, len(rows))
		for i, row := range rows {
			s := SectorStrength{
				SectorName:    asString(row, "name", "板块名称"),
				ChangePct:     akFloat(row, "change_pct", "涨跌幅"),
				LimitUpCount:  akInt(row, "limit_up_count", "涨停家数"),
				NetInflowYuan: akFloat(row, "net_inflow", "主力净流入"),
				RankToday:     akInt(row, "rank", "排名"),
			}
			if s.RankToday == 0 {
				s.RankToday = i + 1
			}
			out = append(out, s)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].ChangePct > out[j].ChangePct })
		if topN > 0 && len(out) > topN {
			out = out[:topN]
		}
		return out, nil
	}
	return nil, ErrNoData
}

// --- shared HTTP helpers ----------------------------------------

func (p *AkshareProvider) buildURL(path string, params map[string]string) (string, error) {
	u, err := url.Parse(strings.TrimRight(p.BaseURL, "/") + path)
	if err != nil {
		return "", fmt.Errorf("akshare cnstruct: build url: %w", err)
	}
	if len(params) > 0 {
		q := u.Query()
		for k, v := range params {
			q.Set(k, v)
		}
		u.RawQuery = q.Encode()
	}
	return u.String(), nil
}

func (p *AkshareProvider) getJSONObject(ctx context.Context, endpoint, symbol string) (map[string]any, error) {
	body, err := p.doGET(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	body = []byte(strings.TrimSpace(string(body)))
	if len(body) == 0 {
		return nil, ErrNoData
	}
	switch body[0] {
	case '{':
		var m map[string]any
		if err := json.Unmarshal(body, &m); err != nil {
			return nil, err
		}
		if data, ok := m["data"].(map[string]any); ok {
			return data, nil
		}
		// Single-row responses with the symbol nested:
		if symbol != "" {
			if entry, ok := m[symbol].(map[string]any); ok {
				return entry, nil
			}
		}
		return m, nil
	case '[':
		var arr []map[string]any
		if err := json.Unmarshal(body, &arr); err != nil {
			return nil, err
		}
		if len(arr) == 0 {
			return nil, ErrNoData
		}
		if symbol != "" {
			for _, row := range arr {
				if s := asString(row, "symbol", "code"); strings.EqualFold(s, symbol) {
					return row, nil
				}
			}
		}
		return arr[0], nil
	}
	return nil, ErrNoData
}

func (p *AkshareProvider) getJSONArray(ctx context.Context, endpoint string) ([]map[string]any, error) {
	body, err := p.doGET(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	body = []byte(strings.TrimSpace(string(body)))
	if len(body) == 0 {
		return nil, ErrNoData
	}
	switch body[0] {
	case '[':
		var arr []map[string]any
		if err := json.Unmarshal(body, &arr); err != nil {
			return nil, err
		}
		return arr, nil
	case '{':
		var m map[string]any
		if err := json.Unmarshal(body, &m); err != nil {
			return nil, err
		}
		if data, ok := m["data"].([]any); ok {
			out := make([]map[string]any, 0, len(data))
			for _, item := range data {
				if row, ok := item.(map[string]any); ok {
					out = append(out, row)
				}
			}
			return out, nil
		}
	}
	return nil, ErrNoData
}

func (p *AkshareProvider) doGET(ctx context.Context, endpoint string) ([]byte, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Accept", "application/json")
	resp, err := p.client().Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("akshare cnstruct: http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNoData
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusUnavailableForLegalReasons {
		return nil, fmt.Errorf("%w: http %d", ErrUpstreamThrottled, resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("akshare cnstruct: http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("akshare cnstruct: read: %w", err)
	}
	return body, nil
}

// --- akshare-flavoured tolerant value parsing -------------------

func akFloat(row map[string]any, keys ...string) float64 {
	for _, key := range keys {
		v, ok := row[key]
		if !ok {
			continue
		}
		switch t := v.(type) {
		case float64:
			return t
		case int:
			return float64(t)
		case int64:
			return float64(t)
		case json.Number:
			if f, err := t.Float64(); err == nil {
				return f
			}
		case string:
			s := strings.TrimSpace(t)
			s = strings.TrimSuffix(s, "%")
			s = strings.TrimSuffix(s, "亿")
			if f, err := strconv.ParseFloat(s, 64); err == nil {
				return f
			}
		}
	}
	return 0
}

func akInt(row map[string]any, keys ...string) int {
	for _, key := range keys {
		v, ok := row[key]
		if !ok {
			continue
		}
		switch t := v.(type) {
		case float64:
			return int(t)
		case int:
			return t
		case int64:
			return int(t)
		case json.Number:
			if i, err := t.Int64(); err == nil {
				return int(i)
			}
		case string:
			if i, err := strconv.Atoi(strings.TrimSpace(t)); err == nil {
				return i
			}
		}
	}
	return 0
}

func asString(row map[string]any, keys ...string) string {
	for _, key := range keys {
		v, ok := row[key]
		if !ok {
			continue
		}
		switch t := v.(type) {
		case string:
			return strings.TrimSpace(t)
		case fmt.Stringer:
			return t.String()
		}
	}
	return ""
}
