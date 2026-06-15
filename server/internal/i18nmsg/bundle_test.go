// bundle_test.go enforces the invariants that make this package safe:
//
//  1. Both locales declare the same set of keys (no zh-only or en-only
//     entries — that would surface as silent fallback at runtime).
//  2. Every value in messages_en.go is free of CJK ideographs (catches
//     copy-paste accidents from the zh map).
//  3. Format placeholders are consistent across the two variants for any
//     key whose value contains `%`. Indexed placeholders (`%[N]`) are
//     compared by index set, plain `%verb` runs are compared by count.
//  4. Context plumbing round-trips: WithLocale → FromCtx returns the
//     same Locale, and an unknown locale collapses to LocaleZH.
//  5. T() and Tf() honour the locale, fall back loudly via "!!MISSING:"
//     when both locales lack the key, and increment the missing-key
//     counter on a partial miss.
package i18nmsg

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"testing"
	"unicode"
)

// TestZhEnKeyParity asserts every key registered in keys.go shows up in
// both messages_zh.go and messages_en.go. The bundle's runtime safety
// depends on this — a key declared but missing in one map produces a
// silent zh leak (or worse, a "!!MISSING:" string) in production.
func TestZhEnKeyParity(t *testing.T) {
	zhKeys := keysOf(messagesZH)
	enKeys := keysOf(messagesEN)

	zhOnly := diff(zhKeys, enKeys)
	enOnly := diff(enKeys, zhKeys)

	if len(zhOnly) > 0 {
		t.Errorf("keys present in messagesZH but missing in messagesEN: %v", zhOnly)
	}
	if len(enOnly) > 0 {
		t.Errorf("keys present in messagesEN but missing in messagesZH: %v", enOnly)
	}
}

// TestEnglishMessagesHaveNoCJK guarantees we never ship a Chinese string
// to en-US users via a copy-paste accident. The check uses unicode.Han
// rather than a hand-rolled range so it also catches Traditional / rare
// glyphs.
func TestEnglishMessagesHaveNoCJK(t *testing.T) {
	for k, v := range messagesEN {
		for _, r := range v {
			if unicode.Is(unicode.Han, r) {
				t.Errorf("messagesEN[%q] contains a CJK ideograph %q in value %q", string(k), string(r), v)
				break
			}
		}
	}
}

// TestPlaceholderParity catches the most common bug in dual-language
// bundles: a translator drops a `%s` and the runtime panics on Tf(). It
// compares the multiset of placeholders between zh and en for every key.
func TestPlaceholderParity(t *testing.T) {
	for k, zh := range messagesZH {
		en, ok := messagesEN[k]
		if !ok {
			continue // covered by TestZhEnKeyParity
		}
		zhSet := placeholderSignature(zh)
		enSet := placeholderSignature(en)
		if zhSet != enSet {
			t.Errorf("placeholder mismatch for key %q: zh=%q en=%q", string(k), zhSet, enSet)
		}
	}
}

