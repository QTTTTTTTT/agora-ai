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

// BinanceWSURLDefault is Binance's public market-data websocket endpoint.
// Quote: "data-stream.binance.vision is a market-data-only domain that does
// not require an API key" — see Binance spot-api-docs/faqs/market_data_only.
// The `/ws` path accepts SUBSCRIBE/UNSUBSCRIBE JSON control messages so we
// can dynamically pin symbols without rebuilding the URL.
const BinanceWSURLDefault = "wss://data-stream.binance.vision/ws"

// defaultBinanceSymbols is the seed subscription list when none is configured.
// These are the top-volume USDT pairs that account for the bulk of trading
// activity. Adding more is cheap (one SUBSCRIBE message) — operators that
// need long-tail coverage should set MARKETDATA_BINANCE_WS_SYMBOLS.
var defaultBinanceSymbols = []string{
	"BTCUSDT", "ETHUSDT", "BNBUSDT", "SOLUSDT", "XRPUSDT",
	"ADAUSDT", "DOGEUSDT", "AVAXUSDT", "DOTUSDT", "MATICUSDT",
	"LTCUSDT", "LINKUSDT", "ATOMUSDT", "NEARUSDT", "ARBUSDT",
	"OPUSDT", "TRXUSDT", "BCHUSDT", "FILUSDT", "APTUSDT",
	"SUIUSDT", "INJUSDT", "PEPEUSDT", "SHIBUSDT", "TONUSDT",
}

// binanceWSStream owns one websocket connection to Binance's public market
// data feed. It subscribes to <symbol>@ticker streams, parses each ticker
// event, and writes the latest QuoteSnapshot into a shared cache. The
// associated quote provider just reads from the cache so it never blocks
// on a network round-trip.
//
// Resilience model:
//   - The outer Run loop reconnects with exponential backoff (1s → 30s cap)
//     after any error until the context is cancelled.
//   - The reader uses a per-read timeout (configurable; default 90s) — if no
//     message or server-ping arrives in that window we treat the connection
//     as dead and reconnect.
//   - coder/websocket auto-responds to server pings; Binance sends one every
//     20s so a healthy connection always sees activity well within the read
//     deadline.
//
// Concurrency: Run is intended to be invoked from a single goroutine. The
// underlying cache is safe for concurrent reads by the Quote provider.
type binanceWSStream struct {
	url            string
	symbols        []string
	cache          *cryptoTickerCache
	health         *providerHealthTracker
	logger         *slog.Logger
	readTimeout    time.Duration
	backoffInitial time.Duration
	backoffMax     time.Duration

	dial         dialFunc // pluggable for tests
	subscribedAt atomic.Int64
	lastMessage  atomic.Int64
}

type dialFunc func(ctx context.Context, urlStr string) (wsConn, error)

// wsConn is the minimal websocket surface we need. coder/websocket.Conn
// satisfies it natively; tests substitute their own implementation that
// pipes messages from a goroutine.
type wsConn interface {
	Read(ctx context.Context) (websocket.MessageType, []byte, error)
	Write(ctx context.Context, typ websocket.MessageType, p []byte) error
	Close(code websocket.StatusCode, reason string) error
}

func newBinanceWSStream(url string, symbols []string, cache *cryptoTickerCache, health *providerHealthTracker, logger *slog.Logger) *binanceWSStream {
	if strings.TrimSpace(url) == "" {
		url = BinanceWSURLDefault
	}
	cleaned := normalizeBinanceSymbols(symbols)
	if logger == nil {
		logger = slog.Default()
	}
	return &binanceWSStream{
		url:            url,
		symbols:        cleaned,
		cache:          cache,
		health:         health,
		logger:         logger.With("component", "marketdata.binance_ws"),
		readTimeout:    90 * time.Second,
		backoffInitial: time.Second,
		backoffMax:     30 * time.Second,
		dial:           dialCoderWebsocket,
	}
}

