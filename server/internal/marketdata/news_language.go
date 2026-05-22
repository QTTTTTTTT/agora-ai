package marketdata

import "strings"

// News language constants used on NewsItem.Language. We intentionally only
// distinguish the two languages we actively support translating between; any
// other locale falls through as empty (the translator may still inspect the
// raw text and infer it).
const (
	NewsLanguageZH = "zh"
	NewsLanguageEN = "en"
)

// detectNewsLanguage inspects the title + summary and returns "zh" if any CJK
// codepoint is present, "en" if the text contains ASCII letters but no CJK,
// and "" if it cannot decide. Used as a fallback when an upstream provider
// does not report the article language explicitly.
func detectNewsLanguage(item NewsItem) string {
	combined := item.Title + " " + item.Summary
	if combined == " " {
		return ""
	}
	if containsCJK(combined) {
		return NewsLanguageZH
	}
	hasLetter := false
	for _, r := range combined {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			hasLetter = true
			break
		}
	}
	if hasLetter {
		return NewsLanguageEN
	}
	return ""
}

// tagNewsItemsWithLanguage populates Language + the matching localized
// title/summary fields for every item. If a provider already supplied a
// localized variant (e.g. titleZh on a translated upstream feed) we keep it.
// Items with explicit hint != "" are tagged with that hint; otherwise we
// auto-detect from the title/summary content.
//
// This is intentionally idempotent: calling it twice does not duplicate work
// or overwrite existing localized fields.
func tagNewsItemsWithLanguage(items []NewsItem, hint string) []NewsItem {
	normalizedHint := strings.ToLower(strings.TrimSpace(hint))
	for i := range items {
		lang := strings.ToLower(strings.TrimSpace(items[i].Language))
		if lang == "" {
			lang = normalizedHint
		}
		if lang == "" {
			lang = detectNewsLanguage(items[i])
		}
		items[i].Language = lang
		switch lang {
		case NewsLanguageZH:
			if strings.TrimSpace(items[i].TitleZh) == "" {
				items[i].TitleZh = items[i].Title
			}
			if strings.TrimSpace(items[i].SummaryZh) == "" {
				items[i].SummaryZh = items[i].Summary
			}
		case NewsLanguageEN:
			if strings.TrimSpace(items[i].TitleEn) == "" {
				items[i].TitleEn = items[i].Title
			}
			if strings.TrimSpace(items[i].SummaryEn) == "" {
				items[i].SummaryEn = items[i].Summary
			}
		}
	}
	return items
}
