# Alert delivery — Sprint 12

> How alerts go from a misbehaving server, through Prometheus and
> alertmanager, into the audit trail and (optionally) on-call
> rotation.

## Pipeline

```
fundai-server (/api/metrics)
        │
        ▼
   prometheus  ── rules ─→  alertmanager  ──┬─→ on-call (PagerDuty / Lark)
                                            │
                                            └─→ webhook → fundai-server
                                                          POST /api/admin/alerts/webhook
                                                            │
                                                            ▼
                                                     admin_alert_events
                                                            │
                                                            ▼
                                                  admin UI: /admin → "Alerts"
```

## Components

### 1. Prometheus rules

The shipped rules live at `prometheus/alerts.yml`. The S12 group is
`fundai-pm-decision-source`, which reads the
`fundai_pm_decision_total{source,category,provider}` counter
introduced in S11.4. The rules cover:

| Alert                                       | Threshold                       | Severity |
| ------------------------------------------- | ------------------------------- | -------- |
| `FundAIPMDecisionFallbackRateHigh`          | fallback share > 5% for 30m     | warning  |
| `FundAIPMDecisionFallbackRateCritical`      | fallback share > 25% for 10m    | critical |
| `FundAIPMDecisionUnknownCategoryDrift`      | new errorclass shape seen 6h    | info     |
| `FundAIPMDecisionAuthFailedSpike`           | > 5 auth_failed in 15m          | critical |
| `FundAIPMDecisionContextLengthSpike`        | > 10 context_length in 1h       | warning  |
| `FundAIPMDecisionBudgetExhausted`           | any budget_exceeded             | warning  |

Inhibition: `FundAIPMDecisionFallbackRateCritical` silences the
warning version (same component) to avoid double-paging.

### 2. Alertmanager

Reference config at `prometheus/alertmanager.yml`. Two receivers:

- `fundai-internal` — the in-app webhook. Always fires (audit trail).
- `fundai-oncall` — placeholder. Wire your real PagerDuty /
  Lark / OpsGenie integration here.

The bearer token between alertmanager and the server is the env
var `FUNDAI_ALERT_WEBHOOK_SECRET`. The server's
`/api/admin/alerts/webhook` endpoint will refuse all requests when
the env var is unset (503) — there is no "open" fallback by design.

### 3. Webhook endpoint

`POST /api/admin/alerts/webhook` is the only admin route that does
NOT go through the user-id auth gate; it authenticates by bearer
token (`Authorization: Bearer <secret>`). Implementation:
`server/cmd/server/admin_alerts.go::handleAlertWebhook`.

Idempotency: a unique partial index on
`(fingerprint, status, starts_at)` lets alertmanager retry the
same payload after a network blip without duplicating the row.
The webhook response includes `{ingested, deduped, failed}` so
alertmanager logs surface the deduplication rate.

### 4. Admin dashboard

`/admin → Alerts dashboard` (component
`web/src/components/AdminAlertsSection.tsx`):

- Status filter: firing / resolved / all.
- Per-alert row drill-down: labels + annotations JSON, plus the
  acknowledgement timeline.
- "Acknowledge" button writes a `MutationEvent` audit row with
  `action=alert_acknowledge`, capturing the optional note.

## Operational runbook

When `FundAIPMDecisionFallbackRateHigh` fires:

1. Open `/admin → LLM health dashboard`, switch to a 1h window.
2. Sort the category table by count; identify the dominant failure
   shape.
3. If `provider` is populated, check that provider's status page.
4. Drill into a sample plan via the plan_id link (S12-alt makes it
   clickable straight into the user's decision center).
5. Acknowledge the alert with a one-line note for the retro
   timeline.

When `FundAIPMDecisionAuthFailedSpike` fires:

1. Rotate the platform-level API key for the affected provider.
2. Inspect `user_provider_keys` for any user-level overrides that
   may have expired.
3. Update the `FUNDAI_*_API_KEY` secret and roll the server pods.

## Local development

The `docker-compose` examples don't yet ship prometheus +
alertmanager containers — production deployments tend to run
those in the infra cluster, not next to the app. To smoke-test
locally:

```bash
prometheus --config.file=prometheus/prometheus.yml
alertmanager --config.file=prometheus/alertmanager.yml
FUNDAI_ALERT_WEBHOOK_SECRET=local-dev go run ./cmd/server
```
