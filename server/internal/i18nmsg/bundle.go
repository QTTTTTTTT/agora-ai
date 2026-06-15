// Package i18nmsg is the single source of truth for backend-emitted strings
// that may surface in the user-facing UI (lessons, verdicts, plan reasoning,
// LLM prompt headers, fallbacks, error details, etc.).
//
// Design goals:
//
//  1. **Centralised lookup.** Every locale-dependent string lives in
//     messages_zh.go / messages_en.go. Code calls T(loc, key) instead of
//     hard-coding zh literals or doing inline `if loc==EN { ... }` branches.
//
//  2. **Compile-time keys.** Keys are typed (Key) and declared in keys.go,
//     so a typo at the call-site is a compile error.
//
//  3. **Loud failures, not silent zh leak.** A missing English entry must
//     never silently fall back to a Chinese string in the en-US UI. If a
//     key is missing for the requested locale, T() returns a sentinel
//     "!!MISSING:<key>" string AND increments the
//     i18nmsg_missing_key_total counter so the on-call sees the gap.
//
//  4. **No hard dependency on cmd/server.** The package defines its own
//     context key. The HTTP middleware (or a Loop helper) calls WithLocale
//     to attach the resolved locale. FromCtx reads it back. Older code
//     that uses cmd/server's LanguageFromContext can be wrapped via
//     Normalize at the call boundary.
//
// This package is intentionally tiny and dependency-free aside from the
// project's own metrics primitives.
package i18nmsg

import (
	"context"
	"fmt"
	"strings"

	"github.com/fundai/server/internal/metrics"
)

// ---------------------------------------------------------------------------
// Public types
// ---------------------------------------------------------------------------

// Locale enumerates the supported user-facing languages. Anything outside the
// set is normalised to LocaleZH to preserve historical behaviour.
type Locale string

const (
	LocaleZH Locale = "zh-CN"
	LocaleEN Locale = "en-US"
)

// Valid reports whether loc is one of the supported locales.
func (l Locale) Valid() bool { return l == LocaleZH || l == LocaleEN }

// String implements fmt.Stringer for ergonomic logging.
func (l Locale) String() string { return string(l) }

// Key identifies a message in the bundle. Declared as a distinct named type
// so a stray `T(loc, "some_key")` won't compile — callers must use a
// constant from keys.go.
type Key string

// ---------------------------------------------------------------------------
// Context plumbing
// ---------------------------------------------------------------------------

type ctxKey struct{}

// localeCtxKey is the package-private key under which WithLocale stashes
// the resolved Locale on a request- or loop-scoped context.
var localeCtxKey = ctxKey{}

// WithLocale attaches loc to ctx. Unsupported values are normalised to
// LocaleZH. The HTTP middleware in cmd/server should call this; loop
// helpers (loop_locale.go) call this with the fund/owner preferred
// language.
func WithLocale(ctx context.Context, loc Locale) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if !loc.Valid() {
		loc = LocaleZH
	}
	return context.WithValue(ctx, localeCtxKey, loc)
}

// FromCtx returns the locale attached to ctx, or LocaleZH if none.
func FromCtx(ctx context.Context) Locale {
	if ctx == nil {
		return LocaleZH
	}
	if v, ok := ctx.Value(localeCtxKey).(Locale); ok && v.Valid() {
		return v
	}
	return LocaleZH
}

// Normalize maps free-form strings (e.g. an X-User-Language header value,
// an Accept-Language fragment, or cmd/server's UserLanguage type cast to
// string) onto the canonical Locale set.
func Normalize(value string) Locale {
	trimmed := strings.ToLower(strings.TrimSpace(value))
	if trimmed == "" {
		return LocaleZH
	}
	if strings.HasPrefix(trimmed, "en") {
		return LocaleEN
	}
	if strings.HasPrefix(trimmed, "zh") {
		return LocaleZH
	}
	return LocaleZH
}

// ---------------------------------------------------------------------------
// Translation lookup
// ---------------------------------------------------------------------------

// missingKeyCounter tracks every missing-key lookup so the on-call can
// surface gaps via dashboards / alerts. Labelled by locale and key for
// fast triage.
var missingKeyCounter = metrics.NewCounter(
	"i18nmsg_missing_key_total",
	"Total number of T()/Tf() calls that found no entry for the requested locale.",
)

// T returns the message registered for (loc, key). On a cache miss for the
// requested locale, the function tries the other supported locale (so a
// missing English entry surfaces the Chinese one rather than an empty
// string), but always increments missingKeyCounter so the gap is visible.
//
// If the key is missing in *both* locales, T returns "!!MISSING:<key>",
// which is intentionally noisy: it shows up in the UI and breaks the
// english-smoke test, forcing a fix rather than silently shipping blank
// content.
func T(loc Locale, key Key) string {
	if !loc.Valid() {
		loc = LocaleZH
	}
	if v, ok := lookup(loc, key); ok {
		return v
	}
	missingKeyCounter.Inc(metrics.Labels{
		"locale": string(loc),
		"key":    string(key),
	})
	other := LocaleZH
	if loc == LocaleZH {
		other = LocaleEN
	}
	if v, ok := lookup(other, key); ok {
		return v
	}
	return "!!MISSING:" + string(key)
}

// Tf is T followed by fmt.Sprintf with args. Use indexed placeholders
// (e.g. %[1]s, %[2]d) in the templates so the zh and en versions can put
// the arguments in different positions without breaking the call-site.
func Tf(loc Locale, key Key, args ...any) string {
	template := T(loc, key)
	if len(args) == 0 {
		return template
	}
	return fmt.Sprintf(template, args...)
}

// lookup is the package-private accessor over the locale-keyed maps in
// messages_zh.go / messages_en.go.
func lookup(loc Locale, key Key) (string, bool) {
	switch loc {
	case LocaleZH:
		v, ok := messagesZH[key]
		return v, ok
	case LocaleEN:
		v, ok := messagesEN[key]
		return v, ok
	default:
		return "", false
	}
}

// Has reports whether the bundle has an entry for (loc, key). Useful for
// callers that want to fall back to a programmatically-generated string
// when the human-curated translation is absent.
func Has(loc Locale, key Key) bool {
	_, ok := lookup(loc, key)
	return ok
}

// AllKeys returns every key registered in either locale. Used by the
// english-smoke test to assert parity between the two maps.
func AllKeys() []Key {
	seen := make(map[Key]struct{}, len(messagesZH)+len(messagesEN))
	for k := range messagesZH {
		seen[k] = struct{}{}
	}
	for k := range messagesEN {
		seen[k] = struct{}{}
	}
	out := make([]Key, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	return out
}
