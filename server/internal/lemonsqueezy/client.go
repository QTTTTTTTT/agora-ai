// Package lemonsqueezy is a minimal client for the LemonSqueezy
// "Merchant of Record" billing surface.
//
// We deliberately implement only what Phase C needs:
//
//   - CreateHostedCheckout — create a checkout URL the SPA
//     redirects to.
//   - VerifyWebhookSignature — validate the HMAC the LemonSqueezy
//     webhook sends so we can trust the body before crediting
//     anyone.
//
// There is no SDK call to "verify an order" — we trust the webhook
// and rely on the unique-index idempotency in advisor_credit_orders
// to handle replays.
//
// References:
//
//	https://docs.lemonsqueezy.com/api/checkouts/create-checkout
//	https://docs.lemonsqueezy.com/help/webhooks
package lemonsqueezy

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	apiBase           = "https://api.lemonsqueezy.com/v1"
	checkoutsEndpoint = "/checkouts"
)

// Config holds the runtime credentials.
type Config struct {
	APIKey        string
	StoreID       string
	WebhookSecret string
	// Optional: BaseURL override for testing.
	BaseURL string
	// Optional HTTP client; defaults to a 15s-timeout client.
	HTTPClient *http.Client
}

// FromEnv pulls the config from environment variables. Returns
// (cfg, ok) so the caller can decide whether to wire the rest of
// the billing surface.
//
// Required env:
//
//	LEMONSQUEEZY_API_KEY        — secret API key (lst_*)
//	LEMONSQUEEZY_STORE_ID       — numeric store id from LS dashboard
//	LEMONSQUEEZY_WEBHOOK_SECRET — webhook signing secret
//
// Optional:
//
//	LEMONSQUEEZY_API_BASE       — override for tests / mock
func FromEnv() (*Config, bool) {
	api := strings.TrimSpace(os.Getenv("LEMONSQUEEZY_API_KEY"))
	store := strings.TrimSpace(os.Getenv("LEMONSQUEEZY_STORE_ID"))
	secret := strings.TrimSpace(os.Getenv("LEMONSQUEEZY_WEBHOOK_SECRET"))
	if api == "" || store == "" || secret == "" {
		return nil, false
	}
	cfg := &Config{
		APIKey:        api,
		StoreID:       store,
		WebhookSecret: secret,
		BaseURL:       strings.TrimSpace(os.Getenv("LEMONSQUEEZY_API_BASE")),
	}
	return cfg, true
}

// Client is the typed wrapper around the LemonSqueezy REST API.
type Client struct {
	cfg  Config
	http *http.Client
}

