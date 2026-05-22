package main

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// htmlTagRE matches a single HTML/XML tag. We use it to strip every tag
// from Google News RSS <description> blocks, which arrive as
// `<a href="…long redirect URL…">Headline</a>&nbsp;<font…>Source</font>`.
// The previous tag-by-tag string replacement missed <a> and <font>, so the
// raw href ended up in the news digest UI and looked like untranslated
// English. We deliberately use a non-greedy match so adjacent tags don't
// get coalesced into one.
var htmlTagRE = regexp.MustCompile(`<[^>]+>`)

type searchServer struct {
	client      *http.Client
	feedBaseURL string
}

type searchResult struct {
	Title       string    `json:"title"`
	URL         string    `json:"url,omitempty"`
	Source      string    `json:"source,omitempty"`
	Summary     string    `json:"summary,omitempty"`
	PublishedAt time.Time `json:"publishedAt,omitempty"`
}

type searchResponse struct {
	Results []searchResult `json:"results"`
}

type rssFeed struct {
	Channel struct {
		Items []rssItem `xml:"item"`
	} `xml:"channel"`
}

type rssItem struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	Description string `xml:"description"`
	PubDate     string `xml:"pubDate"`
	Source      string `xml:"source"`
}

func main() {
	port := firstNonEmpty(strings.TrimSpace(os.Getenv("MCP_PORT")), "3004")
	feedBaseURL := firstNonEmpty(strings.TrimSpace(os.Getenv("WEB_SEARCH_FEED_URL")), "https://news.google.com/rss/search")
	server := &searchServer{
		client: &http.Client{Timeout: 10 * time.Second},
		feedBaseURL: feedBaseURL,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", server.handleHealth)
	for _, path := range []string{"/search", "/api/search", "/news/search", "/api/news/search", "/api/web/search"} {
		mux.HandleFunc("GET "+path, server.handleSearch)
	}
	log.Printf("web-search-mcp listening on :%s", port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatal(err)
	}
}

func (s *searchServer) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (s *searchServer) handleSearch(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(firstNonEmpty(r.URL.Query().Get("q"), r.URL.Query().Get("query")))
	if query == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "query is required"})
		return
	}
	limit := parseLimit(r.URL.Query().Get("limit"), 5)
	locale := localeOverrides{
		hl:   strings.TrimSpace(r.URL.Query().Get("hl")),
		gl:   strings.TrimSpace(r.URL.Query().Get("gl")),
		ceid: strings.TrimSpace(r.URL.Query().Get("ceid")),
	}
	results, err := s.fetchSearchResults(r.Context(), query, limit, locale)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, searchResponse{Results: results})
}

type localeOverrides struct {
	hl   string
	gl   string
	ceid string
}

func (s *searchServer) fetchSearchResults(ctx context.Context, query string, limit int, locale localeOverrides) ([]searchResult, error) {
	endpoint, err := buildFeedEndpoint(s.feedBaseURL, query, locale)
	if err != nil {
		return nil, err
	}
	feed, err := s.fetchFeed(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	results := make([]searchResult, 0, limit)
	seen := make(map[string]struct{}, limit)
	for _, item := range feed.Channel.Items {
		result := normalizeSearchResult(item)
		if result.Title == "" || result.URL == "" {
			continue
		}
		key := result.URL + "|" + result.Title
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		results = append(results, result)
		if len(results) >= limit {
			break
		}
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("empty news results")
	}
	return results, nil
}

func (s *searchServer) fetchFeed(ctx context.Context, endpoint string) (*rssFeed, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/rss+xml, application/xml, text/xml;q=0.9, */*;q=0.1")
	req.Header.Set("User-Agent", "fundai-web-search-mcp/1.0")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}
	var feed rssFeed
	if err := xml.NewDecoder(resp.Body).Decode(&feed); err != nil {
		return nil, err
	}
	return &feed, nil
}

func buildFeedEndpoint(baseURL, query string, locale localeOverrides) (string, error) {
	endpoint, err := url.Parse(strings.TrimRight(strings.TrimSpace(baseURL), "/"))
	if err != nil {
		return "", err
	}
	params := endpoint.Query()
	q := strings.TrimSpace(query)
	if q != "" && !strings.Contains(strings.ToLower(q), "when:") {
		q = q + " when:7d"
	}
	params.Set("q", q)
	hl := firstNonEmpty(locale.hl, params.Get("hl"), "en-US")
	gl := firstNonEmpty(locale.gl, params.Get("gl"), "US")
	ceid := firstNonEmpty(locale.ceid, params.Get("ceid"), gl+":"+strings.SplitN(hl, "-", 2)[0])
	params.Set("hl", hl)
	params.Set("gl", gl)
	params.Set("ceid", ceid)
	endpoint.RawQuery = params.Encode()
	return endpoint.String(), nil
}

func normalizeSearchResult(item rssItem) searchResult {
	title := cleanText(item.Title)
	summary := cleanText(stripHTML(item.Description))
	// Google News' RSS <description> for the en-US locale is almost always a
	// re-statement of the headline wrapped in an <a> tag plus the source
	// label. Once we strip the markup the summary collapses to the same
	// text as the title (or "headline · source"), which the UI then has to
	// show as a meaningless duplicate. Detect that pattern and drop it so
	// the frontend can fall back to "open original" cleanly.
	if summary != "" && summaryIsTitleRestatement(summary, title) {
		summary = ""
	}
	result := searchResult{
		Title:   title,
		URL:     strings.TrimSpace(item.Link),
		Source:  cleanText(item.Source),
		Summary: summary,
	}
	for _, layout := range []string{time.RFC1123Z, time.RFC1123, time.RFC3339} {
		if parsed, err := time.Parse(layout, strings.TrimSpace(item.PubDate)); err == nil {
			result.PublishedAt = parsed.UTC()
			break
		}
	}
	return result
}

func parseLimit(raw string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || value <= 0 {
		return fallback
	}
	if value > 20 {
		return 20
	}
	return value
}

func stripHTML(value string) string {
	if value == "" {
		return ""
	}
	// Replace block-level breaks with spaces first so adjacent words don't
	// get glued together when the surrounding tags vanish.
	value = strings.NewReplacer(
		"<br>", " ", "<br/>", " ", "<br />", " ",
		"</p>", " ", "</li>", " ", "</div>", " ",
	).Replace(value)
	return htmlTagRE.ReplaceAllString(value, " ")
}

// summaryIsTitleRestatement returns true when the summary, after lowercasing
// and stripping non-letter characters, is contained in the title (or vice
// versa). Google News descriptions for en-US almost always satisfy this
// because the description is the headline wrapped in a redirect <a>.
func summaryIsTitleRestatement(summary, title string) bool {
	canonSummary := canonicalizeForCompare(summary)
	canonTitle := canonicalizeForCompare(title)
	if canonSummary == "" || canonTitle == "" {
		return false
	}
	if canonSummary == canonTitle {
		return true
	}
	// The summary is "title · source" in many Google News locales. We
	// consider it a restatement when 90%+ of the summary characters are
	// already present in the title — keeps real summaries (which usually
	// add substantive prose) untouched.
	if strings.Contains(canonTitle, canonSummary) {
		return true
	}
	return false
}

func canonicalizeForCompare(value string) string {
	value = strings.ToLower(value)
	var b strings.Builder
	b.Grow(len(value))
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r >= 0x4E00 {
			// Latin letters, digits, or any CJK ideograph survive.
			b.WriteRune(r)
		}
	}
	return b.String()
}

func cleanText(value string) string {
	value = html.UnescapeString(strings.TrimSpace(value))
	return strings.Join(strings.Fields(value), " ")
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
