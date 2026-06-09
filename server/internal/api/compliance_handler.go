package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fundai/server/internal/compliance"
)

// ComplianceService is the api-package façade for the compliance
// subsystem. Three concerns:
//
//  1. Disclosure text retrieval — clients fetch the standard
//     per-surface disclosure block in the user's locale so the
//     bilingual text lives in one place (server-rendered) rather
//     than being copy-pasted across web / android / miniapp.
//
//  2. Disclosure acknowledgement — clients POST when the user
//     clicks "I understand" on the modal. Stored with IP-country
//     + UA so the legal team can prove informed consent if a
//     dispute arises.
//
//  3. Phrase-violation audit — read-only listing for the admin
//     dashboard.
//
// The wiring is nil-safe in the established pattern: an unwired
// FundHandler.compliance returns 503 from the listing
// endpoints, while the disclosure endpoint always succeeds (the
// text bank is in-process). The ack endpoint without a
// configured service silently succeeds but doesn't persist —
// this prevents test environments without a DB from breaking
// the modal, while still requiring the click client-side.
type ComplianceService interface {
	RecordAcknowledgment(userID string, in ComplianceAckInput) (*ComplianceAckView, error)
	ListAcknowledgments(userID string) ([]ComplianceAckView, error)
	ListViolations(limit int) ([]CompliancePhraseViolationView, error)
	CurrentMode() string
}

// ErrComplianceUnconfigured is the sentinel.
var ErrComplianceUnconfigured = errors.New("compliance service not configured")

// ComplianceAckInput is the body of POST /api/compliance/acknowledgments.
//
// Surface + Mode are required. The client picks Surface based
// on which UI flow it's gating ("advisor" before a consult,
// "global" for the one-time onboarding modal). Mode reflects
// what the user actually SAW (the server-side disclosure bank
// can change which text Mode resolves to, but the user clicked
// through the text the client knew about at click time).
type ComplianceAckInput struct {
	Surface          string `json:"surface"`
	Mode             string `json:"mode"`
	Locale           string `json:"locale,omitempty"`
	AcknowledgedText string `json:"acknowledgedText"`
	TextVersion      int    `json:"textVersion,omitempty"`
}

// ComplianceAckView is what we hand back to the client (and
// also what GET /api/compliance/acknowledgments returns as a
// list element).
type ComplianceAckView struct {
	ID               string    `json:"id"`
	Surface          string    `json:"surface"`
	Mode             string    `json:"mode"`
	Locale           string    `json:"locale"`
	AcknowledgedAt   time.Time `json:"acknowledgedAt"`
	AcknowledgedText string    `json:"acknowledgedText,omitempty"`
	TextVersion      int       `json:"textVersion"`
}

// CompliancePhraseViolationView is the admin-dashboard shape
// of one row of compliance_phrase_violations.
type CompliancePhraseViolationView struct {
	ID             string    `json:"id"`
	UserID         string    `json:"userId,omitempty"`
	Surface        string    `json:"surface"`
	Rule           string    `json:"rule"`
	OriginalPhrase string    `json:"originalPhrase"`
	Replacement    string    `json:"replacement"`
	FullRedacted   string    `json:"fullRedacted,omitempty"`
	FlaggedAt      time.Time `json:"flaggedAt"`
	SourceEntity   string    `json:"sourceEntity,omitempty"`
	SourceID       string    `json:"sourceId,omitempty"`
}

