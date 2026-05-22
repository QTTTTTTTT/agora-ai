package marketdata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// CoinbaseWSURLDefault is Coinbase Exchange's public market-data websocket
// endpoint. It accepts unauthenticated `ticker` channel subscriptions and is
// the recommended free real-time feed for spot pairs. See
// https://docs.cdp.coinbase.com/exchange/websocket-feed/overview.
const CoinbaseWSURLDefault = "wss://ws-feed.exchange.coinbase.com"

// defaultCoinbaseProducts is the seed subscription list. Coinbase uses
// dash-separated product ids ("BTC-USD") natively; we normalise to that
// shape before subscribing so users can supply either spelling via env.
var defaultCoinbaseProducts = []string{
	"BTC-USD", "ETH-USD", "SOL-USD", "XRP-USD", "ADA-USD",
	"DOGE-USD", "AVAX-USD", "DOT-USD", "LINK-USD", "LTC-USD",
	"MATIC-USD", "ATOM-USD", "NEAR-USD", "ARB-USD", "OP-USD",
	"BCH-USD", "FIL-USD", "APT-USD", "SUI-USD", "INJ-USD",
	"SHIB-USD", "PEPE-USD",
}

// coinbaseWSStream is the Coinbase counterpart to binanceWSStream. The
// protocols differ enough — product id format, channel-shaped subscriptions,
// `time` as ISO-8601 — that a shared abstraction would obscure both, so
// each stream owns its own loop and only the cache + retry helpers are
// shared.
type coinbaseWSStream struct {
	url            string
	productIDs     []string
	cache          *cryptoTickerCache
	health         *providerHealthTracker
	logger         *slog.Logger
	readTimeout    time.Duration
	backoffInitial time.Duration
	backoffMax     time.Duration

	dial         dialFunc
	subscribedAt atomic.Int64
	lastMessage  atomic.Int64
}

func newCoinbaseWSStream(url string, productIDs []string, cache *cryptoTickerCache, health *providerHealthTracker, logger *slog.Logger) *coinbaseWSStream {
	if strings.TrimSpace(url) == "" {
		url = CoinbaseWSURLDefault
	}
	cleaned := normalizeCoinbaseProducts(productIDs)
	if logger == nil {
		logger = slog.Default()
	}
	return &coinbaseWSStream{
		url:            url,
		productIDs:     cleaned,
		cache:          cache,
		health:         health,
		logger:         logger.With("component", "marketdata.coinbase_ws"),
		readTimeout:    90 * time.Second,
		backoffInitial: time.Second,
		backoffMax:     30 * time.Second,
		dial:           dialCoderWebsocket,
	}
}

// Run blocks until ctx is cancelled, reconnecting with exponential backoff
// on any error. See binanceWSStream.Run for the rationale.
func (s *coinbaseWSStream) Run(ctx context.Context) {
	if len(s.productIDs) == 0 {
		s.logger.Warn("no product_ids configured; skipping")
		return
	}
	backoff := s.backoffInitial
	for {
		if ctx.Err() != nil {
			return
		}
		err := s.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			if s.health != nil {
				s.health.recordFailure("coinbase-ws", err, time.Now().UTC(), 0)
			}
			s.logger.Warn("disconnected, backing off", "err", err, "backoff", backoff.String())
		} else {
			s.logger.Info("connection ended cleanly, backing off", "backoff", backoff.String())
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = nextBackoff(backoff, s.backoffMax)
	}
}

func (s *coinbaseWSStream) runOnce(ctx context.Context) error {
	dialCtx, dialCancel := context.WithTimeout(ctx, 15*time.Second)
	conn, err := s.dial(dialCtx, s.url)
	dialCancel()
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "shutdown")

	if err := s.subscribe(ctx, conn); err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	if s.health != nil {
		s.health.recordSuccess("coinbase-ws", 0)
	}
	s.subscribedAt.Store(time.Now().UnixNano())

	for {
		readCtx, cancel := context.WithTimeout(ctx, s.readTimeout)
		typ, data, err := conn.Read(readCtx)
		cancel()
		if err != nil {
			if errors.Is(err, context.Canceled) || ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, io.EOF) {
				return errors.New("eof")
			}
			return err
		}
		if typ != websocket.MessageText {
			continue
		}
		s.lastMessage.Store(time.Now().UnixNano())
		s.handleMessage(data)
	}
}

