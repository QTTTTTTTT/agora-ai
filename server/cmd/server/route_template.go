// route_template.go — collapse high-cardinality URL paths into
// stable route templates for metrics labelling.
//
// WHY THIS EXISTS
// ---------------
// The HTTP latency histogram (fundai_http_request_duration_seconds)
// labels each series by the literal r.URL.Path. That means two
// otherwise-identical requests to /api/funds/<fund-A>/holdings and
// /api/funds/<fund-B>/holdings emit DIFFERENT series — one per
// (method, fundID, status) tuple. With ~50 funds × ~100 endpoints
// this explodes to thousands of series, each with a handful of
// observations. Computing P95/P99 from a histogram that thin is
// statistical noise.
//
// We can't ask the inner ServeMux for the matched pattern from the
// OUTER requestLogger middleware: by the time the request reaches
// mux it has been re-WithContext'd by auth/cors/etc., so the mux
// sets Request.Pattern on a copy that requestLogger doesn't see.
//
// The pragmatic fix is regex/heuristic-based templatization at the
// boundary: walk the segments, replace anything that LOOKS like an
// opaque identifier with `{id}`. This is the same trick most
// observability libraries (otelhttp, gin-prometheus, fiber-prom)
// use under the hood.
//
// FALSE-POSITIVE BIAS
// -------------------
// When in doubt we templatize: a slightly over-collapsed metric
// label is recoverable (you still get the path family + count); an
// under-collapsed one is forever lost in the cardinality fog. The
// detection rules below are tuned conservatively against the IDs
// we observed in this codebase:
//
//   * UUID (32-hex with 4 dashes)         — funds, plans, decisions
//   * Long alphanumeric (≥16 chars)       — cuid/nanoid/ULIDs
//   * All-digit ≥ 6 chars                 — DB sequence ids, epoch
//   * Mixed letters+digits ≥ 8 chars      — short random handles
//
// We deliberately DO NOT templatize:
//
//   * Static enum values like "live", "shadow", "approved" —
//     short, all-letters, no digits.
//   * Ticker symbols (1–5 uppercase letters) — keep as labels so
//     /api/symbols/AAPL stays distinguishable from /api/symbols/MSFT
//     at the metrics layer (a real production ops use case).
//
// SHAPE STABILITY GUARANTEE
// -------------------------
// Templatization is purely string-shape based — no DB lookup, no
// allocation per segment beyond the slice rebuild. The function is
// deterministic and side-effect-free; safe to call from the
// hot-path requestLogger middleware on every request.

package main

import "strings"

// templatizeAPIPath returns a label-stable version of an API path
// suitable for use as a Prometheus histogram / counter label.
// Non-/api paths pass through untouched (we only collapse routes
// that flow through the API surface).
func templatizeAPIPath(path string) string {
	if !strings.HasPrefix(path, "/api") {
		return path
	}
	if path == "/api" || path == "/api/" {
		return path
	}
	// Trim trailing slash so we don't emit an empty segment.
	trimmed := strings.TrimRight(path, "/")
	parts := strings.Split(trimmed, "/")
	mutated := false
	for i, seg := range parts {
		if i == 0 {
			// leading "" because path starts with "/"
			continue
		}
		if seg == "" {
			continue
		}
		if isLikelyOpaqueID(seg) {
			parts[i] = "{id}"
			mutated = true
		}
	}
	if !mutated {
		return path
	}
	out := strings.Join(parts, "/")
	if strings.HasSuffix(path, "/") && !strings.HasSuffix(out, "/") {
		out += "/"
	}
	return out
}

// isLikelyOpaqueID reports whether a single path segment looks like
// an opaque per-record identifier (UUID, nanoid, cuid, sequence id,
// hash, etc.). The decision is intentionally conservative on
// false-positives to keep enum-style segments ("live", "shadow",
// "v1", "us_equity") un-collapsed.
func isLikelyOpaqueID(s string) bool {
	if s == "" {
		return false
	}
	// Sub-actions terminated by ":action" — Go 1.22 ServeMux uses
	// these for verbs like "/funds:assist". The ":" itself disqualifies
	// the segment from being an ID.
	if strings.ContainsRune(s, ':') {
		return false
	}
	// UUID 8-4-4-4-12.
	if isUUIDLike(s) {
		return true
	}
	// All-digit ≥ 6 chars — covers DB sequence IDs and epoch ts.
	if len(s) >= 6 && isAllDigits(s) {
		return true
	}
	// Long mixed alphanumeric (cuid, nanoid, ULID, hash, JWT-id...).
	// 16+ chars of [A-Za-z0-9_-] with at least one digit OR mixed
	// case — we require some "randomness shape" to avoid flagging
	// long route literal like "decision_input_fingerprint".
	if len(s) >= 16 && isAlnumDashUnderscore(s) && (hasDigit(s) || hasMixedCase(s)) {
		// Reject obvious static identifiers that happen to be long
		// snake_case (decision_input_fingerprint). Heuristic: if
		// the segment contains '_' AND no digits AND no mixed-case,
		// it's a verb. We've already required "hasDigit OR
		// hasMixedCase" above, so a pure-snake_case-no-digit slug
		// won't get here. Good.
		return true
	}
	// Short opaque IDs: 8–15 chars with BOTH letters and digits.
	// Catches things like nanoid(8), short hash, short slug ids.
	if len(s) >= 8 && len(s) < 16 && isAlnumDashUnderscore(s) && hasDigit(s) && hasLetter(s) {
		// Same guard against verbs: needs digit AND letter to
		// qualify (which we just checked). Pure verbs without
		// digits are exempt.
		return true
	}
	return false
}

func isUUIDLike(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !isHexDigit(r) {
				return false
			}
		}
	}
	return true
}

func isHexDigit(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
}

func isAllDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func hasDigit(s string) bool {
	for _, r := range s {
		if r >= '0' && r <= '9' {
			return true
		}
	}
	return false
}

func hasLetter(s string) bool {
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			return true
		}
	}
	return false
}

func hasMixedCase(s string) bool {
	hasLower, hasUpper := false, false
	for _, r := range s {
		if r >= 'a' && r <= 'z' {
			hasLower = true
		}
		if r >= 'A' && r <= 'Z' {
			hasUpper = true
		}
		if hasLower && hasUpper {
			return true
		}
	}
	return false
}

func isAlnumDashUnderscore(s string) bool {
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '-' || r == '_') {
			return false
		}
	}
	return true
}
