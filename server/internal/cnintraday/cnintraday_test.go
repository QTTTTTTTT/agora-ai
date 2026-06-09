package cnintraday

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"
)

// -------- time filter -------------------------------------------------------

func TestIntradayTimeFilterTrimsOpenAndClose(t *testing.T) {
	loc := beijingTimezone()
	for _, c := range []struct {
		hm    string
		wantOK bool
	}{
		{"09:30", false}, // before 9:35 — open-auction noise
		{"09:34", false},
		{"09:35", true},
		{"10:00", true},
		{"11:30", true},
		{"11:31", false},      // lunch break
		{"12:30", false},
		{"13:00", true},
		{"14:54", true},
		{"14:55", false}, // last 5 min
		{"15:00", false},
	} {
		t.Run(c.hm, func(t *testing.T) {
			tt, _ := time.ParseInLocation("15:04", c.hm, loc)
			got := IntradayTimeFilter(tt)
			if got != c.wantOK {
				t.Errorf("filter(%s) = %v, want %v", c.hm, got, c.wantOK)
			}
		})
	}
}

// -------- factor math -------------------------------------------------------

func makeBars(count int, baseClose float64) []MinuteBar {
	start := time.Date(2026, 1, 5, 9, 30, 0, 0, time.UTC)
	out := make([]MinuteBar, count)
	for i := 0; i < count; i++ {
		out[i] = MinuteBar{
			Timestamp: start.Add(time.Duration(i) * time.Minute),
			Open:      baseClose,
			Close:     baseClose,
			High:      baseClose,
			Low:       baseClose,
			Volume:    1_000_000,
			Amount:    baseClose * 1_000_000,
		}
	}
	return out
}

func TestComputeFactorsHandlesEmptyWindow(t *testing.T) {
	tup := ComputeFactors(nil)
	if tup.SectorRank != 0.5 {
		t.Errorf("default sector rank should be 0.5, got %v", tup.SectorRank)
	}
}

func TestComputeFactorsBreakoutFiresAboveTrailingHigh(t *testing.T) {
	bars := makeBars(65, 10.0)
	// Last bar pushes the close above the trailing high by 1.0
	// (against base of 10.0). Trailing close stdev should be
	// very small (all closes were 10.0) → breakout z-score very
	// large. We just check sign + that it's > 0.
	bars[64].Close = 11.0
	bars[64].High = 11.0
	// Need some variance in the prior bars for stdev > 0 to
	// avoid divide-by-zero gating us out.
	for i := 0; i < 64; i++ {
		bars[i].Close = 10.0 + float64(i%3)*0.001
		bars[i].High = bars[i].Close
	}
	win := &MinuteWindow{Symbol: "TEST", Bars: bars}
	f := ComputeFactors(win)
	if f.Breakout <= 0 {
		t.Errorf("breakout score = %v, want > 0", f.Breakout)
	}
}

func TestComputeFactorsVolumeSurgeReturnsRatio(t *testing.T) {
	bars := makeBars(20, 10.0)
	// First 15 bars: 1M; last 5: 3M. Mean5 = 3M, Mean20 = (15*1M + 5*3M)/20 = 1.5M → ratio 2.0
	for i := 15; i < 20; i++ {
		bars[i].Volume = 3_000_000
	}
	f := ComputeFactors(&MinuteWindow{Symbol: "TEST", Bars: bars})
	if math.Abs(f.VolumeSurge-2.0) > 1e-6 {
		t.Errorf("volume surge = %v, want 2.0", f.VolumeSurge)
	}
}

func TestComputeFactorsBigInflowSumsLastFiveBars(t *testing.T) {
	bars := makeBars(10, 10.0)
	for i := 5; i < 10; i++ {
		bars[i].BigOrderNet = 200_000
	}
	f := ComputeFactors(&MinuteWindow{Symbol: "TEST", Bars: bars})
	if math.Abs(f.BigInflow-1_000_000) > 1e-6 {
		t.Errorf("big inflow = %v, want 1_000_000", f.BigInflow)
	}
}

