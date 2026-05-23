package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fundai/server/internal/fundamental"
	"github.com/fundai/server/internal/marketdata"
	"github.com/fundai/server/internal/sectorflow"
	"github.com/fundai/server/internal/sentiment"
)

// stubFundamentalFetcher lets tests pin the per-symbol metrics the
// pool sees. byMarket lets the same fetcher reply differently per
// market so we can verify the runtime forwards the market tag.
type stubFundamentalFetcher struct {
	metrics map[string]*fundamental.Metrics
	err     error
	calls   []fundamental.FetchRequest
}

func (s *stubFundamentalFetcher) Fetch(_ context.Context, req fundamental.FetchRequest) (*fundamental.Metrics, error) {
	s.calls = append(s.calls, req)
	if s.err != nil {
		return nil, s.err
	}
	if m, ok := s.metrics[strings.ToUpper(req.Symbol)]; ok {
		clone := *m
		return &clone, nil
	}
	return nil, fundamental.ErrNoData
}

// fundamentalMetrics returns ok=false with nil fetcher.
func TestFundamentalMetricsNilFetcher(t *testing.T) {
	pool := runtimeResearcherPool{}
	_, ok := pool.fundamentalMetrics(context.Background(), marketdata.InstrumentRef{Symbol: "AAPL", Market: "us_equity"})
	if ok {
		t.Errorf("expected ok=false when fundamentalFetcher is nil")
	}
}

// fundamentalMetrics swallows ErrNoData / ErrNoProvider so missing
// data never breaks the quant research line.
func TestFundamentalMetricsSwallowsErrNoData(t *testing.T) {
	for _, errCase := range []error{fundamental.ErrNoData, fundamental.ErrNoProvider, errors.New("upstream blip")} {
		stub := &stubFundamentalFetcher{err: errCase}
		pool := runtimeResearcherPool{fundamentalFetcher: stub}
		_, ok := pool.fundamentalMetrics(context.Background(), marketdata.InstrumentRef{Symbol: "AAPL", Market: "us_equity"})
		if ok {
			t.Errorf("expected ok=false for err=%v", errCase)
		}
		if len(stub.calls) != 1 {
			t.Errorf("expected exactly one upstream attempt, got %d", len(stub.calls))
		}
	}
}

// fundamentalMetrics happy path returns a non-nil Metrics + ok=true.
func TestFundamentalMetricsHappyPath(t *testing.T) {
	stub := &stubFundamentalFetcher{metrics: map[string]*fundamental.Metrics{
		"AAPL": {Symbol: "AAPL", PE: 28.3, PB: 47.2, ReturnOnEquity: 1.56, Source: "yahoo"},
	}}
	pool := runtimeResearcherPool{fundamentalFetcher: stub}
	m, ok := pool.fundamentalMetrics(context.Background(), marketdata.InstrumentRef{Symbol: "aapl", Market: "us_equity"})
	if !ok || m == nil {
		t.Fatalf("expected ok=true and non-nil metrics; m=%+v ok=%v", m, ok)
	}
	if m.PE != 28.3 {
		t.Errorf("PE forwarded wrong: %v", m.PE)
	}
	if len(stub.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(stub.calls))
	}
	if stub.calls[0].Symbol != "AAPL" {
		t.Errorf("symbol should be normalised: %q", stub.calls[0].Symbol)
	}
}

// collectFundamentalBlock filters missing symbols + empty entries.
func TestCollectFundamentalBlockFiltersAndCaps(t *testing.T) {
	stub := &stubFundamentalFetcher{metrics: map[string]*fundamental.Metrics{
		"AAPL": {Symbol: "AAPL", PE: 28.3, MarketCap: 2.85e12, Currency: "USD"},
		"MSFT": {Symbol: "MSFT", PE: 32.1, MarketCap: 2.9e12, Currency: "USD"},
		// NVDA is missing → silently dropped
	}}
	pool := runtimeResearcherPool{fundamentalFetcher: stub}
	lines := pool.collectFundamentalBlock(context.Background(), fundMarketProfile{Market: "us_equity"}, []string{"AAPL", "MSFT", "", "  ", "NVDA"})
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines (empty/missing filtered), got %d: %v", len(lines), lines)
	}
	if !strings.Contains(lines[0], "AAPL:") || !strings.Contains(lines[0], "PE 28.3") {
		t.Errorf("AAPL line malformed: %q", lines[0])
	}
}

