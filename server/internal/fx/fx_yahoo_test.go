package fx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// stubYahooEndpoint returns an httptest.Server that mimics the
// /v7/finance/quote shape. The script lets the caller drive the
// response body / status per call.
func stubYahooEndpoint(t *testing.T, body string, status int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v7/finance/quote") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if r.URL.Query().Get("symbols") == "" {
			t.Error("missing symbols param")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func TestYahooProvider_Fetch_HappyPath(t *testing.T) {
	srv := stubYahooEndpoint(t, `{"quoteResponse":{"result":[{"symbol":"USDCNY=X","regularMarketPrice":7.18,"regularMarketTime":1717200000,"currency":"CNY"}]}}`, 200)
	defer srv.Close()
	p := NewYahooProvider(YahooProviderOptions{BaseURL: srv.URL, HTTPClient: srv.Client()})
	r, err := p.Fetch(context.Background(), "USD", "CNY")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if r.Rate != 7.18 || r.Source != "yahoo" {
		t.Errorf("rate = %+v", r)
	}
	if r.RateAt.IsZero() {
		t.Error("expected non-zero rate_at")
	}
}

func TestYahooProvider_Fetch_RateLimited(t *testing.T) {
	srv := stubYahooEndpoint(t, `{}`, http.StatusTooManyRequests)
	defer srv.Close()
	p := NewYahooProvider(YahooProviderOptions{BaseURL: srv.URL, HTTPClient: srv.Client()})
	_, err := p.Fetch(context.Background(), "USD", "CNY")
	if !errors.Is(err, ErrRateUnavailable) {
		t.Errorf("err = %v, want ErrRateUnavailable", err)
	}
}

func TestYahooProvider_Fetch_5xx(t *testing.T) {
	srv := stubYahooEndpoint(t, `{}`, http.StatusBadGateway)
	defer srv.Close()
	p := NewYahooProvider(YahooProviderOptions{BaseURL: srv.URL, HTTPClient: srv.Client()})
	_, err := p.Fetch(context.Background(), "USD", "CNY")
	if !errors.Is(err, ErrRateUnavailable) {
		t.Errorf("err = %v, want ErrRateUnavailable", err)
	}
}

func TestYahooProvider_Fetch_ZeroRate(t *testing.T) {
	srv := stubYahooEndpoint(t, `{"quoteResponse":{"result":[{"symbol":"USDCNY=X","regularMarketPrice":0,"regularMarketTime":1717200000,"currency":"CNY"}]}}`, 200)
	defer srv.Close()
	p := NewYahooProvider(YahooProviderOptions{BaseURL: srv.URL, HTTPClient: srv.Client()})
	_, err := p.Fetch(context.Background(), "USD", "CNY")
	if !errors.Is(err, ErrRateUnavailable) {
		t.Errorf("err = %v, want ErrRateUnavailable", err)
	}
}

func TestYahooProvider_Fetch_CrossPairRefused(t *testing.T) {
	p := NewYahooProvider(YahooProviderOptions{})
	_, err := p.Fetch(context.Background(), "CNY", "HKD")
	if !errors.Is(err, ErrUnsupportedPair) {
		t.Errorf("err = %v, want ErrUnsupportedPair", err)
	}
}

func TestYahooProvider_Fetch_UnsupportedCurrency(t *testing.T) {
	p := NewYahooProvider(YahooProviderOptions{})
	_, err := p.Fetch(context.Background(), "USD", "BTC")
	if !errors.Is(err, ErrUnsupportedPair) {
		t.Errorf("err = %v, want ErrUnsupportedPair", err)
	}
}

func TestYahooProvider_Fetch_Identity(t *testing.T) {
	p := NewYahooProvider(YahooProviderOptions{})
	r, err := p.Fetch(context.Background(), "USD", "USD")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if r.Rate != 1.0 {
		t.Errorf("rate = %v", r.Rate)
	}
}

func TestYahooProvider_Fetch_TimeoutContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	p := NewYahooProvider(YahooProviderOptions{BaseURL: srv.URL, HTTPClient: srv.Client()})
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()
	_, err := p.Fetch(ctx, "USD", "CNY")
	if err == nil {
		t.Error("expected error from canceled ctx")
	}
}