func TestComputeFactorsOrderImbalanceAverages(t *testing.T) {
	bars := makeBars(5, 10.0)
	bars[2].BidAskRatio = 1.0
	bars[3].BidAskRatio = 2.0
	bars[4].BidAskRatio = 3.0
	f := ComputeFactors(&MinuteWindow{Symbol: "TEST", Bars: bars})
	// Last 3 bars: 1, 2, 3 → mean 2.0
	if math.Abs(f.OrderImbalance-2.0) > 1e-6 {
		t.Errorf("order imbalance = %v, want 2.0", f.OrderImbalance)
	}
}

// -------- rule evaluation ---------------------------------------------------

func nowAtHM(hour, minute int) time.Time {
	loc := beijingTimezone()
	return time.Date(2026, 6, 5, hour, minute, 0, 0, loc)
}

func TestEvaluateReturnsNilOutsideSession(t *testing.T) {
	sig := Evaluate(EvaluateInput{
		PrevClose:  10.0,
		LastBar:    MinuteBar{Close: 10.5},
		NowBeijing: nowAtHM(8, 0), // before market
		Info:       SymbolInfo{Symbol: "X", Market: MarketMainBoard},
	}, ConservativeRuleSet())
	if sig != nil {
		t.Errorf("expected nil signal before market open, got %+v", sig)
	}
}

func TestEvaluateEmitsWarningWhenNearLimit(t *testing.T) {
	sig := Evaluate(EvaluateInput{
		Symbol:    "X",
		Info:      SymbolInfo{Symbol: "X", Name: "Test", Market: MarketMainBoard}, // limit ±10%
		PrevClose: 10.0,
		LastBar:   MinuteBar{Close: 10.9}, // limit-up = 11.0, distance = 1/10.9 ≈ 0.9% (< 4%)
		Factors: FactorTuple{
			Breakout: 5.0, VolumeSurge: 5.0, SectorRank: 1.0,
			BigInflow: 10_000_000, OrderImbalance: 5.0,
		},
		NowBeijing: nowAtHM(10, 30),
	}, ConservativeRuleSet())
	if sig == nil || sig.Type != SignalWarning {
		t.Fatalf("expected WARNING near limit-up, got %+v", sig)
	}
	if len(sig.RiskWarnings) == 0 {
		t.Errorf("warning should carry RiskWarnings")
	}
}

func TestEvaluateEmitsBuyWhenAllFactorsPass(t *testing.T) {
	sig := Evaluate(EvaluateInput{
		Symbol:    "002415",
		Info:      SymbolInfo{Symbol: "002415", Name: "海康威视", Market: MarketMainBoard},
		PrevClose: 35.0,
		LastBar:   MinuteBar{Close: 35.5}, // plenty of distance to limit-up @ 38.5
		Factors: FactorTuple{
			Breakout:       2.0, // > 1.5
			VolumeSurge:    1.8, // > 1.5
			BigInflow:      600_000,
			OrderImbalance: 1.4,
			SectorRank:     0.7,
		},
		NowBeijing: nowAtHM(10, 0),
	}, ConservativeRuleSet())
	if sig == nil {
		t.Fatalf("expected BUY signal")
	}
	if sig.Type != SignalBuy {
		t.Errorf("type = %v, want BUY", sig.Type)
	}
	if sig.Confidence <= 0 {
		t.Errorf("confidence should be > 0, got %v", sig.Confidence)
	}
	if len(sig.Reasons) == 0 {
		t.Errorf("BUY must carry Reasons")
	}
	if sig.TargetPrice <= sig.Price {
		t.Errorf("target price %v should exceed entry %v", sig.TargetPrice, sig.Price)
	}
	if sig.StopLoss >= sig.Price {
		t.Errorf("stop loss %v should be below entry %v", sig.StopLoss, sig.Price)
	}
}