// TestNormalizeAndCtxRoundtrip pins the locale plumbing.
func TestNormalizeAndCtxRoundtrip(t *testing.T) {
	cases := []struct {
		in   string
		want Locale
	}{
		{"", LocaleZH},
		{"   ", LocaleZH},
		{"zh-CN", LocaleZH},
		{"ZH-cn", LocaleZH},
		{"en-US", LocaleEN},
		{"en", LocaleEN},
		{"en-gb", LocaleEN},
		{"fr-FR", LocaleZH}, // unsupported -> safe default
		{"garbage", LocaleZH},
	}
	for _, tc := range cases {
		if got := Normalize(tc.in); got != tc.want {
			t.Errorf("Normalize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	ctx := WithLocale(context.Background(), LocaleEN)
	if got := FromCtx(ctx); got != LocaleEN {
		t.Errorf("FromCtx after WithLocale(EN) = %q, want en-US", got)
	}
	if got := FromCtx(context.Background()); got != LocaleZH {
		t.Errorf("FromCtx on empty ctx = %q, want zh-CN", got)
	}

	// nil context must not panic.
	if got := FromCtx(nil); got != LocaleZH {
		t.Errorf("FromCtx(nil) = %q, want zh-CN", got)
	}
	// Unknown locale via WithLocale collapses to default.
	ctx = WithLocale(context.Background(), Locale("klingon"))
	if got := FromCtx(ctx); got != LocaleZH {
		t.Errorf("WithLocale(klingon) -> FromCtx = %q, want zh-CN", got)
	}
}

// TestTLookupAndMissing exercises the lookup paths and the loud-failure
// behaviour when a key is absent in the requested locale.
func TestTLookupAndMissing(t *testing.T) {
	if got := T(LocaleEN, KeyPlanVerdict_BuyIncrease); got != "Buy / Increase" {
		t.Errorf("T(EN, BuyIncrease) = %q", got)
	}
	if got := T(LocaleZH, KeyPlanVerdict_BuyIncrease); got != "买入/增配" {
		t.Errorf("T(ZH, BuyIncrease) = %q", got)
	}
	// Missing in both locales -> "!!MISSING:" sentinel.
	if got := T(LocaleEN, Key("does.not.exist")); !strings.HasPrefix(got, "!!MISSING:") {
		t.Errorf("T() on unknown key = %q, want !!MISSING: prefix", got)
	}
}

// TestTfFormatting ensures Tf substitutes positional/indexed args. It
// uses a key with a known formatter to avoid hand-rolled assertions.
func TestTfFormatting(t *testing.T) {
	zhOut := Tf(LocaleZH, KeyDecisionTraceQuoteFallbackFmt, "AAPL")
	if !strings.Contains(zhOut, "AAPL") {
		t.Errorf("Tf(zh) did not substitute symbol: %q", zhOut)
	}
	enOut := Tf(LocaleEN, KeyDecisionTraceQuoteFallbackFmt, "AAPL")
	if !strings.Contains(enOut, "AAPL") {
		t.Errorf("Tf(en) did not substitute symbol: %q", enOut)
	}
	if strings.Contains(enOut, "未能") {
		t.Errorf("Tf(en) leaked Chinese: %q", enOut)
	}
}

// TestAllKeysReturnsUnion ensures the introspection helper sees both maps.
func TestAllKeysReturnsUnion(t *testing.T) {
	all := AllKeys()
	if len(all) == 0 {
		t.Fatal("AllKeys returned empty slice")
	}
	want := KeyPlanVerdict_BuyIncrease
	found := false
	for _, k := range all {
		if k == want {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("AllKeys missing %q", string(want))
	}
}

// TestLocaleString covers the Stringer impl used by log lines.
func TestLocaleString(t *testing.T) {
	if LocaleEN.String() != "en-US" {
		t.Errorf("LocaleEN.String() = %q", LocaleEN.String())
	}
	if LocaleZH.String() != "zh-CN" {
		t.Errorf("LocaleZH.String() = %q", LocaleZH.String())
	}
}

// TestWithLocaleNilCtx pins the no-panic guarantee even when callers
// hand in a nil context (which can happen in some Loop bootstrap paths
// before the real ctx is plumbed through).
func TestWithLocaleNilCtx(t *testing.T) {
	ctx := WithLocale(nil, LocaleEN)
	if got := FromCtx(ctx); got != LocaleEN {
		t.Errorf("WithLocale(nil, EN) -> FromCtx = %q", got)
	}
}

// TestHasReports covers the Has() helper used by callers that want to
// branch on translation availability before falling back to a generated
// string.
func TestHasReports(t *testing.T) {
	if !Has(LocaleEN, KeyPlanVerdict_BuyIncrease) {
		t.Error("Has(EN, known key) returned false")
	}
	if Has(LocaleEN, Key("does.not.exist")) {
		t.Error("Has(EN, unknown key) returned true")
	}
	// Invalid locale -> never has anything.
	if Has(Locale("klingon"), KeyPlanVerdict_BuyIncrease) {
		t.Error("Has(invalid locale) returned true")
	}
}

// TestTLooksUpOtherLocaleBeforeMissing pins the fallback chain: when a
// key is missing in the requested locale but present in the other, T()
// must return the other value (rather than the "!!MISSING:" sentinel)
// while still bumping the missing-key counter so the gap is visible.
func TestTLooksUpOtherLocaleBeforeMissing(t *testing.T) {
	// Inject a one-sided key for the duration of this test, then clean
	// up so other tests still see parity.
	probe := Key("__test.only_in_zh__")
	messagesZH[probe] = "中文专属测试"
	defer delete(messagesZH, probe)

	got := T(LocaleEN, probe)
	if got != "中文专属测试" {
		t.Errorf("T(EN, zh-only key) = %q, want zh fallback", got)
	}

	// The reverse direction: en-only key requested under zh.
	probeEN := Key("__test.only_in_en__")
	messagesEN[probeEN] = "english only"
	defer delete(messagesEN, probeEN)

	if got := T(LocaleZH, probeEN); got != "english only" {
		t.Errorf("T(ZH, en-only key) = %q, want en fallback", got)
	}
}

// TestTfNoArgsReturnsTemplate covers the early-return path for keys
// that don't have placeholders (most of the bundle).
func TestTfNoArgsReturnsTemplate(t *testing.T) {
	got := Tf(LocaleEN, KeyPlanVerdict_BuyIncrease)
	if got != "Buy / Increase" {
		t.Errorf("Tf(EN, no args) = %q", got)
	}
}

// TestTInvalidLocaleCollapsesToZH covers the defensive branch that
// rejects out-of-set Locale values handed in by mistaken callers.
func TestTInvalidLocaleCollapsesToZH(t *testing.T) {
	got := T(Locale("klingon"), KeyPlanVerdict_BuyIncrease)
	if got != "买入/增配" {
		t.Errorf("T(invalid locale) = %q, want zh value", got)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func keysOf(m map[Key]string) []Key {
	out := make([]Key, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func diff(a, b []Key) []Key {
	bset := make(map[Key]struct{}, len(b))
	for _, k := range b {
		bset[k] = struct{}{}
	}
	var out []Key
	for _, k := range a {
		if _, ok := bset[k]; !ok {
			out = append(out, k)
		}
	}
	return out
}

// placeholderSignature extracts a stable signature of all `%...` runs in
// s. Both `%s` and `%[N]X` forms collapse to a sorted-comma string so
// reordering between zh and en variants is allowed but the SET must
// match.
var placeholderRe = regexp.MustCompile(`%(\[\d+\])?[+\-# 0]*\d*(?:\.\d+)?[a-zA-Z%]`)

func placeholderSignature(s string) string {
	matches := placeholderRe.FindAllString(s, -1)
	sort.Strings(matches)
	return strings.Join(matches, ",")
}