// collectFundamentalBlock caps at 20 symbols.
func TestCollectFundamentalBlockCaps(t *testing.T) {
	mm := make(map[string]*fundamental.Metrics, 40)
	syms := make([]string, 40)
	for i := 0; i < 40; i++ {
		s := "S" + string(rune('A'+i%26)) + string(rune('A'+i%5))
		syms[i] = s
		mm[strings.ToUpper(s)] = &fundamental.Metrics{Symbol: s, PE: float64(10 + i)}
	}
	stub := &stubFundamentalFetcher{metrics: mm}
	pool := runtimeResearcherPool{fundamentalFetcher: stub}
	lines := pool.collectFundamentalBlock(context.Background(), fundMarketProfile{Market: "us_equity"}, syms)
	if len(lines) > 20 {
		t.Errorf("expected cap at 20 lines, got %d", len(lines))
	}
	if len(stub.calls) > 20 {
		t.Errorf("expected at most 20 calls, got %d", len(stub.calls))
	}
}

// collectFundamentalBlock returns nil when fetcher is unwired.
func TestCollectFundamentalBlockNilFetcher(t *testing.T) {
	pool := runtimeResearcherPool{}
	if lines := pool.collectFundamentalBlock(context.Background(), fundMarketProfile{Market: "us_equity"}, []string{"AAPL"}); lines != nil {
		t.Errorf("expected nil, got %v", lines)
	}
}

// buildFundamentalFetcherFromEnv: disabled flag returns nil.
func TestBuildFundamentalFetcherFromEnvDisabled(t *testing.T) {
	t.Setenv("FUNDAMENTAL_DISABLED", "1")
	if f := buildFundamentalFetcherFromEnv(); f != nil {
		t.Errorf("expected nil when FUNDAMENTAL_DISABLED=1, got %T", f)
	}
}

// buildFundamentalFetcherFromEnv: default returns non-nil cache.
func TestBuildFundamentalFetcherFromEnvDefault(t *testing.T) {
	t.Setenv("FUNDAMENTAL_DISABLED", "")
	t.Setenv("YAHOO_FUNDAMENTAL_DISABLED", "")
	t.Setenv("AKSHARE_FUNDAMENTAL_URL", "")
	f := buildFundamentalFetcherFromEnv()
	if f == nil {
		t.Fatal("expected non-nil cache by default")
	}
	if _, ok := f.(*fundamental.Cache); !ok {
		t.Errorf("expected *fundamental.Cache wrapper, got %T", f)
	}
}

// -----------------------------------------------------------------
// SectorFlow wiring
// -----------------------------------------------------------------

type stubSectorFlowFetcher struct {
	snapshot *sectorflow.Snapshot
	err      error
	calls    []sectorflow.FetchRequest
}

func (s *stubSectorFlowFetcher) Fetch(_ context.Context, req sectorflow.FetchRequest) (*sectorflow.Snapshot, error) {
	s.calls = append(s.calls, req)
	if s.err != nil {
		return nil, s.err
	}
	if s.snapshot == nil {
		return nil, sectorflow.ErrNoData
	}
	clone := *s.snapshot
	clone.Sectors = append([]sectorflow.Sector(nil), s.snapshot.Sectors...)
	return &clone, nil
}

// sectorRotationDebateBlock returns "" with nil fetcher.
func TestSectorRotationDebateBlockNilFetcher(t *testing.T) {
	pool := runtimeResearcherPool{}
	if got := pool.sectorRotationDebateBlock(context.Background(), fundMarketProfile{Market: "us_equity"}); got != "" {
		t.Errorf("expected empty when fetcher nil, got %q", got)
	}
}

