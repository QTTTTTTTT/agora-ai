// geo.go — OFAC sanctions + EU geo-block helpers.
//
// Why this lives in the compliance package, not infra:
//
//   - The list of blocked jurisdictions is a compliance decision,
//     not a network policy. When we add or drop a country, the
//     legal team owns the call and they only need to look at this
//     one file.
//   - Returning a structured Decision (rather than just a bool)
//     lets the HTTP middleware emit a precise audit-log entry
//     including the rule that fired, which we need for
//     recordkeeping.
//
// Two layers:
//
//  1. OFAC sanctioned countries → HARD BLOCK with HTTP 451.
//     OFAC compliance is a federal criminal matter (50 U.S.C.
//     § 1705 — up to 20 years in prison for wilful violations).
//     We err on the side of over-blocking.
//
//  2. EU member states → SOFT WARN. Under MiFID II + DORA
//     providing "investment advice" cross-border requires
//     either an EU branch or a passport. In Publisher mode we
//     are not formally giving advice, but we still log the hit
//     so the compliance team has a record (and, if they later
//     decide to block, the geo path is wired).
//
// The actual IP → country resolution is left to the caller —
// either via a reverse-proxy header (Cloudflare's CF-IPCountry,
// AWS' CloudFront-Viewer-Country) or an in-process MaxMind GeoIP2
// lookup. This file accepts an ISO-3166 alpha-2 string and
// returns a Decision.

package compliance

import "strings"

// Action is the action the middleware should take based on
// GeoDecide.
type Action string

const (
	ActionAllow    Action = "allow"
	ActionWarn     Action = "warn"     // serve but log for compliance review
	ActionBlock    Action = "block"    // refuse with HTTP 451
	ActionRequiresAck Action = "requires_ack" // serve but require user re-ack first
)

// Decision is what GeoDecide returns. Includes the country we
// resolved, the action to take, and a short reason for audit
// logging.
type Decision struct {
	CountryCode string `json:"country_code"`
	Action      Action `json:"action"`
	Reason      string `json:"reason"`
	RuleID      string `json:"rule_id"`
}

// ofacBlocked = countries on the OFAC comprehensive sanctions
// list as of 2024 (Cuba, Iran, North Korea, Syria, Crimea/DNR/LNR
// regions of Ukraine). Crimea / DNR / LNR don't have ISO-3166
// codes; the source of truth needs to be GeoIP at the region
// level for those — for now the country-level block of UA + RU
// is over-inclusive, but the soft path is to use ActionRequiresAck
// for RU/UA (the user has to attest they're NOT in the
// embargoed regions) rather than full block.
var ofacBlocked = map[string]bool{
	"CU": true, // Cuba
	"IR": true, // Iran
	"KP": true, // North Korea
	"SY": true, // Syria
	"RU": true, // Russia (Crimea/DNR/LNR/Sevastopol embargo + sectoral sanctions)
	// "UA" omitted: Ukraine is allowed at the country level;
	// the embargo applies only to Crimea / DNR / LNR / Sevastopol.
	// Region-level enforcement requires a GeoIP database.
}

// euMemberStates = the 27 EU member states. We do NOT block —
// we WARN. MiFID II cross-border restrictions can be navigated
// (reverse-solicitation doctrine), but the legal review of
// each onboarding is mandatory.
var euMemberStates = map[string]bool{
	"AT": true, "BE": true, "BG": true, "HR": true, "CY": true,
	"CZ": true, "DK": true, "EE": true, "FI": true, "FR": true,
	"DE": true, "GR": true, "HU": true, "IE": true, "IT": true,
	"LV": true, "LT": true, "LU": true, "MT": true, "NL": true,
	"PL": true, "PT": true, "RO": true, "SK": true, "SI": true,
	"ES": true, "SE": true,
}

// usStates that require additional scrutiny in Publisher mode.
// CA, NY, and TX have state-level securities laws (Blue Sky
// Laws) that occasionally diverge from federal exemptions. We
// don't block, but in Publisher mode we require the per-state
// acknowledgment text be served.
//
// usStateScrutiny is consulted only when CountryCode == "US"
// AND the caller supplies a sub-region code via GeoDecideEx.
var usStateScrutiny = map[string]string{
	"CA": "California state Blue Sky requires additional acknowledgment",
	"NY": "New York Martin Act requires additional acknowledgment",
	"TX": "Texas TSSB requires additional acknowledgment",
}

// GeoDecide returns the Decision for the given ISO-3166 alpha-2
// country code. Returns ActionAllow with an empty Reason for
// the happy path (the caller can short-circuit on Action ==
// ActionAllow). Unknown / empty inputs default to ActionAllow
// because the front gate is "if you can't resolve, don't block";
// add a stricter env-driven mode if you'd rather fail closed.
func GeoDecide(countryCode string) Decision {
	cc := normalizeCountry(countryCode)
	if cc == "" {
		return Decision{Action: ActionAllow, Reason: "no-country-resolved"}
	}
	if ofacBlocked[cc] {
		return Decision{
			CountryCode: cc,
			Action:      ActionBlock,
			Reason:      "country is on the OFAC comprehensive sanctions list",
			RuleID:      "ofac_comprehensive",
		}
	}
	if euMemberStates[cc] {
		return Decision{
			CountryCode: cc,
			Action:      ActionWarn,
			Reason:      "EU member state — MiFID II review required before personalised advice; impersonal Publisher-mode content is served",
			RuleID:      "eu_mifid_warn",
		}
	}
	return Decision{
		CountryCode: cc,
		Action:      ActionAllow,
		Reason:      "no geo restriction",
		RuleID:      "default_allow",
	}
}

// GeoDecideEx accepts an optional sub-region code (e.g. a US
// state) and produces the same Decision but with the
// ActionRequiresAck escalation when a stricter US state is
// resolved. The sub-region is only consulted when countryCode
// is "US".
func GeoDecideEx(countryCode, subRegion string) Decision {
	d := GeoDecide(countryCode)
	if d.Action != ActionAllow {
		return d
	}
	cc := normalizeCountry(countryCode)
	sub := normalizeCountry(subRegion)
	if cc != "US" || sub == "" {
		return d
	}
	if reason, ok := usStateScrutiny[sub]; ok {
		return Decision{
			CountryCode: cc,
			Action:      ActionRequiresAck,
			Reason:      reason,
			RuleID:      "us_state_blue_sky_" + strings.ToLower(sub),
		}
	}
	return d
}

func normalizeCountry(s string) string {
	return strings.ToUpper(strings.TrimSpace(s))
}
