package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFetchSearchResultsParsesRSSFeed(t *testing.T) {
	feedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("q"); got != "AAPL market news when:7d" {
			t.Fatalf("expected query %q, got %q", "AAPL market news when:7d", got)
		}
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
			<rss version="2.0">
			  <channel>
			    <item>
			      <title>AAPL jumps on earnings</title>
			      <link>https://example.com/aapl-earnings</link>
			      <description><![CDATA[<b>Apple</b> beats expectations]]></description>
			      <pubDate>Fri, 15 May 2026 06:00:00 GMT</pubDate>
			      <source>Example Wire</source>
			    </item>
			    <item>
			      <title>AAPL jumps on earnings</title>
			      <link>https://example.com/aapl-earnings</link>
			      <description>duplicate</description>
			      <pubDate>Fri, 15 May 2026 06:00:00 GMT</pubDate>
			      <source>Example Wire</source>
			    </item>
			  </channel>
			</rss>`))
	}))
	defer feedServer.Close()

	server := &searchServer{
		client:      feedServer.Client(),
		feedBaseURL: feedServer.URL,
	}

	results, err := server.fetchSearchResults(context.Background(), "AAPL market news", 5, localeOverrides{})
	if err != nil {
		t.Fatalf("fetch search results: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 deduped result, got %#v", results)
	}
	if results[0].Title != "AAPL jumps on earnings" {
		t.Fatalf("unexpected title: %#v", results[0])
	}
	if results[0].URL != "https://example.com/aapl-earnings" {
		t.Fatalf("unexpected url: %#v", results[0])
	}
	if results[0].Source != "Example Wire" {
		t.Fatalf("unexpected source: %#v", results[0])
	}
	if !results[0].PublishedAt.Equal(time.Date(2026, 5, 15, 6, 0, 0, 0, time.UTC)) {
		t.Fatalf("unexpected published time: %#v", results[0].PublishedAt)
	}
	if !strings.Contains(results[0].Summary, "Apple beats expectations") {
		t.Fatalf("unexpected summary: %#v", results[0].Summary)
	}
}

func TestHandleSearchRequiresQuery(t *testing.T) {
	server := &searchServer{client: http.DefaultClient, feedBaseURL: "https://example.com/rss"}
	req := httptest.NewRequest(http.MethodGet, "/search", nil)
	rr := httptest.NewRecorder()

	server.handleSearch(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d body=%s", http.StatusBadRequest, rr.Code, rr.Body.String())
	}
}

func TestBuildFeedEndpointPreservesExplicitLocale(t *testing.T) {
	endpoint, err := buildFeedEndpoint("https://example.com/rss?hl=zh-CN&gl=CN&ceid=CN:zh-Hans", "比亚迪 A股 新闻", localeOverrides{})
	if err != nil {
		t.Fatalf("build endpoint: %v", err)
	}
	if !strings.Contains(endpoint, "hl=zh-CN") || !strings.Contains(endpoint, "gl=CN") || !strings.Contains(endpoint, "ceid=CN%3Azh-Hans") {
		t.Fatalf("expected explicit locale preserved, got %q", endpoint)
	}
	if !strings.Contains(endpoint, "q=%E6%AF%94%E4%BA%9A%E8%BF%AA+") {
		t.Fatalf("expected query encoded, got %q", endpoint)
	}
}

func TestBuildFeedEndpointHonorsRequestLocaleOverrides(t *testing.T) {
	endpoint, err := buildFeedEndpoint("https://example.com/rss", "贵州茅台", localeOverrides{
		hl:   "zh-CN",
		gl:   "CN",
		ceid: "CN:zh-Hans",
	})
	if err != nil {
		t.Fatalf("build endpoint: %v", err)
	}
	if !strings.Contains(endpoint, "hl=zh-CN") || !strings.Contains(endpoint, "gl=CN") || !strings.Contains(endpoint, "ceid=CN%3Azh-Hans") {
		t.Fatalf("expected request locale applied, got %q", endpoint)
	}
}

func TestBuildFeedEndpointDefaultsToEnglishWhenUnspecified(t *testing.T) {
	endpoint, err := buildFeedEndpoint("https://example.com/rss", "AAPL", localeOverrides{})
	if err != nil {
		t.Fatalf("build endpoint: %v", err)
	}
	if !strings.Contains(endpoint, "hl=en-US") || !strings.Contains(endpoint, "gl=US") || !strings.Contains(endpoint, "ceid=US%3Aen") {
		t.Fatalf("expected en-US defaults, got %q", endpoint)
	}
}

// TestNormalizeSearchResultStripsGoogleNewsRedirectAnchor reproduces the bug
// where Google News' <description> ("<a href="https://news.google.com/rss/articles/CBMi...">Headline</a> Source")
// leaked through stripHTML and surfaced as the news digest "summary". The
// previous tag-by-tag replacement missed <a>, so users saw a raw href in
// what was supposed to be an English/中文 article preview. After the fix
// the summary collapses to empty (since it would only restate the title),
// letting the UI fall back to "open original".
func TestNormalizeSearchResultStripsGoogleNewsRedirectAnchor(t *testing.T) {
	item := rssItem{
		Title:       "Western Digital (WDC) Included in 2026 S&amp;P Dow Jones Best-in-Class Index North America - Yahoo Finance",
		Link:        "https://example.com/wdc",
		Description: `<a href="https://news.google.com/rss/articles/CBMiqAFBVV95cUxNOThKdlZjVFlWWkQtSzBEc1VtSUt5dUU1NHgydVVfYVJjdk83SGh2V0Q0bEY2LVV4QW4xeUhnVzFoWDdiS0ZoWDFGa3lrMVhjelpTNVdvWlZYLWFZQ0FmQTc5OThwb01FRHQ2dTZMRU?oc=5">Western Digital (WDC) Included in 2026 S&amp;P Dow Jones Best-in-Class Index North America</a>&nbsp;&nbsp;<font color="#6f6f6f">Yahoo Finance</font>`,
		PubDate:     "Fri, 15 May 2026 06:00:00 GMT",
		Source:      "Yahoo Finance",
	}
	got := normalizeSearchResult(item)
	if strings.Contains(got.Summary, "<") || strings.Contains(got.Summary, "href=") {
		t.Fatalf("expected summary to be free of HTML, got %q", got.Summary)
	}
	if got.Summary != "" {
		t.Fatalf("expected restated-title summary to be dropped, got %q", got.Summary)
	}
	if !strings.Contains(got.Title, "Western Digital") {
		t.Fatalf("title corrupted: %q", got.Title)
	}
}

// TestNormalizeSearchResultKeepsSubstantiveSummary makes sure we only suppress
// summaries that merely restate the headline. A real summary that adds new
// information must survive the canonicalization heuristic.
func TestNormalizeSearchResultKeepsSubstantiveSummary(t *testing.T) {
	item := rssItem{
		Title:       "MU jumps on memory cycle upgrade",
		Link:        "https://example.com/mu-news",
		Description: `<p>Analysts at <b>Example Capital</b> lifted Micron's price target to $185 citing tightening DRAM supply through 2027.</p>`,
		PubDate:     "Fri, 15 May 2026 06:00:00 GMT",
		Source:      "Example Capital",
	}
	got := normalizeSearchResult(item)
	if got.Summary == "" {
		t.Fatalf("expected substantive summary to survive, got empty")
	}
	if !strings.Contains(got.Summary, "Example Capital") || !strings.Contains(got.Summary, "$185") {
		t.Fatalf("substantive summary lost detail: %q", got.Summary)
	}
	if strings.Contains(got.Summary, "<") {
		t.Fatalf("HTML survived strip: %q", got.Summary)
	}
}

func TestSummaryIsTitleRestatementCJK(t *testing.T) {
	// The Chinese-locale RSS has an identical structure. Make sure the
	// canonicalizer doesn't trip over CJK characters.
	if !summaryIsTitleRestatement("比亚迪 一季度净利大增", "比亚迪一季度净利大增") {
		t.Fatal("expected restatement to match across whitespace")
	}
	if summaryIsTitleRestatement("分析师上调比亚迪目标价至 320 元，理由是动力电池毛利率回升", "比亚迪一季度净利大增") {
		t.Fatal("expected substantive Chinese summary to survive")
	}
}