// sectorRotationDebateBlock swallows ErrNoData / ErrNoProvider so a
// missing rotation source doesn't break the macro brief.
func TestSectorRotationDebateBlockSwallowsErr(t *testing.T) {
	for _, errCase := range []error{sectorflow.ErrNoData, sectorflow.ErrNoProvider, errors.New("network blip")} {
		stub := &stubSectorFlowFetcher{err: errCase}
		pool := runtimeResearcherPool{sectorFlowFetcher: stub}
		if got := pool.sectorRotationDebateBlock(context.Background(), fundMarketProfile{Market: "us_equity"}); got != "" {
			t.Errorf("expected empty for err=%v, got %q", errCase, got)
		}
	}
}

// sectorRotationDebateBlock produces a "Sector rotation" header +
// FormatForPrompt body on success.
func TestSectorRotationDebateBlockHappyPath(t *testing.T) {
	stub := &stubSectorFlowFetcher{snapshot: &sectorflow.Snapshot{
		Market: "us_equity",
		Sectors: []sectorflow.Sector{
			{Name: "Technology", Return1d: 0.018, Currency: "USD"},
			{Name: "Energy", Return1d: -0.023, Currency: "USD"},
			{Name: "Financials", Return1d: 0.012, Currency: "USD"},
			{Name: "Real Estate", Return1d: 0.005, Currency: "USD"},
			{Name: "Utilities", Return1d: -0.015, Currency: "USD"},
			{Name: "Industrials", Return1d: 0.009, Currency: "USD"},
		},
	}}
	pool := runtimeResearcherPool{sectorFlowFetcher: stub}
	got := pool.sectorRotationDebateBlock(context.Background(), fundMarketProfile{Market: "us_equity"})
	if !strings.HasPrefix(got, "Sector rotation\n") {
		t.Errorf("missing header: %q", got)
	}
	if !strings.Contains(got, "Top") || !strings.Contains(got, "Bottom") {
		t.Errorf("missing top/bottom lines: %s", got)
	}
}

// sectorRotationDebateBlock forwards the market tag lowercase.
func TestSectorRotationDebateBlockForwardsMarket(t *testing.T) {
	stub := &stubSectorFlowFetcher{snapshot: &sectorflow.Snapshot{
		Market:  "a_share",
		Sectors: []sectorflow.Sector{{Name: "半导体", Return1d: 0.021}},
	}}
	pool := runtimeResearcherPool{sectorFlowFetcher: stub}
	_ = pool.sectorRotationDebateBlock(context.Background(), fundMarketProfile{Market: "A_Share"})
	if len(stub.calls) != 1 || stub.calls[0].Market != "a_share" {
		t.Errorf("market should be normalised to a_share, got %+v", stub.calls)
	}
}

// buildSectorFlowFetcherFromEnv: disabled flag returns nil.
func TestBuildSectorFlowFetcherFromEnvDisabled(t *testing.T) {
	t.Setenv("SECTORFLOW_DISABLED", "1")
	if f := buildSectorFlowFetcherFromEnv(nil); f != nil {
		t.Errorf("expected nil when SECTORFLOW_DISABLED=1, got %T", f)
	}
}

// buildSectorFlowFetcherFromEnv: default returns non-nil cache.
func TestBuildSectorFlowFetcherFromEnvDefault(t *testing.T) {
	t.Setenv("SECTORFLOW_DISABLED", "")
	t.Setenv("YAHOO_SECTORFLOW_DISABLED", "")
	t.Setenv("AKSHARE_SECTORFLOW_URL", "")
	f := buildSectorFlowFetcherFromEnv(nil)
	if f == nil {
		t.Fatal("expected non-nil cache by default")
	}
	if _, ok := f.(*sectorflow.Cache); !ok {
		t.Errorf("expected *sectorflow.Cache wrapper, got %T", f)
	}
}

// -----------------------------------------------------------------
// Sentiment wiring
// -----------------------------------------------------------------

