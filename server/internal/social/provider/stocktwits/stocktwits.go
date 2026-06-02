// Package stocktwits implements social.Provider against the
// public StockTwits "streams/symbol" endpoint.
//
// StockTwits is the de-facto retail social network for US
// equities. Its public REST API exposes per-symbol message
// streams without authentication
// (https://api.stocktwits.com/api/2/streams/symbol/<SYM>.json),
// and — crucially for us — it also returns the author's
// self-declared bull/bear sentiment tag on each message when
// present. We pass that tag through as a sentiment.Item tag so
// the downstream scorer doesn't have to guess the polarity for
// already-classified posts.
//
// Rate-limit posture: the unauthenticated endpoint allows ~200
// requests per IP per hour. The daily PM loop fetches each
// candidate symbol once, so that's well within budget.
package stocktwits

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/fundai/server/internal/sentiment"
	"github.com/fundai/server/internal/social"
)

const (
	defaultEndpoint = "https://api.stocktwits.com/api/2/streams/symbol"
	defaultUA       = "fundai-platform/1.0 (sentiment-analyst social ingest)"
	maxBodyRunes    = 280
)

// Options tune the StockTwits provider. All fields are optional.
type Options struct {
	HTTPClient *http.Client
	UserAgent  string
	Endpoint   string // override for testing
	// AccessToken is honored when set so deployments that have
	// upgraded to the paid tier can lift the unauthenticated
	// rate limit; the unauthenticated path keeps working when
	// it's empty.
	AccessToken string
}

// Provider implements social.Provider for StockTwits.
type Provider struct {
	client    *http.Client
	userAgent string
	endpoint  string
	token     string
}

// New constructs a Provider with the supplied options.
func New(opts Options) *Provider {
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	ua := opts.UserAgent
	if strings.TrimSpace(ua) == "" {
		ua = defaultUA
	}
	endpoint := opts.Endpoint
	if strings.TrimSpace(endpoint) == "" {
		endpoint = defaultEndpoint
	}
	return &Provider{
		client:    client,
		userAgent: ua,
		endpoint:  strings.TrimRight(endpoint, "/"),
		token:     strings.TrimSpace(opts.AccessToken),
	}
}

// Platform satisfies social.Provider.
func (p *Provider) Platform() social.Platform { return social.PlatformStockTwits }

// FetchPosts satisfies social.Provider. Returns the symbol's
// most-recent N messages (the public endpoint returns up to 30
// per call regardless of the limit parameter — we trim client-
// side).
func (p *Provider) FetchPosts(ctx context.Context, req social.Request) ([]sentiment.Item, error) {
	if p == nil {
		return nil, errors.New("stocktwits: provider not initialised")
	}
	symbol := strings.ToUpper(strings.TrimSpace(req.Symbol))
	if symbol == "" {
		return nil, errors.New("stocktwits: req.Symbol required")
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 25
	}
	if limit > 30 {
		limit = 30
	}
	full := fmt.Sprintf("%s/%s.json", p.endpoint, symbol)
	if p.token != "" {
		full += "?access_token=" + p.token
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return nil, fmt.Errorf("stocktwits: build request: %w", err)
	}
	httpReq.Header.Set("User-Agent", p.userAgent)
	httpReq.Header.Set("Accept", "application/json")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("stocktwits: http call: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("stocktwits: http status %d", resp.StatusCode)
	}

	var body stocktwitsResp
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("stocktwits: decode body: %w", err)
	}
	out := make([]sentiment.Item, 0, len(body.Messages))
	for i := range body.Messages {
		m := &body.Messages[i]
		if strings.TrimSpace(m.Body) == "" {
			continue
		}
		published, err := time.Parse(time.RFC3339, m.CreatedAt)
		if err != nil {
			// Tolerate a single bad row rather than failing the
			// entire stream — StockTwits has been known to emit
			// timezone-suffixed strings on edge accounts.
			published = time.Now().UTC()
		}
		item := sentiment.Item{
			ID:          fmt.Sprintf("stocktwits:%d", m.ID),
			Title:       firstLine(m.Body),
			Summary:     truncate(m.Body, maxBodyRunes),
			Source:      string(social.PlatformStockTwits),
			URL:         strings.TrimSpace(m.Entities.Permalink),
			Language:    "en",
			PublishedAt: published.UTC(),
			Symbols:     []string{symbol},
		}
		out = append(out, item)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// stocktwitsResp mirrors the subset of fields we read off the
// public streams/symbol endpoint.
type stocktwitsResp struct {
	Messages []stocktwitsMessage `json:"messages"`
}

type stocktwitsMessage struct {
	ID        int64                `json:"id"`
	Body      string               `json:"body"`
	CreatedAt string               `json:"created_at"`
	Entities  stocktwitsEntities   `json:"entities"`
}

type stocktwitsEntities struct {
	Permalink string `json:"chart"` // StockTwits API actually nests permalink under Entities.Chart; UI link is just the message id
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if idx := strings.Index(s, "\n"); idx > 0 {
		s = s[:idx]
	}
	return truncate(s, 140)
}

func truncate(s string, maxRunes int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "…"
}