// Run blocks until ctx is cancelled. It dials, subscribes, reads ticker
// messages, and reconnects with exponential backoff on any error.
func (s *binanceWSStream) Run(ctx context.Context) {
	if len(s.symbols) == 0 {
		s.logger.Warn("no symbols configured; skipping")
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
				s.health.recordFailure("binance-ws", err, time.Now().UTC(), 0)
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

// runOnce dials, subscribes, and reads until error or ctx cancel. Returns
// the disconnect reason (nil only when ctx was cancelled mid-read).
func (s *binanceWSStream) runOnce(ctx context.Context) error {
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
		s.health.recordSuccess("binance-ws", 0)
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

// subscribe sends a single SUBSCRIBE control message covering every
// configured symbol's @ticker stream. Binance batches subscriptions
// gracefully; we don't paginate until we exceed 1024 streams (their
// documented per-connection cap).
func (s *binanceWSStream) subscribe(ctx context.Context, conn wsConn) error {
	if len(s.symbols) == 0 {
		return nil
	}
	params := make([]string, 0, len(s.symbols))
	for _, sym := range s.symbols {
		params = append(params, strings.ToLower(sym)+"@ticker")
	}
	payload := map[string]any{
		"method": "SUBSCRIBE",
		"params": params,
		"id":     1,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return conn.Write(writeCtx, websocket.MessageText, body)
}

// binanceTickerMessage models the relevant fields from Binance's 24hr ticker
// stream payload. Unrecognised fields (subscription ack responses, etc.) are
// dropped silently — the JSON decoder leaves zero values on missing keys.
//
// Sample payload (truncated):
//
//	{
//	  "e": "24hrTicker",      // event type
//	  "E": 1715000000000,     // event time (ms)
//	  "s": "BTCUSDT",         // symbol
//	  "c": "67000.10",        // last/close price
//	  "b": "67000.00",        // best bid
//	  "a": "67000.20",        // best ask
//	  "v": "12345.67"         // 24h base asset volume
//	}
//
// Subscription replies look like {"result":null,"id":1} which contain no "s"
// or "c"; we skip them via the empty-symbol check in handleMessage.
type binanceTickerMessage struct {
	EventType string `json:"e"`
	EventTime int64  `json:"E"`
	Symbol    string `json:"s"`
	LastPrice string `json:"c"`
	Bid       string `json:"b"`
	Ask       string `json:"a"`
	Volume    string `json:"v"`
}

func (s *binanceWSStream) handleMessage(data []byte) {
	var msg binanceTickerMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return
	}
	if msg.Symbol == "" || msg.LastPrice == "" {
		return
	}
	price, err := strconv.ParseFloat(msg.LastPrice, 64)
	if err != nil || price <= 0 {
		return
	}
	bid, _ := strconv.ParseFloat(msg.Bid, 64)
	ask, _ := strconv.ParseFloat(msg.Ask, 64)
	volume, _ := strconv.ParseFloat(msg.Volume, 64)
	asOf := time.Now().UTC()
	if msg.EventTime > 0 {
		asOf = time.UnixMilli(msg.EventTime).UTC()
	}
	snap := &QuoteSnapshot{
		Symbol:        msg.Symbol,
		Price:         price,
		Bid:           bid,
		Ask:           ask,
		Volume:        int64(volume),
		QuoteCurrency: inferQuoteFromBinanceSymbol(msg.Symbol),
		AsOf:          asOf,
		Source:        "binance-ws",
	}
	s.cache.Put(msg.Symbol, snap)
}

// dialCoderWebsocket is the production dial implementation. Tests override
// binanceWSStream.dial to inject a fake conn.
func dialCoderWebsocket(ctx context.Context, urlStr string) (wsConn, error) {
	conn, _, err := websocket.Dial(ctx, urlStr, nil)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// normalizeBinanceSymbols uppercases + trims + dedupes the input list and
// drops empty strings. Order is preserved so the SUBSCRIBE payload is
// deterministic (useful for tests).
func normalizeBinanceSymbols(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, s := range in {
		s = strings.ToUpper(strings.TrimSpace(s))
		if s == "" {
			continue
		}
		s = strings.ReplaceAll(s, "-", "")
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// inferQuoteFromBinanceSymbol extracts the quote-currency suffix from a
// canonical Binance pair like "BTCUSDT" → "USDT". Returns empty when no
// recognised suffix is found.
func inferQuoteFromBinanceSymbol(symbol string) string {
	upper := strings.ToUpper(symbol)
	suffixes := []string{"USDT", "USDC", "BUSD", "TUSD", "DAI", "FDUSD", "USD", "EUR", "BTC", "ETH"}
	for _, suf := range suffixes {
		if strings.HasSuffix(upper, suf) && len(upper) > len(suf) {
			return suf
		}
	}
	return ""
}

// nextBackoff doubles the current delay up to a hard cap. Pure function so
// tests can replay the schedule deterministically.
func nextBackoff(current, ceiling time.Duration) time.Duration {
	if current <= 0 {
		return time.Second
	}
	next := current * 2
	if next > ceiling {
		return ceiling
	}
	return next
}

// binanceQuoteProvider returns a quoteProviderFunc that reads from the
// supplied cache. maxAge controls how old a cached ticker may be before the
// provider returns ErrQuoteUnavailable, letting the chain fall back to
// CoinGecko / Yahoo. binanceCryptoCacheLookup centralises the cache hit
// path so the Coinbase provider can share the same logic.
func binanceQuoteProvider(cache *cryptoTickerCache, maxAge time.Duration) quoteProviderFunc {
	return func(ctx context.Context, instrument InstrumentRef) (*QuoteSnapshot, error) {
		return cryptoCacheQuote(cache, instrument, maxAge, "binance-ws")
	}
}

// cryptoCacheQuote is shared by Binance and Coinbase providers. It performs
// a cache lookup using the instrument's normalised symbol, decorates the
// snapshot with the instrument's identity fields, and returns
// ErrQuoteUnavailable when nothing fresh is cached.
func cryptoCacheQuote(cache *cryptoTickerCache, instrument InstrumentRef, maxAge time.Duration, expectSource string) (*QuoteSnapshot, error) {
	if cache == nil {
		return nil, ErrQuoteUnavailable
	}
	symbol := strings.TrimSpace(instrument.Symbol)
	if symbol == "" {
		return nil, fmt.Errorf("%s: empty symbol", expectSource)
	}
	now := time.Now().UTC()
	snap, ok := cache.Get(symbol, now, maxAge)
	if !ok {
		return nil, ErrQuoteUnavailable
	}
	snap.InstrumentKey = instrument.InstrumentKey
	snap.Market = instrument.Market
	snap.Exchange = instrument.Exchange
	snap.AssetClass = instrument.AssetClass
	if snap.Symbol == "" {
		snap.Symbol = instrument.NormalizedSymbol()
	}
	if instrument.QuoteCurrency != "" {
		snap.QuoteCurrency = instrument.QuoteCurrency
	}
	return snap, nil
}

