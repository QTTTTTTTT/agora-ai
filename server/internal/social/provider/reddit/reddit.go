// Package reddit implements social.Provider against the public
// r/wallstreetbets feed using Reddit's JSON listing endpoint.
//
// We use the JSON-suffix trick (`/r/wallstreetbets/hot.json?...`)
// instead of the OAuth API because:
//
//   - It needs zero credentials, so single-binary deployments and
//     OSS-style installs work out of the box.
//   - The data we need (title, selftext snippet, score, created_utc,
//     permalink) is identical between the JSON listing and the
//     OAuth endpoint.
//   - Rate limits on the JSON endpoint are friendly (1 req / 2s
//     by default), more than enough for the daily PM loop.
//
// We DO send a descriptive User-Agent because Reddit blocks
// requests with the default Go HTTP UA on the JSON endpoint;
// callers can override it via Options.UserAgent.
//
// The provider scopes posts to a single symbol by running the
// subreddit's search endpoint with `q=$<symbol>` + `restrict_sr=on`.
// Selftext snippets are truncated at 280 runes so a viral post
// with a wall of text doesn't blow the prompt budget downstream.
package reddit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/fundai/server/internal/sentiment"
	"github.com/fundai/server/internal/social"
)

const (
	defaultEndpoint  = "https://www.reddit.com/r/wallstreetbets/search.json"
	defaultUserAgent = "fundai-platform/1.0 (sentiment-analyst social ingest)"
	maxBodyRunes     = 280
)

// Options tune the Reddit provider. All fields are optional and
// override the package-level defaults.
type Options struct {
	HTTPClient  *http.Client
	UserAgent   string
	Endpoint    string // override for testing
	Subreddit   string // default "wallstreetbets"
	MinUpvotes  int    // posts below this score are dropped; 0 = no filter
}

// Provider implements social.Provider for r/wallstreetbets.
type Provider struct {
	client    *http.Client
	userAgent string
	endpoint  string
	subreddit string
	minScore  int
}

// New constructs a Provider with the given options. Pass the zero
// value to get sensible defaults (no auth, friendly UA, 10s
// per-call HTTP timeout, no upvote filter).
func New(opts Options) *Provider {
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	ua := opts.UserAgent
	if strings.TrimSpace(ua) == "" {
		ua = defaultUserAgent
	}
	endpoint := opts.Endpoint
	if strings.TrimSpace(endpoint) == "" {
		endpoint = defaultEndpoint
	}
	sub := opts.Subreddit
	if strings.TrimSpace(sub) == "" {
		sub = "wallstreetbets"
	}
	return &Provider{
		client:    client,
		userAgent: ua,
		endpoint:  endpoint,
		subreddit: sub,
		minScore:  opts.MinUpvotes,
	}
}

// Platform satisfies social.Provider.
func (p *Provider) Platform() social.Platform { return social.PlatformRedditWSB }

// FetchPosts satisfies social.Provider. Returns posts matching
// the symbol query, sorted by relevance (Reddit's `sort=new`
// returns timestamp-ordered, which is what we want for daily
// freshness).
func (p *Provider) FetchPosts(ctx context.Context, req social.Request) ([]sentiment.Item, error) {
	if p == nil {
		return nil, errors.New("reddit: provider not initialised")
	}
	symbol := strings.TrimSpace(req.Symbol)
	if symbol == "" {
		return nil, errors.New("reddit: req.Symbol required")
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 25
	}
	if limit > 100 {
		limit = 100 // Reddit hard cap per request
	}
	q := url.Values{}
	// "$AAPL OR AAPL" — cashtag-prefixed mentions are more
	// reliably symbol-specific (filters out "aapl is delicious"
	// posts on a fruit subreddit etc.) but bare-symbol is the
	// usual WSB style for tickers in titles.
	q.Set("q", fmt.Sprintf("$%s OR %s", symbol, symbol))
	q.Set("restrict_sr", "on")
	q.Set("sort", "new")
	q.Set("limit", fmt.Sprintf("%d", limit))
	q.Set("t", "day")

	uri := strings.Replace(p.endpoint, "wallstreetbets", p.subreddit, 1)
	full := uri + "?" + q.Encode()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return nil, fmt.Errorf("reddit: build request: %w", err)
	}
	httpReq.Header.Set("User-Agent", p.userAgent)
	httpReq.Header.Set("Accept", "application/json")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("reddit: http call: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("reddit: http status %d", resp.StatusCode)
	}

	var body redditListing
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("reddit: decode body: %w", err)
	}
	items := make([]sentiment.Item, 0, len(body.Data.Children))
	for _, c := range body.Data.Children {
		post := c.Data
		if p.minScore > 0 && post.Score < p.minScore {
			continue
		}
		if strings.TrimSpace(post.Title) == "" {
			continue
		}
		published := time.Unix(int64(post.CreatedUTC), 0).UTC()
		permalink := "https://www.reddit.com" + strings.TrimSpace(post.Permalink)
		items = append(items, sentiment.Item{
			ID:          "reddit:" + strings.TrimSpace(post.ID),
			Title:       strings.TrimSpace(post.Title),
			Summary:     truncate(post.SelfText, maxBodyRunes),
			Source:      string(social.PlatformRedditWSB),
			URL:         permalink,
			Language:    "en",
			PublishedAt: published,
			Symbols:     []string{strings.ToUpper(symbol)},
		})
	}
	return items, nil
}

type redditListing struct {
	Data struct {
		Children []struct {
			Data redditPost `json:"data"`
		} `json:"children"`
	} `json:"data"`
}

type redditPost struct {
	ID         string  `json:"id"`
	Title      string  `json:"title"`
	SelfText   string  `json:"selftext"`
	Permalink  string  `json:"permalink"`
	Score      int     `json:"score"`
	CreatedUTC float64 `json:"created_utc"`
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
