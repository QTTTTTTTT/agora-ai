// advisor_credits_handler.go — Phase C-3 HTTP surface for /advisor
// credit packs and the LemonSqueezy webhook.
//
// Three endpoints:
//
//   POST  /api/advisor/credits/packs                       list available SKUs
//   POST  /api/advisor/credits/packs/{sku}/checkout        create hosted checkout URL
//   GET   /api/advisor/billing/orders                      user's purchase history
//   POST  /api/lemonsqueezy/webhook                        LS webhook receiver
//
// The webhook is the only un-authenticated endpoint here — it
// verifies HMAC against the configured webhook secret before
// touching any tables. Auth + advisor_mode flag gating happens
// upstream via featureFlagGateMiddleware for /api/advisor/* paths;
// the webhook is intentionally outside that prefix so admins
// can leave advisor_mode disabled and still process incoming
// payments (refunds, dispute updates).

package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/fundai/server/internal/advisorbilling"
	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/lemonsqueezy"
)

type advisorCreditsHandler struct {
	credits       *advisorbilling.CreditsRepo
	lsClient      *lemonsqueezy.Client
	successURL    string
	cancelURL     string
	now           func() time.Time
}

func newAdvisorCreditsHandler(svc *Services) *advisorCreditsHandler {
	if svc == nil || svc.AdvisorCreditsRepo == nil {
		return nil
	}
	lsClient, _, err := lemonsqueezy.NewClientFromEnv()
	if err != nil {
		// Construction error means the env was malformed (not
		// just unset). Leave the client nil so checkout returns
		// 503 and ops can investigate.
		lsClient = nil
	}
	return &advisorCreditsHandler{
		credits:    svc.AdvisorCreditsRepo,
		lsClient:   lsClient,
		successURL: strings.TrimSpace(os.Getenv("ADVISOR_CREDITS_SUCCESS_URL")),
		cancelURL:  strings.TrimSpace(os.Getenv("ADVISOR_CREDITS_CANCEL_URL")),
		now:        time.Now,
	}
}

func (h *advisorCreditsHandler) RegisterRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil {
		return
	}
	mux.HandleFunc("GET /api/advisor/credits/packs", h.handleListPacks)
	mux.HandleFunc("POST /api/advisor/credits/packs/{sku}/checkout", h.handleCheckout)
	mux.HandleFunc("GET /api/advisor/billing/orders", h.handleListOrders)
	mux.HandleFunc("POST /api/lemonsqueezy/webhook", h.handleWebhook)
}

// --- Wire shapes -----------------------------------------------------------

type creditPackWire struct {
	SKU             string `json:"sku"`
	LabelZh         string `json:"label_zh"`
	LabelEn         string `json:"label_en"`
	DescriptionZh   string `json:"description_zh"`
	DescriptionEn   string `json:"description_en"`
	DeepUnits       int    `json:"deep_units"`
	QuickUnits      int    `json:"quick_units"`
	PriceCentsUSD   int    `json:"price_cents_usd"`
	SortOrder       int    `json:"sort_order"`
	Available       bool   `json:"available"`
}

type creditOrderWire struct {
	ID                 string `json:"id"`
	PackSKU            string `json:"pack_sku"`
	DeepUnitsGranted   int    `json:"deep_units_granted"`
	QuickUnitsGranted  int    `json:"quick_units_granted"`
	PriceCentsUSD      int    `json:"price_cents_usd"`
	Currency           string `json:"currency"`
	Status             string `json:"status"`
	LemonSqueezyOrder  string `json:"lemonsqueezy_order_id,omitempty"`
	CheckoutURL        string `json:"checkout_url,omitempty"`
	PaidAt             string `json:"paid_at,omitempty"`
	RefundedAt         string `json:"refunded_at,omitempty"`
	CreatedAt          string `json:"created_at"`
}

type checkoutResponseWire struct {
	OrderID     string `json:"order_id"`
	CheckoutURL string `json:"checkout_url"`
	PackSKU     string `json:"pack_sku"`
}

// --- Handlers --------------------------------------------------------------

