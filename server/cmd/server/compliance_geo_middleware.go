// compliance_geo_middleware.go — OFAC sanctions + EU MiFID-II
// soft-warn HTTP middleware.
//
// Two concerns this middleware handles, in order of strictness:
//
//   1. OFAC comprehensive sanctions (Iran, North Korea, Cuba,
//      Syria, Russia at country-level). Visitors resolved to
//      these countries get HTTP 451 ("Unavailable For Legal
//      Reasons") with a short JSON body explaining the block.
//      A request to /api/health is exempt — we don't want to
//      break uptime monitors that ping from anywhere.
//
//   2. EU member states: soft warn. We don't block but we
//      emit a structured log line and stamp the response with
//      an "X-Compliance-Region: EU" header so the legal team
//      can pull MAU numbers per region. Path A (Publisher
//      mode) content is fine for EU visitors under the
//      reverse-solicitation doctrine; Path B (RIA mode) will
//      need MiFID II passporting before we can serve them.
//
// Country resolution preference order:
//
//   1. CF-IPCountry           (Cloudflare)
//   2. X-Vercel-IP-Country    (Vercel)
//   3. X-Country               (custom reverse-proxy override)
//
// When none of those is present we fail OPEN in development
// (no block, no log) because in local dev the absence of a CDN
// means every request would otherwise be flagged.
//
// In production (APP_ENV=production) we fail CLOSE: any request
// that reaches the middleware without any recognised geo header
// is answered with HTTP 503 + a structured slog line (rate-
// limited to once per minute so a real CDN-less incident does
// not flood the log). Rationale: the only way a production
// request can lack the header is (a) the CDN was bypassed
// (direct-origin hit) or (b) the CDN is misconfigured — either
// case lets sanctioned-country traffic in unnoticed, so refusing
// the request is strictly safer than answering it. Uptime
// monitors and observability scrapers stay on the
// pathsExemptFromGeoBlock allow-list and so are not affected
// even when they call from outside the CDN.

package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/fundai/server/internal/compliance"
	"golang.org/x/time/rate"
)

// pathsExemptFromGeoBlock are health / status / metrics paths
// we always serve regardless of country. Uptime monitors and
// observability scrapers run from arbitrary clouds and we
// don't want their checks to flap based on geo.
var pathsExemptFromGeoBlock = map[string]bool{
	"/api/health":          true,
	"/api/healthz":         true,
	"/api/readiness":       true,
	"/api/metrics":         true,
	"/api/version":         true,
	"/api/compliance/disclosure": true, // need to load the disclosure to render the 451 page
}

// complianceGeoMiddleware constructs the geo middleware. The
// optional metrics parameter lets every block / fail-close
// decision bump compliance_filter_blocked_total{pattern,layer="geo"}
// so the same SRE alert (per-pattern spike) catches a sudden
// uptick of OFAC-country traffic alongside scanner-driven hits.
func complianceGeoMiddleware(metrics *serverMetrics) func(http.Handler) http.Handler {
	// Captured once at construction. APP_ENV does not change at
	// runtime; reading it on every request would be needless work.
	isProduction := strings.EqualFold(strings.TrimSpace(os.Getenv("APP_ENV")), "production")
	// One log per minute, burst of one. Tuned so a real CDN outage
	// produces a steady drumbeat in stdout without ten thousand
	// lines per second drowning out the rest of the log feed.
	missingHeaderLogLimiter := rate.NewLimiter(rate.Every(time.Minute), 1)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if pathsExemptFromGeoBlock[r.URL.Path] {
				next.ServeHTTP(w, r)
				return
			}
			country := resolveCountryHeader(r)
			subRegion := resolveSubRegionHeader(r)
			// Production fail-close: missing CDN header means the
			// request bypassed the geo enforcement perimeter. We
			// refuse rather than serve, even though the visitor
			// might be in an allowed country, because we cannot
			// prove they are. Dev / staging keep fail-open so
			// `curl localhost:8080/api/funds` still works.
			if country == "" && isProduction {
				if missingHeaderLogLimiter.Allow() {
					slog.Warn("compliance.geo.missing_cdn_header",
						"path", r.URL.Path,
						"method", r.Method,
						"remote_addr", r.RemoteAddr,
						"hint", "production traffic must transit a CDN that sets CF-IPCountry, X-Vercel-IP-Country, or X-Country")
				}
				metrics.RecordComplianceFilterBlock("missing_cdn_header", "geo")
				writeGeoHeaderMissing(w)
				return
			}
			decision := compliance.GeoDecideEx(country, subRegion)
			// Stamp the response with the resolved country for
			// downstream observability and (in EU's case) for the
			// front-end to render a "we noticed you're in the EU"
			// nudge.
			if decision.CountryCode != "" {
				w.Header().Set("X-Compliance-Country", decision.CountryCode)
			}
			switch decision.Action {
			case compliance.ActionBlock:
				slog.Warn("compliance.geo.block",
					"country", decision.CountryCode,
					"rule", decision.RuleID,
					"path", r.URL.Path,
					"method", r.Method)
				metrics.RecordComplianceFilterBlock(decision.RuleID, "geo")
				writeGeoBlock(w, decision)
				return
			case compliance.ActionWarn:
				slog.Info("compliance.geo.warn",
					"country", decision.CountryCode,
					"rule", decision.RuleID,
					"path", r.URL.Path)
				w.Header().Set("X-Compliance-Region", "EU")
			case compliance.ActionRequiresAck:
				// US Blue Sky state — non-blocking but the front
				// end may want to re-prompt the disclosure modal.
				w.Header().Set("X-Compliance-Requires-Ack", decision.RuleID)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// resolveCountryHeader picks the first non-empty value out of
// the known CDN headers. Values are normalised ToUpper'd in
// compliance.GeoDecide so callers don't have to worry about
// case.
func resolveCountryHeader(r *http.Request) string {
	for _, key := range []string{"CF-IPCountry", "X-Vercel-IP-Country", "X-Country"} {
		if v := strings.TrimSpace(r.Header.Get(key)); v != "" {
			return v
		}
	}
	return ""
}

// resolveSubRegionHeader picks the first non-empty value among
// the known per-state CDN headers. Only consulted when the
// country is US (compliance.GeoDecideEx ignores sub-region for
// other countries).
func resolveSubRegionHeader(r *http.Request) string {
	for _, key := range []string{"CF-Region-Code", "X-Vercel-IP-Country-Region", "X-Region-Code"} {
		if v := strings.TrimSpace(r.Header.Get(key)); v != "" {
			return v
		}
	}
	return ""
}

// writeGeoBlock emits the HTTP 451 + a short JSON body so the
// front-end can render a friendly "not available in your
// region" page. Per RFC 7725 the body should also include a
// link to the blocking authority (here, the OFAC list URL).
func writeGeoBlock(w http.ResponseWriter, d compliance.Decision) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	w.WriteHeader(http.StatusUnavailableForLegalReasons)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":        "unavailable_for_legal_reasons",
		"reason":       d.Reason,
		"country":      d.CountryCode,
		"rule":         d.RuleID,
		"authority":    "https://www.treasury.gov/resource-center/sanctions/SDN-List",
	})
}

// writeGeoHeaderMissing is the production fail-close response.
// We deliberately do NOT echo the request path or any header
// data back to the caller — the response body is identical for
// every blocked request so a scanner cannot probe which paths
// would have served had a header been forged.
func writeGeoHeaderMissing(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":  "geo_resolution_unavailable",
		"reason": "Required CDN geo headers missing; production traffic must transit a CDN.",
	})
}
