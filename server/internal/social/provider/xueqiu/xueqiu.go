// Package xueqiu implements social.Provider against the public
// Xueqiu (snowball, xueqiu.com) per-symbol timeline.
//
// Xueqiu is the dominant retail social network for Chinese
// investors — A-shares, HK, US-listed China names. Their public
// web endpoint (`stock.xueqiu.com/v5/stock/comment/list.json`)
// returns the per-symbol comment stream as JSON when called with
// a session cookie (`xq_a_token`). Without that cookie the
// endpoint returns HTTP 400, so callers must bootstrap a guest
// cookie via Options.GuestCookie OR drop in a session token.
//
// For Xueqiu the symbol code follows a specific format:
//
//   - A-shares Shanghai: SH<6-digit-code> (e.g. SH600519)
//   - A-shares Shenzhen: SZ<6-digit-code> (e.g. SZ000001)
//   - HK: <5-digit-code> (e.g. 00700)
//   - US: bare ticker (e.g. AAPL)
//
// We let the caller pass the already-formatted code; the
// SocialAdapter layer in cmd/server is responsible for mapping
// the internal symbol+market to the Xueqiu form.
//
// Rate-limit posture: Xueqiu rate-limits aggressively when a
// single guest token is used too fast; we DO NOT auto-rotate
// tokens. Production deployments that need higher throughput
// should plug in a session pool via a custom HTTPClient.
package xueqiu

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
	defaultEndpoint = "https://stock.xueqiu.com/v5/stock/comment/list.json"
	defaultUA       = "Mozilla/5.0 (compatible; fundai-platform/1.0)"
	maxBodyRunes    = 280
)

// Options tune the Xueqiu provider.
type Options struct {
	HTTPClient *http.Client
	UserAgent  string
	Endpoint   string // override for testing
	// GuestCookie is the value of the `xq_a_token` cookie a
	// deployment must supply to read public timeline data
	// without a logged-in user. When empty the provider sends
	// no cookie and most requests will fail with 400 — that
	// degradation is intentional (we fail loudly rather than
	// silently dropping the channel).
	GuestCookie string
}

// Provider implements social.Provider for Xueqiu.
type Provider struct {
	client    *http.Client
	userAgent string
	endpoint  string
	cookie    string
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
		endpoint:  endpoint,
		cookie:    strings.TrimSpace(opts.GuestCookie),
	}
}

// Platform satisfies social.Provider.
func (p *Provider) Platform() social.Platform { return social.PlatformXueqiu }

// FetchPosts satisfies social.Provider. Returns the symbol's
// most-recent N comments.
func (p *Provider) FetchPosts(ctx context.Context, req social.Request) ([]sentiment.Item, error) {
	if p == nil {
		return nil, errors.New("xueqiu: provider not initialised")
	}
	symbol := strings.ToUpper(strings.TrimSpace(req.Symbol))
	if symbol == "" {
		return nil, errors.New("xueqiu: req.Symbol required")
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 25
	}
	if limit > 50 {
		limit = 50 // server caps at 50 per page anyway
	}
	q := url.Values{}
	q.Set("symbol", symbol)
	q.Set("count", fmt.Sprintf("%d", limit))
	q.Set("page", "1")
	q.Set("source", "all")
	full := p.endpoint + "?" + q.Encode()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return nil, fmt.Errorf("xueqiu: build request: %w", err)
	}
	httpReq.Header.Set("User-Agent", p.userAgent)
	httpReq.Header.Set("Accept", "application/json")
	if p.cookie != "" {
		httpReq.Header.Set("Cookie", "xq_a_token="+p.cookie)
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("xueqiu: http call: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("xueqiu: http status %d", resp.StatusCode)
	}

	var body xueqiuResp
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("xueqiu: decode body: %w", err)
	}
	if body.ErrorCode != 0 && body.ErrorCode != 0.0 {
		return nil, fmt.Errorf("xueqiu: api error code=%v msg=%q", body.ErrorCode, body.ErrorDescription)
	}
	out := make([]sentiment.Item, 0, len(body.Data.Items))
	for i := range body.Data.Items {
		m := &body.Data.Items[i]
		text := stripHTML(m.Text)
		if strings.TrimSpace(text) == "" {
			continue
		}
		published := time.UnixMilli(m.CreatedAt).UTC()
		permalink := strings.TrimSpace(m.TargetURL)
		if permalink == "" && m.ID > 0 {
			permalink = fmt.Sprintf("https://xueqiu.com/%d/%d", m.UserID, m.ID)
		}
		out = append(out, sentiment.Item{
			ID:          fmt.Sprintf("xueqiu:%d", m.ID),
			Title:       firstLine(text),
			Summary:     truncate(text, maxBodyRunes),
			Source:      string(social.PlatformXueqiu),
			URL:         permalink,
			Language:    "zh",
			PublishedAt: published,
			Symbols:     []string{symbol},
		})
	}
	return out, nil
}

type xueqiuResp struct {
	ErrorCode        any    `json:"error_code"`
	ErrorDescription string `json:"error_description"`
	Data             struct {
		Items []xueqiuItem `json:"items"`
	} `json:"data"`
}

type xueqiuItem struct {
	ID        int64  `json:"id"`
	UserID    int64  `json:"user_id"`
	Text      string `json:"text"`
	CreatedAt int64  `json:"created_at"`
	TargetURL string `json:"target"`
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

// stripHTML removes the surrounding HTML Xueqiu wraps user posts
// in. It's a deliberately conservative scan — we only drop the
// most common tags (<a>, <br>, <img>, plus their closers) rather
// than trying to be a real HTML parser. Anything left in is
// truncated by the rune cap below, so a stray tag won't blow the
// prompt budget.
func stripHTML(s string) string {
	if s == "" {
		return ""
	}
	replacer := strings.NewReplacer(
		"<br/>", "\n",
		"<br>", "\n",
		"<br />", "\n",
		"</p>", "\n",
		"</div>", "\n",
		"</a>", "",
		"</span>", "",
	)
	s = replacer.Replace(s)
	// Strip remaining tags between '<' and '>' for the same defensive reason.
	var b strings.Builder
	b.Grow(len(s))
	inTag := false
	for _, r := range s {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
		case !inTag:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}
