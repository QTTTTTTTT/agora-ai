package marketdata

import (
	"context"
	"strings"
)

// LanguageHintZH and LanguageHintEN are the canonical normalized values used
// to drive locale-aware behaviour in news / search providers. Any value that
// resolves to one of these two strings will steer the web-search MCP locale,
// query construction, and (later) translation toggles.
const (
	LanguageHintZH = "zh-CN"
	LanguageHintEN = "en-US"
)

type languageContextKey struct{}

// WithLanguage attaches a normalized language hint to ctx. The value is
// surfaced to marketdata providers via LanguageHint(ctx) and is used to pick
// between zh-CN and en-US locales for the web-search MCP. Unknown / empty
// inputs are dropped so callers can pass through raw header values without
// guarding.
func WithLanguage(ctx context.Context, lang string) context.Context {
	normalized := normalizeLanguageHint(lang)
	if normalized == "" {
		return ctx
	}
	return context.WithValue(ctx, languageContextKey{}, normalized)
}

// LanguageHint returns the normalized language hint previously attached via
// WithLanguage. Returns "" when no hint is present.
func LanguageHint(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	value, _ := ctx.Value(languageContextKey{}).(string)
	return value
}

// normalizeLanguageHint coerces free-form Accept-Language style values (zh,
// zh-CN, zh-Hans, en, en-US, EN_GB, ...) into one of the two canonical
// language hints. Unknown values produce an empty string so providers can
// pick their own defaults.
func normalizeLanguageHint(lang string) string {
	trimmed := strings.ToLower(strings.TrimSpace(lang))
	if trimmed == "" {
		return ""
	}
	switch {
	case strings.HasPrefix(trimmed, "zh"):
		return LanguageHintZH
	case strings.HasPrefix(trimmed, "en"):
		return LanguageHintEN
	}
	return ""
}

// containsCJK reports whether the string contains at least one Han ideograph
// or CJK punctuation rune. It is the auto-detection signal used when no
// explicit language hint is present in ctx.
func containsCJK(s string) bool {
	for _, r := range s {
		switch {
		case r >= 0x4E00 && r <= 0x9FFF: // CJK Unified Ideographs
			return true
		case r >= 0x3400 && r <= 0x4DBF: // CJK Extension A
			return true
		case r >= 0x3000 && r <= 0x303F: // CJK Symbols and Punctuation
			return true
		case r >= 0xFF00 && r <= 0xFFEF: // Halfwidth/Fullwidth Forms
			return true
		}
	}
	return false
}
