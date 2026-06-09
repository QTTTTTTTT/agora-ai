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
// When none of those is present we fail OPEN (no block, no
// log) because in local dev the absence of a CDN means every
// request would otherwise be flagged. Production deployments
// MUST sit behind a CDN that injects one of the headers.

package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/fundai/server/internal/compliance"
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

func complianceGeoMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if pathsExemptFromGeoBlock[r.URL.Path] {
				next.ServeHTTP(w, r)
				return
			}
			country := resolveCountryHeader(r)
			subRegion := resolveSubRegionHeader(r)
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