func TestEvaluateNoSignalWhenBreakoutFails(t *testing.T) {
	sig := Evaluate(EvaluateInput{
		Symbol:    "X",
		Info:      SymbolInfo{Symbol: "X", Name: "Test", Market: MarketMainBoard},
		PrevClose: 10.0,
		LastBar:   MinuteBar{Close: 10.5},
		Factors: FactorTuple{
			Breakout:   0.5, // < 1.5 → fail
			VolumeSurge: 2.0, SectorRank: 0.8,
			BigInflow: 1_000_000, OrderImbalance: 2.0,
		},
		NowBeijing: nowAtHM(10, 0),
	}, ConservativeRuleSet())
	if sig != nil {
		t.Errorf("expected nil when breakout fails, got %+v", sig)
	}
}

// -------- price-limit per market segment ------------------------------------

func TestSymbolInfoPriceLimitMatrix(t *testing.T) {
	cases := []struct {
		market Market
		want   float64
	}{
		{MarketMainBoard, 0.10},
		{MarketChinext, 0.20},
		{MarketSTAR, 0.20},
		{MarketST, 0.05},
		{MarketBSE, 0.30},
	}
	for _, c := range cases {
		got := SymbolInfo{Market: c.market}.PriceLimit()
		if math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%v limit = %v, want %v", c.market, got, c.want)
		}
	}
}

// -------- feishu render -----------------------------------------------------

func TestRenderSignalProducesPostMessage(t *testing.T) {
	sig := &TradeSignal{
		Timestamp:         time.Now(),
		Symbol:            "002415",
		Name:              "海康威视",
		Type:              SignalBuy,
		Price:             35.5,
		Confidence:        0.78,
		SuggestedPosition: 0.10,
		TargetPrice:       37.3,
		StopLoss:          34.4,
		Reasons:           []string{"突破前 60min 高点", "放量 1.82x"},
	}
	msg := RenderSignal(sig)
	if msg.MsgType != "post" {
		t.Errorf("msg_type = %q, want post", msg.MsgType)
	}
	zh, ok := msg.Content.Post["zh_cn"]
	if !ok {
		t.Fatalf("missing zh_cn locale")
	}
	if !strings.Contains(zh.Title, "海康威视") {
		t.Errorf("title missing symbol name: %q", zh.Title)
	}
	if len(zh.Content) < 2 {
		t.Errorf("expected ≥2 rows, got %d", len(zh.Content))
	}
}

func TestStubFeishuWebhookCapturesPayloads(t *testing.T) {
	stub := &StubFeishuWebhook{}
	err := stub.Send(context.Background(), FeishuMessage{MsgType: "text", Content: FeishuContent{Text: "hi"}})
	if err != nil {
		t.Errorf("stub send err = %v", err)
	}
	if len(stub.Sent) != 1 {
		t.Errorf("expected 1 captured message, got %d", len(stub.Sent))
	}
	if stub.Sent[0].Content.Text != "hi" {
		t.Errorf("captured text = %q", stub.Sent[0].Content.Text)
	}
}

func TestStaticDirectoryLookup(t *testing.T) {
	d := NewStaticDirectory(
		SymbolInfo{Symbol: "000300.SH", Name: "沪深300", Market: MarketMainBoard},
		SymbolInfo{Symbol: "300750.SZ", Name: "宁德时代", Market: MarketChinext},
	)
	if _, ok := d.Lookup("000300.SH"); !ok {
		t.Errorf("expected directory hit for 000300.SH")
	}
	v, ok := d.Lookup("300750.SZ")
	if !ok || v.Market != MarketChinext {
		t.Errorf("expected chinext lookup hit, got %+v ok=%v", v, ok)
	}
	if _, ok := d.Lookup("MISSING"); ok {
		t.Errorf("expected miss for unknown symbol")
	}
}