// NewClient wires a client. Returns an error when the config is
// missing required fields.
func NewClient(cfg Config) (*Client, error) {
	if cfg.APIKey == "" || cfg.StoreID == "" || cfg.WebhookSecret == "" {
		return nil, errors.New("lemonsqueezy: api key, store id, and webhook secret required")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = apiBase
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &Client{cfg: cfg, http: httpClient}, nil
}

// NewClientFromEnv is a convenience wrapper for the common case.
// Returns (nil, false, nil) when env is unset — the wiring layer
// then disables the checkout endpoint cleanly.
func NewClientFromEnv() (*Client, bool, error) {
	cfg, ok := FromEnv()
	if !ok {
		return nil, false, nil
	}
	c, err := NewClient(*cfg)
	if err != nil {
		return nil, false, err
	}
	return c, true, nil
}

// CheckoutRequest is the input to CreateHostedCheckout.
type CheckoutRequest struct {
	VariantID  string
	UserEmail  string
	UserName   string
	CustomData map[string]string
	// VariantQuantity is used for seat-based products. LemonSqueezy
	// expects this under checkout_data.variant_quantities, not as a
	// top-level quantity field.
	VariantQuantity    int
	RedirectURL        string
	SuccessRedirectURL string
}

// CheckoutResponse is the (subset of the) hosted-checkout payload
// the SPA needs.
type CheckoutResponse struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

// CreateHostedCheckout creates a checkout URL the SPA redirects
// to. The user pays on lemonsqueezy.com; the webhook fires once
// payment completes.
//
// CustomData is the merchant-side metadata bag — we put our
// internal order id here so the webhook handler can correlate
// without an additional lookup table.
func (c *Client) CreateHostedCheckout(ctx context.Context, req CheckoutRequest) (*CheckoutResponse, error) {
	if c == nil {
		return nil, errors.New("lemonsqueezy: client not configured")
	}
	if strings.TrimSpace(req.VariantID) == "" {
		return nil, errors.New("lemonsqueezy: variant_id required")
	}

	checkoutData := map[string]any{
		"email":  req.UserEmail,
		"name":   req.UserName,
		"custom": req.CustomData,
	}
	if req.VariantQuantity > 0 {
		checkoutData["variant_quantities"] = []map[string]any{
			{"variant_id": req.VariantID, "quantity": req.VariantQuantity},
		}
	}

	attrs := map[string]any{
		"checkout_data": checkoutData,
	}
	if u := strings.TrimSpace(req.SuccessRedirectURL); u != "" {
		attrs["product_options"] = map[string]any{
			"redirect_url": u,
		}
	}

	body := map[string]any{
		"data": map[string]any{
			"type":       "checkouts",
			"attributes": attrs,
			"relationships": map[string]any{
				"store": map[string]any{
					"data": map[string]any{"type": "stores", "id": c.cfg.StoreID},
				},
				"variant": map[string]any{
					"data": map[string]any{"type": "variants", "id": req.VariantID},
				},
			},
		},
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("lemonsqueezy: marshal: %w", err)
	}

	endpoint := strings.TrimRight(c.cfg.BaseURL, "/") + checkoutsEndpoint
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(buf))
	if err != nil {
		return nil, fmt.Errorf("lemonsqueezy: build request: %w", err)
	}
	httpReq.Header.Set("Accept", "application/vnd.api+json")
	httpReq.Header.Set("Content-Type", "application/vnd.api+json")
	httpReq.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("lemonsqueezy: http: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("lemonsqueezy: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("lemonsqueezy: checkout failed (%d): %s", resp.StatusCode, truncateBody(respBody, 400))
	}
	var parsed struct {
		Data struct {
			ID         string `json:"id"`
			Attributes struct {
				URL string `json:"url"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("lemonsqueezy: parse response: %w", err)
	}
	if parsed.Data.Attributes.URL == "" {
		return nil, errors.New("lemonsqueezy: empty checkout url")
	}
	return &CheckoutResponse{ID: parsed.Data.ID, URL: parsed.Data.Attributes.URL}, nil
}

// VerifyWebhookSignature validates the X-Signature HMAC header
// against the webhook body using the configured webhook secret.
//
// LS sends the signature as a lowercase hex-encoded HMAC-SHA256.
// We compare in constant time to avoid leaking which byte
// differed.
func (c *Client) VerifyWebhookSignature(signatureHeader string, body []byte) bool {
	if c == nil {
		return false
	}
	expected := computeHexHMAC(c.cfg.WebhookSecret, body)
	return hmac.Equal([]byte(expected), []byte(strings.ToLower(strings.TrimSpace(signatureHeader))))
}

// VariantIDFromEnv reads the LemonSqueezy variant id for a given
// pack SKU from env. The mapping (SKU → env var) is centralised
// in advisorbilling.CreditPack so a renamed SKU bubbles up to a
// single line of code.
func VariantIDFromEnv(envVarName string) string {
	return strings.TrimSpace(os.Getenv(envVarName))
}

// GetCustomerPortalURL fetches the LemonSqueezy-hosted
// customer-portal URL for an existing customer. The portal is
// where the user can self-serve cancel / change card / view
// invoices, so we don't have to build that surface ourselves.
//
// LS schema:
//
//	GET /v1/customers/{id}
//	→ data.attributes.urls.customer_portal
func (c *Client) GetCustomerPortalURL(ctx context.Context, customerID string) (string, error) {
	if c == nil {
		return "", errors.New("lemonsqueezy: client not configured")
	}
	if strings.TrimSpace(customerID) == "" {
		return "", errors.New("lemonsqueezy: customer_id required")
	}
	endpoint := strings.TrimRight(c.cfg.BaseURL, "/") + "/customers/" + url.PathEscape(customerID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("lemonsqueezy: build request: %w", err)
	}
	httpReq.Header.Set("Accept", "application/vnd.api+json")
	httpReq.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("lemonsqueezy: http: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("lemonsqueezy: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("lemonsqueezy: customer portal failed (%d): %s", resp.StatusCode, truncateBody(respBody, 400))
	}
	var parsed struct {
		Data struct {
			Attributes struct {
				URLs struct {
					CustomerPortal string `json:"customer_portal"`
				} `json:"urls"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("lemonsqueezy: parse response: %w", err)
	}
	if parsed.Data.Attributes.URLs.CustomerPortal == "" {
		return "", errors.New("lemonsqueezy: empty customer portal url")
	}
	return parsed.Data.Attributes.URLs.CustomerPortal, nil
}

// BuildCheckoutCustomData constructs the LS custom_data payload
// we round-trip through the webhook. The order_id we store here
// is the internal advisor_credit_orders.id (NOT the LS order id);
// the webhook handler looks it up by that field to credit the
// right user.
func BuildCheckoutCustomData(internalOrderID, userID, sku string) map[string]string {
	return map[string]string{
		"order_id": internalOrderID,
		"user_id":  userID,
		"pack_sku": sku,
		"source":   "advisor_credits",
	}
}

// --- internals -------------------------------------------------------------

func computeHexHMAC(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return strings.ToLower(hex.EncodeToString(mac.Sum(nil)))
}

func truncateBody(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}

// EncodeCheckoutCustomData base64-url encodes the custom_data
// dict into the format LS expects in the embedded `checkout`
// query string when CreateCheckout isn't used. Reserved for
// future "open variant URL with prefilled custom data" flow;
// keeps the API symmetric with BuildCheckoutCustomData.
func EncodeCheckoutCustomData(values url.Values) string {
	return values.Encode()
}