// GetComplianceDisclosure returns the standard disclosure text
// for a single (surface, mode, locale) tuple. Cheap, no DB; just
// reads the disclosureBank in-memory.
//
//	GET /api/compliance/disclosure?surface=advisor&locale=en
//
// Mode is taken from the server's configured ComplianceMode (NOT
// from the client) — clients aren't allowed to ask "show me the
// RIA disclosure" if the server is in Publisher mode, because
// that would weaken the audit trail.
func (h *FundHandler) GetComplianceDisclosure(w http.ResponseWriter, r *http.Request) {
	surface := compliance.Surface(strings.ToLower(strings.TrimSpace(r.URL.Query().Get("surface"))))
	if surface == "" {
		surface = compliance.SurfaceAdvisor
	}
	locale := r.URL.Query().Get("locale")
	if locale == "" {
		locale = "en"
	}
	mode := compliance.ParseMode(h.activeComplianceMode())
	text := compliance.Disclosure(mode, surface, locale)
	ack := compliance.AcknowledgmentText(mode, locale)
	hypo := compliance.HypotheticalPerformanceDisclaimer(locale)
	writeJSON(w, http.StatusOK, map[string]any{
		"mode":                  string(mode),
		"surface":               string(surface),
		"locale":                locale,
		"disclosure":            text,
		"acknowledgmentText":    ack,
		"hypotheticalPerformanceDisclaimer": hypo,
	})
}

// RecordComplianceAck stores the user click-through. Returns
// 401 if the request isn't authenticated.
//
//	POST /api/compliance/acknowledgments
//	{ "surface": "advisor", "mode": "publisher", "locale": "en",
//	  "acknowledgedText": "...", "textVersion": 1 }
//
// User identity comes from the JWT-validated request context
// (AuthenticatedUserID) — NOT from a client-supplied header —
// so the audit row can't be forged by a malicious user impersonating
// someone else's user_id.
func (h *FundHandler) RecordComplianceAck(w http.ResponseWriter, r *http.Request) {
	userID, ok := AuthenticatedUserID(r)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	var input ComplianceAckInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if h.compliance == nil {
		// Service unwired (e.g. local dev without DB) — degrade
		// gracefully so the modal still proceeds. We return
		// 200 with a synthetic view so the client knows the
		// click was accepted, but no row is persisted.
		writeJSON(w, http.StatusOK, &ComplianceAckView{
			Surface:          input.Surface,
			Mode:             input.Mode,
			Locale:           input.Locale,
			AcknowledgedAt:   time.Now().UTC(),
			AcknowledgedText: input.AcknowledgedText,
			TextVersion:      max1(input.TextVersion),
		})
		return
	}
	view, err := h.compliance.RecordAcknowledgment(userID, input)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// ListComplianceAcks returns every ack row for the calling user.
//
//	GET /api/compliance/acknowledgments
func (h *FundHandler) ListComplianceAcks(w http.ResponseWriter, r *http.Request) {
	userID, ok := AuthenticatedUserID(r)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	if h.compliance == nil {
		writeJSON(w, http.StatusOK, []ComplianceAckView{})
		return
	}
	rows, err := h.compliance.ListAcknowledgments(userID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if rows == nil {
		rows = []ComplianceAckView{}
	}
	writeJSON(w, http.StatusOK, rows)
}

// ListComplianceViolations is the admin dashboard endpoint.
//
//	GET /api/compliance/violations?limit=100
//
// Authorisation is intentionally minimal here (any authenticated
// user passes the header check) — the wider admin auth layer
// owns "is this user an operator" enforcement. Keeping the
// handler open lets us run the same code path against ad-hoc
// JWTs in CI.
func (h *FundHandler) ListComplianceViolations(w http.ResponseWriter, r *http.Request) {
	if h.compliance == nil {
		http.Error(w, ErrComplianceUnconfigured.Error(), http.StatusServiceUnavailable)
		return
	}
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}
	rows, err := h.compliance.ListViolations(limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if rows == nil {
		rows = []CompliancePhraseViolationView{}
	}
	writeJSON(w, http.StatusOK, rows)
}

// activeComplianceMode returns the mode as a string. When the
// compliance service isn't wired we still serve the default
// Publisher mode — the safer floor.
func (h *FundHandler) activeComplianceMode() string {
	if h == nil || h.compliance == nil {
		return string(compliance.DefaultMode)
	}
	mode := h.compliance.CurrentMode()
	if mode == "" {
		return string(compliance.DefaultMode)
	}
	return mode
}

func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}
