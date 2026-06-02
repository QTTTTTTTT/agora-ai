// wiring_social.go — Sprint 9.3 default social.Registry factory.
//
// Each platform is opt-in via an environment flag so brand-new
// deployments don't fan out HTTP calls to Xueqiu / StockTwits /
// Reddit on day one without operator consent. Setting the flag
// to anything non-empty turns the provider on; absence disables
// it cleanly.
//
//	SOCIAL_PROVIDERS_ENABLED=1            global kill-switch
//	SOCIAL_PROVIDER_XUEQIU=1              enable Xueqiu provider
//	SOCIAL_XUEQIU_GUEST_TOKEN=<cookie>    guest cookie for the public timeline
//	SOCIAL_PROVIDER_STOCKTWITS=1          enable StockTwits provider
//	SOCIAL_STOCKTWITS_ACCESS_TOKEN=<tok>  optional paid-tier token
//	SOCIAL_PROVIDER_REDDIT=1              enable r/wallstreetbets provider
//	SOCIAL_REDDIT_MIN_UPVOTES=10          optional upvote floor
//	SOCIAL_REDDIT_USER_AGENT=...          optional UA override
//
// The shared *http.Client is reused across providers so connection
// pooling is effective when several symbols are queried in
// parallel by the registry.
package main

import (
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/fundai/server/internal/social"
	"github.com/fundai/server/internal/social/provider/reddit"
	"github.com/fundai/server/internal/social/provider/stocktwits"
	"github.com/fundai/server/internal/social/provider/xueqiu"
)

// buildSocialRegistryFromEnv constructs a social.Registry by
// reading the per-provider env flags described in the file
// header. Returns nil when the global kill-switch is off OR no
// provider is individually enabled — workflowServiceAdapter then
// stays in news-only mode (pre-9.3 behaviour).
func buildSocialRegistryFromEnv(logger *slog.Logger) *social.Registry {
	if !envFlagEnabled("SOCIAL_PROVIDERS_ENABLED") {
		return nil
	}
	client := &http.Client{Timeout: 10 * time.Second}
	providers := make([]social.Provider, 0, 3)
	if envFlagEnabled("SOCIAL_PROVIDER_XUEQIU") {
		providers = append(providers, xueqiu.New(xueqiu.Options{
			HTTPClient:  client,
			GuestCookie: strings.TrimSpace(os.Getenv("SOCIAL_XUEQIU_GUEST_TOKEN")),
		}))
	}
	if envFlagEnabled("SOCIAL_PROVIDER_STOCKTWITS") {
		providers = append(providers, stocktwits.New(stocktwits.Options{
			HTTPClient:  client,
			AccessToken: strings.TrimSpace(os.Getenv("SOCIAL_STOCKTWITS_ACCESS_TOKEN")),
		}))
	}
	if envFlagEnabled("SOCIAL_PROVIDER_REDDIT") {
		minUp, _ := strconv.Atoi(strings.TrimSpace(os.Getenv("SOCIAL_REDDIT_MIN_UPVOTES")))
		providers = append(providers, reddit.New(reddit.Options{
			HTTPClient: client,
			UserAgent:  strings.TrimSpace(os.Getenv("SOCIAL_REDDIT_USER_AGENT")),
			MinUpvotes: minUp,
		}))
	}
	if len(providers) == 0 {
		return nil
	}
	return social.NewRegistry(providers, social.RegistryOptions{
		PerProviderTimeout: 8 * time.Second,
		PerProviderLimit:   25,
		MaxAge:             24 * time.Hour,
	}, logger)
}

// envFlagEnabled is the canonical truthiness check we apply to
// every social.* env var. Mirrors the convention used by the
// existing FUND_DEBATE_ROUNDTABLE flag so operators have a single
// mental model: "set to anything that isn't 0 / false / no".
func envFlagEnabled(name string) bool {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return false
	}
	switch strings.ToLower(v) {
	case "0", "false", "no", "off", "disabled":
		return false
	}
	return true
}
