package marketdata

import (
	"testing"
	"time"
)

func TestDetectNewsLanguage(t *testing.T) {
	cases := []struct {
		name string
		item NewsItem
		want string
	}{
		{name: "chinese only", item: NewsItem{Title: "贵州茅台一季度净利增长"}, want: NewsLanguageZH},
		{name: "english only", item: NewsItem{Title: "Apple earnings beat estimates"}, want: NewsLanguageEN},
		{name: "mixed prefers cjk", item: NewsItem{Title: "Apple 苹果发布新机"}, want: NewsLanguageZH},
		{name: "numeric only", item: NewsItem{Title: "12345"}, want: ""},
		{name: "empty", item: NewsItem{}, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := detectNewsLanguage(tc.item); got != tc.want {
				t.Fatalf("detectNewsLanguage = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTagNewsItemsWithLanguageFillsZhVariant(t *testing.T) {
	items := []NewsItem{{Title: "贵州茅台一季度净利增长", Summary: "公司公告称…"}}
	tagged := tagNewsItemsWithLanguage(items, NewsLanguageZH)
	if tagged[0].Language != NewsLanguageZH {
		t.Fatalf("expected language=zh, got %q", tagged[0].Language)
	}
	if tagged[0].TitleZh != "贵州茅台一季度净利增长" {
		t.Fatalf("expected titleZh populated, got %q", tagged[0].TitleZh)
	}
	if tagged[0].TitleEn != "" {
		t.Fatalf("expected titleEn empty, got %q", tagged[0].TitleEn)
	}
}

func TestTagNewsItemsWithLanguageAutoDetectsWhenHintEmpty(t *testing.T) {
	items := []NewsItem{
		{Title: "Apple beats earnings"},
		{Title: "贵州茅台一季度净利增长"},
	}
	tagged := tagNewsItemsWithLanguage(items, "")
	if tagged[0].Language != NewsLanguageEN || tagged[0].TitleEn != "Apple beats earnings" {
		t.Fatalf("expected first item tagged en, got language=%q titleEn=%q", tagged[0].Language, tagged[0].TitleEn)
	}
	if tagged[1].Language != NewsLanguageZH || tagged[1].TitleZh != "贵州茅台一季度净利增长" {
		t.Fatalf("expected second item tagged zh, got language=%q titleZh=%q", tagged[1].Language, tagged[1].TitleZh)
	}
}

func TestTagNewsItemsWithLanguageIsIdempotent(t *testing.T) {
	items := []NewsItem{{Title: "Apple beats earnings"}}
	first := tagNewsItemsWithLanguage(items, NewsLanguageEN)
	second := tagNewsItemsWithLanguage(first, NewsLanguageEN)
	if second[0].TitleEn != "Apple beats earnings" {
		t.Fatalf("idempotent tag should preserve titleEn, got %q", second[0].TitleEn)
	}
}

func TestCanonicalNewsKeyStripsSchemeAndQuery(t *testing.T) {
	a := NewsItem{URL: "https://www.example.com/news/123?utm=foo"}
	b := NewsItem{URL: "HTTP://example.com/news/123/?utm=bar"}
	if canonicalNewsKey(a) != canonicalNewsKey(b) {
		t.Fatalf("expected canonical keys to match, got %q vs %q", canonicalNewsKey(a), canonicalNewsKey(b))
	}
}

func TestCanonicalNewsKeyFallsBackToTitle(t *testing.T) {
	item := NewsItem{Title: "  Apple  Beats  Earnings  "}
	got := canonicalNewsKey(item)
	want := "apple  beats  earnings"
	if got != want {
		t.Fatalf("expected trimmed lowercased title key %q, got %q", want, got)
	}
}

func TestMergeHybridNewsDedupesByURL(t *testing.T) {
	now := time.Now().UTC()
	primary := []NewsItem{
		{Title: "ZH one", URL: "https://example.com/a", PublishedAt: now.Add(-1 * time.Hour)},
	}
	secondary := []NewsItem{
		{Title: "EN dup", URL: "https://www.example.com/a/?utm=tracker", PublishedAt: now},
		{Title: "EN two", URL: "https://example.com/b", PublishedAt: now.Add(-2 * time.Hour)},
	}
	merged := mergeHybridNews(primary, secondary, 5)
	if len(merged) != 2 {
		t.Fatalf("expected 2 unique items, got %d (%v)", len(merged), titles(merged))
	}
	if merged[0].Title != "ZH one" {
		t.Fatalf("expected primary first, got %v", titles(merged))
	}
}

func TestMergeHybridNewsSortsByPublishedAt(t *testing.T) {
	now := time.Now().UTC()
	primary := []NewsItem{
		{Title: "ZH old", URL: "https://a.example/1", PublishedAt: now.Add(-3 * time.Hour)},
	}
	secondary := []NewsItem{
		{Title: "EN new", URL: "https://b.example/2", PublishedAt: now.Add(-30 * time.Minute)},
	}
	merged := mergeHybridNews(primary, secondary, 5)
	if merged[0].Title != "EN new" {
		t.Fatalf("expected most recent first, got %v", titles(merged))
	}
}

func TestMergeHybridNewsRespectsLimit(t *testing.T) {
	primary := []NewsItem{
		{Title: "p1", URL: "https://a.example/1"},
		{Title: "p2", URL: "https://a.example/2"},
		{Title: "p3", URL: "https://a.example/3"},
	}
	secondary := []NewsItem{
		{Title: "s1", URL: "https://b.example/1"},
	}
	merged := mergeHybridNews(primary, secondary, 2)
	if len(merged) != 2 {
		t.Fatalf("expected limit=2, got %d", len(merged))
	}
}

// TestMergeHybridNewsReservesSecondaryQuota guards against the regression
// where a rich primary chain (e.g. 6 native A-share items) could lock the
// secondary chain (English macro coverage) out of every slot, defeating the
// whole point of hybrid aggregation.
func TestMergeHybridNewsReservesSecondaryQuota(t *testing.T) {
	primary := make([]NewsItem, 6)
	for i := range primary {
		primary[i] = NewsItem{Title: "zh", URL: "https://a.example/p" + string(rune('0'+i)), Language: NewsLanguageZH}
	}
	secondary := []NewsItem{
		{Title: "en1", URL: "https://b.example/1", Language: NewsLanguageEN},
		{Title: "en2", URL: "https://b.example/2", Language: NewsLanguageEN},
	}
	merged := mergeHybridNews(primary, secondary, 6)
	if len(merged) != 6 {
		t.Fatalf("expected 6 merged items, got %d", len(merged))
	}
	enCount := 0
	for _, item := range merged {
		if item.Language == NewsLanguageEN {
			enCount++
		}
	}
	if enCount == 0 {
		t.Fatalf("secondary chain produced 2 items but none made it into the merged result")
	}
}

// TestMergeHybridNewsFallsBackWhenSecondaryEmpty ensures the reserved-quota
// logic doesn't waste capacity when there is nothing to fill it with.
func TestMergeHybridNewsFallsBackWhenSecondaryEmpty(t *testing.T) {
	primary := make([]NewsItem, 5)
	for i := range primary {
		primary[i] = NewsItem{Title: "zh", URL: "https://a.example/p" + string(rune('0'+i))}
	}
	merged := mergeHybridNews(primary, nil, 5)
	if len(merged) != 5 {
		t.Fatalf("expected 5 merged items (no quota waste), got %d", len(merged))
	}
}

func TestPrimarySecondaryNewsLanguageDefaultsToEnglish(t *testing.T) {
	zhInstr := InstrumentRef{Symbol: "600000", Market: "cnstock"}
	usInstr := InstrumentRef{Symbol: "AAPL", Market: "us_equity"}
	cryptoInstr := InstrumentRef{Symbol: "BTC-USD", AssetClass: "crypto"}

	if p, s := primarySecondaryNewsLanguage(zhInstr); p != NewsLanguageZH || s != NewsLanguageEN {
		t.Fatalf("cnstock should be zh,en got %q,%q", p, s)
	}
	if p, s := primarySecondaryNewsLanguage(usInstr); p != NewsLanguageEN || s != NewsLanguageZH {
		t.Fatalf("us_equity should be en,zh got %q,%q", p, s)
	}
	if p, s := primarySecondaryNewsLanguage(cryptoInstr); p != NewsLanguageEN || s != NewsLanguageZH {
		t.Fatalf("crypto should be en,zh got %q,%q", p, s)
	}
}

func titles(items []NewsItem) []string {
	out := make([]string, len(items))
	for i, item := range items {
		out[i] = item.Title
	}
	return out
}
