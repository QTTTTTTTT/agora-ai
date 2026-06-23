package lemonsqueezy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestVerifyWebhookSignature_Matches(t *testing.T) {
	c, err := NewClient(Config{
		APIKey: "k", StoreID: "1", WebhookSecret: "shhh",
	})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"order_id":"abc"}`)
	sig := computeHexHMAC("shhh", body)
	if !c.VerifyWebhookSignature(sig, body) {
		t.Error("expected match")
	}
	// Case-insensitive header (some proxies upcase the hex).
	if !c.VerifyWebhookSignature(strings.ToUpper(sig), body) {
		t.Error("expected case-insensitive match")
	}
}

func TestVerifyWebhookSignature_Mismatch(t *testing.T) {
	c, err := NewClient(Config{
		APIKey: "k", StoreID: "1", WebhookSecret: "shhh",
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.VerifyWebhookSignature("deadbeef", []byte(`{}`)) {
		t.Error("expected reject")
	}
	if c.VerifyWebhookSignature("", []byte(`{}`)) {
		t.Error("expected reject on empty signature")
	}
}

func TestCreateHostedCheckout_RoundTripsURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("bad auth header: %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		// Just sanity check we sent the variant relationship.
		var parsed map[string]any
		if err := json.Unmarshal(body, &parsed); err != nil {
			t.Fatal(err)
		}
		data := parsed["data"].(map[string]any)
		attrs := data["attributes"].(map[string]any)
		if _, ok := attrs["checkout_options"]; ok {
			t.Fatalf("checkout_options must not contain unsupported redirect fields: %#v", attrs["checkout_options"])
		}
		productOptions := attrs["product_options"].(map[string]any)
		if got := productOptions["redirect_url"]; got != "https://app.example/subscription?ok=1" {
			t.Fatalf("bad success redirect_url: %#v", got)
		}
		checkoutData := attrs["checkout_data"].(map[string]any)
		quantities := checkoutData["variant_quantities"].([]any)
		q := quantities[0].(map[string]any)
		if q["variant_id"] != "variant-1" || q["quantity"] != float64(3) {
			t.Fatalf("bad variant_quantities: %#v", quantities)
		}
		w.Header().Set("Content-Type", "application/vnd.api+json")
		_, _ = w.Write([]byte(`{"data":{"id":"chk-123","attributes":{"url":"https://checkout.example/abc"}}}`))
	}))
	defer srv.Close()

	c, err := NewClient(Config{
		APIKey: "test-key", StoreID: "store-1", WebhookSecret: "s",
		BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.CreateHostedCheckout(context.Background(), CheckoutRequest{
		VariantID:          "variant-1",
		UserEmail:          "u@example.com",
		CustomData:         BuildCheckoutCustomData("order-7", "user-9", "advisor_credits_small"),
		VariantQuantity:    3,
		RedirectURL:        "https://app.example/pricing?cancelled=1",
		SuccessRedirectURL: "https://app.example/subscription?ok=1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.URL != "https://checkout.example/abc" {
		t.Errorf("bad url: %q", resp.URL)
	}
}

func TestCreateHostedCheckout_RequiresVariant(t *testing.T) {
	c, _ := NewClient(Config{APIKey: "k", StoreID: "1", WebhookSecret: "s"})
	_, err := c.CreateHostedCheckout(context.Background(), CheckoutRequest{})
	if err == nil {
		t.Error("expected error for empty variant")
	}
}

func TestFromEnv_RequiresAllThree(t *testing.T) {
	t.Setenv("LEMONSQUEEZY_API_KEY", "k")
	t.Setenv("LEMONSQUEEZY_STORE_ID", "")
	t.Setenv("LEMONSQUEEZY_WEBHOOK_SECRET", "s")
	if _, ok := FromEnv(); ok {
		t.Error("missing store id should disable")
	}
	t.Setenv("LEMONSQUEEZY_STORE_ID", "1")
	if _, ok := FromEnv(); !ok {
		t.Error("all three set should enable")
	}
}