// subscribe issues a single subscribe message with all configured product ids.
// Coinbase requires the subscribe message within 5s of connecting, so we
// send it eagerly before entering the read loop.
func (s *coinbaseWSStream) subscribe(ctx context.Context, conn wsConn) error {
	payload := map[string]any{
		"type": "subscribe",
		"channels": []map[string]any{
			{
				"name":        "ticker",
				"product_ids": s.productIDs,
			},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return conn.Write(writeCtx, websocket.MessageText, body)
}

// coinbaseTickerMessage models the relevant fields from a Coinbase ticker
// channel event. Field types are strings on the wire even for numeric
// values; we parse with strconv.ParseFloat and silently drop messages with
// unparseable / non-positive prices.
//
// Example payload:
//
//	{
//	  "type": "ticker",
//	  "product_id": "BTC-USD",
//	  "price": "67000.12",
//	  "best_bid": "67000.00",
//	  "best_ask": "67000.20",
//	  "volume_24h": "1200.3456",
//	  "time": "2025-05-19T12:34:56.789Z"
//	}
type coinbaseTickerMessage struct {
	Type      string `json:"type"`
	ProductID string `json:"product_id"`
	Price     string `json:"price"`
	BestBid   string `json:"best_bid"`
	BestAsk   string `json:"best_ask"`
	Volume24h string `json:"volume_24h"`
	Time      string `json:"time"`
}

func (s *coinbaseWSStream) handleMessage(data []byte) {
	var msg coinbaseTickerMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return
	}
	if msg.Type != "ticker" || msg.ProductID == "" || msg.Price == "" {
		return
	}
	price, err := strconv.ParseFloat(msg.Price, 64)
	if err != nil || price <= 0 {
		return
	}
	bid, _ := strconv.ParseFloat(msg.BestBid, 64)
	ask, _ := strconv.ParseFloat(msg.BestAsk, 64)
	volume, _ := strconv.ParseFloat(msg.Volume24h, 64)
	asOf := time.Now().UTC()
	if msg.Time != "" {
		if parsed, perr := time.Parse(time.RFC3339Nano, msg.Time); perr == nil {
			asOf = parsed.UTC()
		}
	}
	// Coinbase product ids contain a dash ("BTC-USD"); the cache strips it
	// during normalisation so a lookup with either spelling resolves to the
	// same row. Symbol on the snapshot keeps the dash so downstream UI /
	// logs show the canonical Coinbase form.
	snap := &QuoteSnapshot{
		Symbol:        msg.ProductID,
		Price:         price,
		Bid:           bid,
		Ask:           ask,
		Volume:        int64(volume),
		QuoteCurrency: inferQuoteFromCoinbaseProduct(msg.ProductID),
		AsOf:          asOf,
		Source:        "coinbase-ws",
	}
	s.cache.Put(msg.ProductID, snap)
}

func normalizeCoinbaseProducts(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, raw := range in {
		s := strings.ToUpper(strings.TrimSpace(raw))
		if s == "" {
			continue
		}
		// Accept both "BTCUSDT" and "BTC-USD"; if no dash is present, try
		// to insert one before the recognised quote suffix so the
		// subscription matches Coinbase's product id format.
		if !strings.Contains(s, "-") {
			s = injectCoinbaseDash(s)
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// injectCoinbaseDash converts "BTCUSD" → "BTC-USD" when a known suffix is
// recognised. Coinbase rejects subscribe requests for unknown product ids
// silently (the subscription ack just omits them), so a best-effort
// transformation here keeps env-driven config forgiving.
func injectCoinbaseDash(s string) string {
	upper := strings.ToUpper(s)
	suffixes := []string{"USDT", "USDC", "USD", "EUR", "GBP", "BTC", "ETH"}
	for _, suf := range suffixes {
		if strings.HasSuffix(upper, suf) && len(upper) > len(suf) {
			base := upper[:len(upper)-len(suf)]
			return base + "-" + suf
		}
	}
	return upper
}

// inferQuoteFromCoinbaseProduct extracts the quote token from a "BASE-QUOTE"
// product id. Returns empty when the format is unexpected.
func inferQuoteFromCoinbaseProduct(productID string) string {
	parts := strings.SplitN(productID, "-", 2)
	if len(parts) != 2 {
		return ""
	}
	return strings.ToUpper(parts[1])
}

// coinbaseQuoteProvider returns a quote provider that serves from the
// supplied cache, mirroring binanceQuoteProvider.
func coinbaseQuoteProvider(cache *cryptoTickerCache, maxAge time.Duration) quoteProviderFunc {
	return func(ctx context.Context, instrument InstrumentRef) (*QuoteSnapshot, error) {
		return cryptoCacheQuote(cache, instrument, maxAge, "coinbase-ws")
	}
}