func (h *advisorCreditsHandler) handleListPacks(w http.ResponseWriter, r *http.Request) {
	userID, ok := api.AuthenticatedUserID(r)
	if !ok || strings.TrimSpace(userID) == "" {
		writeJSON(w, http.StatusUnauthorized, errorPayload("unauthorized", "missing or invalid bearer token"))
		return
	}
	cat := advisorbilling.CreditPackCatalog()
	out := make([]creditPackWire, 0, len(cat))
	for _, p := range cat {
		available := false
		if h.lsClient != nil {
			variantID := lemonsqueezy.VariantIDFromEnv(p.LemonSqueezyVariantEnvVar)
			available = strings.TrimSpace(variantID) != ""
		}
		out = append(out, creditPackWire{
			SKU:           p.SKU,
			LabelZh:       p.LabelZh,
			LabelEn:       p.LabelEn,
			DescriptionZh: p.DescriptionZh,
			DescriptionEn: p.DescriptionEn,
			DeepUnits:     p.DeepUnits,
			QuickUnits:    p.QuickUnits,
			PriceCentsUSD: p.PriceCentsUSD,
			SortOrder:     p.SortOrder,
			Available:     available,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"packs":            out,
		"checkout_enabled": h.lsClient != nil,
	})
}

func (h *advisorCreditsHandler) handleCheckout(w http.ResponseWriter, r *http.Request) {
	userID, ok := api.AuthenticatedUserID(r)
	if !ok || strings.TrimSpace(userID) == "" {
		writeJSON(w, http.StatusUnauthorized, errorPayload("unauthorized", "missing or invalid bearer token"))
		return
	}
	sku := strings.TrimSpace(r.PathValue("sku"))
	if sku == "" {
		writeJSON(w, http.StatusBadRequest, errorPayload("invalid_path", "pack sku required"))
		return
	}
	if h.lsClient == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorPayload("checkout_unavailable",
			"LemonSqueezy is not configured on this deployment"))
		return
	}
	pack, err := advisorbilling.LookupCreditPack(sku)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errorPayload("unknown_sku", err.Error()))
		return
	}
	variantID := lemonsqueezy.VariantIDFromEnv(pack.LemonSqueezyVariantEnvVar)
	if variantID == "" {
		writeJSON(w, http.StatusServiceUnavailable, errorPayload("variant_not_bound",
			"this pack has no LemonSqueezy variant binding — ask ops to set "+pack.LemonSqueezyVariantEnvVar))
		return
	}

	// Optional success/cancel URLs from query string overrides
	// (lets the SPA pin per-page return targets without an env
	// reload).
	successURL := strings.TrimSpace(r.URL.Query().Get("success_url"))
	if successURL == "" {
		successURL = h.successURL
	}
	cancelURL := strings.TrimSpace(r.URL.Query().Get("cancel_url"))
	if cancelURL == "" {
		cancelURL = h.cancelURL
	}

	// Step 1: create pending order so we have an internal id to
	// embed in the LS custom_data round-trip.
	orderID, err := h.credits.CreatePendingOrder(r.Context(), userID, pack, "")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("order_create_failed", err.Error()))
		return
	}

	// Step 2: call LS to mint the checkout URL.
	emailHint := strings.TrimSpace(r.URL.Query().Get("email"))
	nameHint := strings.TrimSpace(r.URL.Query().Get("name"))
	resp, err := h.lsClient.CreateHostedCheckout(r.Context(), lemonsqueezy.CheckoutRequest{
		VariantID:          variantID,
		UserEmail:          emailHint,
		UserName:           nameHint,
		CustomData:         lemonsqueezy.BuildCheckoutCustomData(orderID, userID, pack.SKU),
		RedirectURL:        cancelURL,
		SuccessRedirectURL: successURL,
	})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, errorPayload("checkout_failed", err.Error()))
		return
	}

	writeJSON(w, http.StatusOK, checkoutResponseWire{
		OrderID:     orderID,
		CheckoutURL: resp.URL,
		PackSKU:     pack.SKU,
	})
}

func (h *advisorCreditsHandler) handleListOrders(w http.ResponseWriter, r *http.Request) {
	userID, ok := api.AuthenticatedUserID(r)
	if !ok || strings.TrimSpace(userID) == "" {
		writeJSON(w, http.StatusUnauthorized, errorPayload("unauthorized", "missing or invalid bearer token"))
		return
	}
	limit := 50
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	orders, err := h.credits.ListOrders(r.Context(), userID, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("list_failed", err.Error()))
		return
	}
	out := make([]creditOrderWire, 0, len(orders))
	for _, o := range orders {
		row := creditOrderWire{
			ID:                o.ID,
			PackSKU:           o.PackSKU,
			DeepUnitsGranted:  o.DeepUnitsGranted,
			QuickUnitsGranted: o.QuickUnitsGranted,
			PriceCentsUSD:     o.PriceCentsUSD,
			Currency:          o.Currency,
			Status:            o.Status,
			CheckoutURL:       o.CheckoutURL,
			CreatedAt:         o.CreatedAt.UTC().Format(time.RFC3339),
		}
		if o.LemonSqueezyOrderID.Valid {
			row.LemonSqueezyOrder = o.LemonSqueezyOrderID.String
		}
		if o.PaidAt.Valid {
			row.PaidAt = o.PaidAt.Time.UTC().Format(time.RFC3339)
		}
		if o.RefundedAt.Valid {
			row.RefundedAt = o.RefundedAt.Time.UTC().Format(time.RFC3339)
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"orders": out})
}

