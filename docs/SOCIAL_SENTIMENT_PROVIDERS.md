# Sprint 9.3 — Social Sentiment Providers

This document covers the integration of three retail social media platforms
(Xueqiu, StockTwits, r/wallstreetbets) into the existing sentiment scoring
pipeline.

## 1. Why

Until 9.3 the workflow's sentiment block was fed only wire-news (Reuters,
Bloomberg, akshare). That blind-spot matters most on retail-driven names
(AMC, GME, Nvidia, meme baskets, A-share single-day movers) where the
news flow lags or completely misses the move that's already happened on
the boards. Sprint 9.3 closes that gap with three providers chosen to
cover the markets we trade:

- **Xueqiu** (xueqiu.com) — A-shares, HK, US-listed China names.
- **StockTwits** — US equities + crypto.
- **r/wallstreetbets** — highest-frequency, highest-noise US single-name
  retail forum.

## 2. Data flow

```text
                    +-------------------+
                    |   social.Registry |
                    +---------+---------+
                              | parallel FetchPosts per provider
            +-----------------+-----------------+
            |                 |                 |
       Xueqiu            StockTwits          Reddit-WSB
       (cookie)         (HTTP REST)         (JSON listing)
            |                 |                 |
            +-----------------+-----------------+
                              v
                  []sentiment.Item (one per post)
                              v
        runtimeResearcherPool.collectSocialItems
                              v
        merged with news-derived sentiment.Item batch
                              v
              sentimentScorer.Score(ctx, items)
                              v
            sentiment.AggregateBySymbol → sentiment block
                              v
           macroSentimentBlock / collectSentimentDebateBlock
                              v
               consumed by macro brief + debate input
```

Posts are emitted as `sentiment.Item` rows — the same shape news items
already use — so the downstream scorer and aggregator do not need to
know whether a row came from Reuters or Reddit. The only externally
visible difference is the `Item.Source` value: `xueqiu`,
`stocktwits`, or `reddit_wsb`.

## 3. Configuration

Each platform is opt-in via an environment flag so brand-new deployments
do not fan out HTTP calls without operator consent. See `.env.example`
for the full set; the canonical knobs are:

| Env var | Purpose |
| --- | --- |
| `SOCIAL_PROVIDERS_ENABLED` | Global kill-switch. Off (default) = feature disabled. |
| `SOCIAL_PROVIDER_XUEQIU` | Enable Xueqiu provider. |
| `SOCIAL_XUEQIU_GUEST_TOKEN` | Guest cookie (`xq_a_token`) for the public timeline. |
| `SOCIAL_PROVIDER_STOCKTWITS` | Enable StockTwits provider. |
| `SOCIAL_STOCKTWITS_ACCESS_TOKEN` | Optional paid-tier token (raises rate limit). |
| `SOCIAL_PROVIDER_REDDIT` | Enable r/wallstreetbets provider. |
| `SOCIAL_REDDIT_MIN_UPVOTES` | Optional upvote floor; posts under this score are dropped. |
| `SOCIAL_REDDIT_USER_AGENT` | Optional UA override (Reddit blocks default Go UA). |

## 4. Code locations

- `server/internal/social/social.go` — `Provider` interface, `Registry`
  with parallel fan-out, options, sentinel errors.
- `server/internal/social/provider/xueqiu/xueqiu.go` — Xueqiu provider.
- `server/internal/social/provider/stocktwits/stocktwits.go` — StockTwits
  provider.
- `server/internal/social/provider/reddit/reddit.go` — r/wallstreetbets
  provider.
- `server/cmd/server/wiring_social.go` — env-driven `social.Registry`
  factory and `envFlagEnabled` truthiness helper.
- `server/cmd/server/wiring_adapters.go` — `runtimeResearcherPool.
  collectSocialItems` (per-symbol social fetch + dedup), wired into
  `macroSentimentBlock` and `collectSentimentDebateBlock`.
- `server/cmd/server/main.go` — top-level `WithSocialRegistry`
  installation.

## 5. Failure handling

| Failure | Behaviour |
| --- | --- |
| Global kill-switch off | `buildSocialRegistryFromEnv` returns nil; workflow stays news-only. |
| One provider unreachable | `Registry.FetchPosts` logs `social.provider.fetch_failed` and returns the items from the surviving providers. |
| All providers unreachable | `Registry.FetchPosts` returns `ErrAllProvidersFailed`; the caller drops the social batch and the sentiment block falls back to news-only. |
| Symbol not ticker-like | `collectSocialItems` skips the symbol (avoids hitting the providers with macro keywords like "macro_news"). |
| Per-provider timeout | `RegistryOptions.PerProviderTimeout` (default 8s) cancels stuck providers without stalling the run. |

## 6. Rate-limit posture

- **Xueqiu**: aggressive rate-limit when a single guest token is used too
  fast. Production deployments with high QPS should plug a session pool
  in via a custom `*http.Client`.
- **StockTwits**: ~200 requests / IP / hour on the unauthenticated tier.
  The daily PM loop queries each candidate symbol once, so we sit well
  under the limit.
- **Reddit**: 1 req / 2s on the `.json` listing endpoint without OAuth.
  More than enough headroom for the daily flow.

## 7. Testing

- Per-provider unit tests use `net/http/httptest` so the suite is
  network-isolated.
- `social_test.go` exercises the Registry's parallel fan-out,
  per-provider timeout, fail-soft behaviour, and the
  `ErrAllProvidersFailed` sentinel.
- `wiring_social_test.go` covers the env-driven factory and the
  `collectSocialItems` helper (per-symbol dedup, non-ticker skipping,
  nil-safe paths).

## 8. Future work

- Embedding-based dedup so two near-identical Xueqiu reposts of the
  same screenshot don't double-count the sentiment.
- A weighted scorer that gives higher trust to high-karma Reddit
  authors and StockTwits accounts with self-declared trader flair.
- A persistent post cache so a re-run of the same day doesn't re-fetch
  posts already scored (the news pipeline already does this; the
  social pipeline currently does not).