// stubSentimentScorer canned-replies for the chain.
type stubSentimentScorer struct {
	scores []sentiment.Score
	err    error
	calls  int
}

func (s *stubSentimentScorer) Score(_ context.Context, _ []sentiment.Item) ([]sentiment.Score, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	out := make([]sentiment.Score, len(s.scores))
	copy(out, s.scores)
	return out, nil
}

// newsItemsToSentiment maps marketdata fields → sentiment.Item.
func TestNewsItemsToSentimentMapsFields(t *testing.T) {
	items := []marketdata.NewsItem{
		{Title: "Apple beats earnings", URL: "https://a.com/1", Source: "WSJ", Symbols: []string{"AAPL"}},
		{Title: "", TitleEn: "Tesla recall", URL: "https://t.com/2", Symbols: []string{"TSLA"}},
		{Title: "无标题", Summary: "公司停牌"}, // no URL → falls back to title hash
	}
	out := newsItemsToSentiment(items)
	if len(out) != 3 {
		t.Fatalf("expected 3 items, got %d", len(out))
	}
	if out[0].ID == "" || out[0].Title != "Apple beats earnings" {
		t.Errorf("AAPL item wrong: %+v", out[0])
	}
	if out[1].Title != "Tesla recall" {
		t.Errorf("expected EN fallback title, got %+v", out[1])
	}
	if out[2].ID != "无标题" {
		t.Errorf("expected ID = title when URL empty, got %+v", out[2])
	}
}

// resolveSentimentScorer returns the wired scorer when set.
func TestResolveSentimentScorerWired(t *testing.T) {
	stub := &stubSentimentScorer{}
	adapter := &workflowServiceAdapter{}
	adapter.WithSentimentScorer(stub)
	if got := adapter.resolveSentimentScorer("fund-1"); got != stub {
		t.Errorf("expected wired stub, got %T", got)
	}
}

// resolveSentimentScorer falls back to env-driven default when
// nothing is wired and SENTIMENT_DISABLED is unset.
func TestResolveSentimentScorerFallsBackToKeywordWhenNoLLM(t *testing.T) {
	t.Setenv("SENTIMENT_DISABLED", "")
	t.Setenv("SENTIMENT_LLM_DISABLED", "")
	adapter := &workflowServiceAdapter{}
	got := adapter.resolveSentimentScorer("fund-1")
	if got == nil {
		t.Fatal("expected non-nil scorer (keyword fallback) when nothing wired")
	}
	// Without a runtime/LLM the build path returns a KeywordScorer
	// directly (not a CompositeScorer).
	if _, ok := got.(*sentiment.KeywordScorer); !ok {
		t.Errorf("expected *sentiment.KeywordScorer, got %T", got)
	}
}

// resolveSentimentScorer returns nil when SENTIMENT_DISABLED=1.
func TestResolveSentimentScorerDisabled(t *testing.T) {
	t.Setenv("SENTIMENT_DISABLED", "1")
	adapter := &workflowServiceAdapter{}
	if got := adapter.resolveSentimentScorer("fund-1"); got != nil {
		t.Errorf("expected nil when SENTIMENT_DISABLED=1, got %T", got)
	}
}

// buildSentimentScorerFromRuntime returns *KeywordScorer when LLM
// disabled.
func TestBuildSentimentScorerFromRuntimeLLMDisabled(t *testing.T) {
	t.Setenv("SENTIMENT_DISABLED", "")
	t.Setenv("SENTIMENT_LLM_DISABLED", "1")
	got := buildSentimentScorerFromRuntime(nil, "fund-1", "user-tong")
	if _, ok := got.(*sentiment.KeywordScorer); !ok {
		t.Errorf("expected *sentiment.KeywordScorer, got %T", got)
	}
}

// firstNonEmpty grabs the first non-empty trimmed value.
func TestFirstNonEmpty(t *testing.T) {
	if firstNonEmpty("", "  ", "hello", "world") != "hello" {
		t.Errorf("expected 'hello'")
	}
	if firstNonEmpty("", "  ") != "" {
		t.Errorf("expected ''")
	}
}