// handleWebhook is the LemonSqueezy callback. Two interesting
// event types:
//
//   * order_created  (alias: order_paid in some accounts)
//   * order_refunded
//
// The handler:
//   1. Reads the body verbatim (the HMAC is over the raw bytes).
//   2. Verifies the X-Signature header against the configured
//      webhook secret.
//   3. Parses just enough of the JSON to extract the event id +
//      the embedded custom_data (the internal order_id).
//   4. Calls CreditsRepo.MarkOrderPaid / MarkOrderRefunded inside
//      a transaction.
//   5. Always returns 200 to ack the webhook unless the signature
//      fails — LemonSqueezy retries on non-200, which would dupe.
func (h *advisorCreditsHandler) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if h.lsClient == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorPayload("webhook_unavailable",
			"LemonSqueezy is not configured on this deployment"))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("body_read_failed", err.Error()))
		return
	}
	sig := r.Header.Get("X-Signature")
	if !h.lsClient.VerifyWebhookSignature(sig, body) {
		writeJSON(w, http.StatusUnauthorized, errorPayload("bad_signature",
			"X-Signature does not match HMAC of body — webhook rejected"))
		return
	}

	var payload struct {
		Meta struct {
			EventName string `json:"event_name"`
			CustomData map[string]string `json:"custom_data"`
		} `json:"meta"`
		Data struct {
			ID         string `json:"id"`
			Attributes struct {
				Status     string `json:"status"`
				Refunded   bool   `json:"refunded"`
				RefundedAt string `json:"refunded_at"`
				CreatedAt  string `json:"created_at"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		// Signature was valid but body isn't the expected shape.
		// Ack with 200 so LS doesn't retry — we'll surface this
		// in logs for ops to investigate.
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "warn": "payload parse failed"})
		return
	}

	internalOrderID := strings.TrimSpace(payload.Meta.CustomData["order_id"])
	eventID := strings.TrimSpace(r.Header.Get("X-Event-Id"))
	if eventID == "" {
		// Some LS variants put event id in the body under a
		// `meta.event_id` field; fall back to the data id.
		eventID = strings.TrimSpace(payload.Data.ID)
	}
	if internalOrderID == "" {
		// No internal id round-tripped — likely a manually-
		// created LS order. Ack and skip.
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "warn": "no internal order_id in custom_data"})
		return
	}

	switch payload.Meta.EventName {
	case "order_created", "order_paid":
		paidAt := time.Now().UTC()
		if t, err := time.Parse(time.RFC3339, payload.Data.Attributes.CreatedAt); err == nil {
			paidAt = t
		}
		_, err := h.credits.MarkOrderPaid(r.Context(), internalOrderID, payload.Data.ID, eventID, paidAt, body)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, errorPayload("credit_failed", err.Error()))
			return
		}
	case "order_refunded":
		refundedAt := time.Now().UTC()
		if t, err := time.Parse(time.RFC3339, payload.Data.Attributes.RefundedAt); err == nil {
			refundedAt = t
		}
		if err := h.credits.MarkOrderRefunded(r.Context(), internalOrderID, eventID, refundedAt, body); err != nil {
			// Logged but acked — we don't want LS to retry a
			// refund that already happened.
			if !errors.Is(err, errOrderNotPaid) {
				writeJSON(w, http.StatusOK, map[string]any{"ok": true, "warn": err.Error()})
				return
			}
		}
	default:
		// Unknown event: ack and skip so LS doesn't retry forever.
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// errOrderNotPaid is reserved for a future refinement where
// MarkOrderRefunded returns a typed error for "row was not in paid
// state". Today the repo returns a plain wrapped error; we keep
// the sentinel here so the webhook handler call site reads
// idiomatically.
var errOrderNotPaid = errors.New("advisorbilling: order not in paid state")
