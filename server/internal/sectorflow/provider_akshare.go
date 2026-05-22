package sectorflow

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// AkshareSectorProvider talks to a self-hosted akshare-MCP route
// that returns A-share industry money-flow rows. Akshare exposes
// several "concept" / "industry" money-flow endpoints; this
// provider tries a few candidate paths so we work across MCP forks.
//
// Expected response shapes (all tolerated):
//
//   {"data":[{"name":"半导体","change_pct":2.13,"net_inflow":1.5e9}, ...]}
//   [{"name":"白酒","change_pct":-1.1,"net_inflow":-3.2e8}, ...]
//
// Field aliases are tolerated so the parser works against both the
// English and Chinese variants of the akshare endpoint.
type AkshareSectorProvider struct {
	HTTPClient *http.Client
	// BaseURL is the akshare-MCP root URL. Empty disables this
	// provider entirely.
	BaseURL string
	// Markets defaults to {"a_share"}.
	Markets []string
}

// Name implements Provider.
func (p *AkshareSectorProvider) Name() string { return "akshare_sector" }

// Supports implements Provider. Requires BaseURL set.
func (p *AkshareSectorProvider) Supports(market string) bool {
	if strings.TrimSpace(p.BaseURL) == "" {
		return false
	}
	markets := p.Markets
	if len(markets) == 0 {
		markets = []string{"a_share"}
	}
	for _, m := range markets {
		if strings.EqualFold(m, market) {
			return true
		}
	}
	return false
}

// Fetch implements Provider.
func (p *AkshareSectorProvider) Fetch(ctx context.Context, req FetchRequest) (*Snapshot, error) {
	if strings.TrimSpace(p.BaseURL) == "" {
		return nil, ErrNoData
	}
	client := p.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second}
	}
	candidates := []string{"/api/sector_flow", "/sector_flow", "/api/industry_money_flow", "/industry_money_flow"}
	var lastErr error
	for _, path := range candidates {
		endpoint, err := p.endpoint(path)
		if err != nil {
			lastErr = err
			continue
		}
		snap, err := p.fetchOne(ctx, client, endpoint)
		if err == nil && snap != nil && len(snap.Sectors) > 0 {
			return snap, nil
		}
		if err != nil {
			lastErr = err
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, ErrNoData
}

func (p *AkshareSectorProvider) endpoint(path string) (string, error) {
	u, err := url.Parse(strings.TrimRight(p.BaseURL, "/") + path)
	if err != nil {
		return "", fmt.Errorf("akshare sectorflow: build url: %w", err)
	}
	q := u.Query()
	q.Set("market", "a_share")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func (p *AkshareSectorProvider) fetchOne(ctx context.Context, client *http.Client, endpoint string) (*Snapshot, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Accept", "application/json")
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("akshare sectorflow: http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNoData
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("akshare sectorflow: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("akshare sectorflow: read: %w", err)
	}
	return parseAkshareSectorFlow(body)
}

// parseAkshareSectorFlow walks the two shape variants. Returns
// ErrNoData when the result row count is zero so the registry can
// try the next candidate path / provider.
func parseAkshareSectorFlow(body []byte) (*Snapshot, error) {
	trim := strings.TrimSpace(string(body))
	if trim == "" {
		return nil, ErrNoData
	}
	var rows []map[string]any
	if strings.HasPrefix(trim, "[") {
		if err := json.Unmarshal(body, &rows); err != nil {
			return nil, fmt.Errorf("akshare sectorflow: decode array: %w", err)
		}
	} else {
		var wrapper map[string]any
		if err := json.Unmarshal(body, &wrapper); err != nil {
			return nil, fmt.Errorf("akshare sectorflow: decode: %w", err)
		}
		if raw, ok := wrapper["data"].([]any); ok {
			for _, row := range raw {
				if mp, ok := row.(map[string]any); ok {
					rows = append(rows, mp)
				}
			}
		} else if raw, ok := wrapper["sectors"].([]any); ok {
			for _, row := range raw {
				if mp, ok := row.(map[string]any); ok {
					rows = append(rows, mp)
				}
			}
		} else {
			// Single bare row (rare, but tolerated).
			rows = []map[string]any{wrapper}
		}
	}
	if len(rows) == 0 {
		return nil, ErrNoData
	}

	out := make([]Sector, 0, len(rows))
	for _, row := range rows {
		name := akStr(row, "name", "sector", "industry", "板块名称", "行业")
		if strings.TrimSpace(name) == "" {
			continue
		}
		s := Sector{
			Name:        name,
			Symbol:      akStr(row, "symbol", "code", "板块代码"),
			Return1d:    pct(akFloat(row, "change_pct", "pct_change", "今日涨跌幅", "涨跌幅")),
			Return5d:    pct(akFloat(row, "change_pct_5d", "近5日涨跌幅")),
			Return20d:   pct(akFloat(row, "change_pct_20d", "近20日涨跌幅", "近一月涨跌幅")),
			NetInflow:   akFloat(row, "net_inflow", "main_net_inflow", "今日主力净流入", "主力净流入"),
			NetInflow5d: akFloat(row, "net_inflow_5d", "近5日主力净流入"),
			Currency:    "CNY",
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil, ErrNoData
	}
	return &Snapshot{
		Market:  "a_share",
		AsOf:    time.Now().UTC(),
		Sectors: out,
		Source:  "akshare_sector",
	}, nil
}

// pct converts a percentage point ("+2.13" meaning +2.13%) to a
// fraction (0.0213). Akshare typically returns whole-percent
// figures, while Yahoo returns fractions; the formatter assumes
// fractions, so we normalize here.
//
// We only convert when the absolute value is greater than 1.5 — a
// guard against the rare endpoint that already returns fractions
// (which would have |v|<<1). Values like ±0.013 are left intact.
func pct(v float64) float64 {
	if v == 0 {
		return 0
	}
	if v > 1.5 || v < -1.5 {
		return v / 100
	}
	return v
}

func akStr(row map[string]any, keys ...string) string {
	for _, key := range keys {
		if v, ok := row[key]; ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				return s
			}
		}
	}
	return ""
}

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
			f, _ := t.Float64()
			return f
		case string:
			s := strings.TrimSpace(strings.TrimSuffix(t, "%"))
			if f, err := strconv.ParseFloat(s, 64); err == nil {
				return f
			}
		}
	}
	return 0
}
