# Release QA & Launch Playbook

This is the operator runbook for taking the platform (server + web + miniapp +
Android RN) from green-on-CI to **Play Internal → Closed Beta → Production**
and **WeChat Miniapp Submitted → Released**.

It is intentionally checklist-shaped — every section is a copy-pasteable list
of commands and gates. Print it out before a release window.

---

## 0. Pre-flight gates (everyone, every release)

Run these in the order listed. If any fails, **do not proceed**.

```bash
# 1. Repo hygiene
bash scripts/verify.sh

# 2. Backend tests, vet, vulncheck
cd server
go test ./... -count=1 -race
go vet ./...
# govulncheck pinned — bumping requires the SLA in §0.1 below.
go run golang.org/x/vuln/cmd/govulncheck@v1.3.0 ./...

# 3. Shared client + web + android typecheck
cd ..
npm install --legacy-peer-deps
npm --workspace shared/api-client run build
npm --workspace web run build
npm --workspace android run typecheck

# 4. Docker compose smoke
docker compose up -d --build postgres web-search-mcp app
sleep 30
curl -fsS http://localhost:8080/api/health
docker compose down -v
```

CI mirrors all of the above and gates merges; this is the local rehearsal.

---

## 0.1. govulncheck pinning + bump SLA

The pre-flight gate above runs `govulncheck@v1.3.0`. It is **pinned, not
`@latest`**, in three places that must stay in lockstep:

  - `.github/workflows/ci.yml` (the CI gate)
  - `scripts/verify.sh` (the local repro runner)
  - `docs/RELEASE_QA_PLAYBOOK.md` §0 step 2 (this very playbook)

**Why pin.** `@latest` re-resolves on every CI run; a fresh govulncheck
release that adds a new vulnerability rule (or an over-eager false
positive) can turn a previously-green main red without any code change
in this repo. We've also seen weeks where the upstream module had
build errors against newer Go toolchains. Pinning lets every CI run
of the same commit produce the same scan output, which is the only
way "scan was clean on this commit" is a meaningful claim.

**Bump SLA.** Refresh the pin **monthly**, on the first business day,
and **immediately** if a CVE in the Go ecosystem is publicly disclosed
between scheduled bumps. Procedure:

  1. Pick the new version: visit
     https://pkg.go.dev/golang.org/x/vuln/cmd/govulncheck and use the
     latest **`v1.x.x`** stable (skip `vN.M.0-DATE-HASH` pre-release
     pseudo-versions; we want only tagged releases).
  2. Run a dry scan against the current `main`:
     ```bash
     cd server
     go run golang.org/x/vuln/cmd/govulncheck@<NEW_VERSION> ./...
     ```
     If it reports new findings that the pinned version didn't, treat
     each as a normal vulnerability response (open an issue, decide:
     fix, accept-risk-with-expiry, or upstream-already-mitigated).
     Do not proceed to the bump until the scan is clean OR every new
     finding has an explicit triage decision recorded.
  3. Update the version string in **all three** files in one PR.
     A grep helper:
     ```bash
     git grep -n 'govulncheck@'
     ```
     The PR title format is `chore(deps): bump govulncheck pin to <NEW_VERSION>`
     and the description must list every CVE the new version's
     database picked up against `main` (zero is fine — record that
     too).
  4. Land the PR through the normal review path. The CI run on the
     PR is itself the proof that the new version is build-clean
     against our codebase.

**Owner.** Backend on-call rotates the bump as part of the Monday
triage routine (see `docs/MONDAY_TRIAGE_PLAYBOOK.md`). If a CVE drops
mid-week and the rotating on-call is unavailable, any backend
maintainer may land the bump out-of-cycle following the same
procedure.

---

## 1. Three-surface smoke test suite

The **same 7 user journeys** must pass on each of (web, miniapp, Android RN)
before promoting a release. Use `scripts/smoke-test.sh` to run the API-side
checks; the UI sides are manual.

| # | Journey                       | Web (Playwright)                    | Miniapp                                  | Android (manual)                     |
|---|-------------------------------|-------------------------------------|------------------------------------------|--------------------------------------|
| 1 | Sign in (email/password)      | login.spec.ts                       | tap 我的 → 登录                         | LoginScreen, real creds              |
| 2 | Forgot password               | forgot-password.spec.ts             | 忘记密码 → 邮件 link                    | ForgotPasswordScreen                 |
| 3 | View latest decision          | decision-center.spec.ts             | 决策 tab                                 | Decisions tab                        |
| 4 | Skill approval inbox          | skill-inbox.spec.ts                 | n/a (admin only)                         | n/a (admin only)                     |
| 5 | Browse memory + reflection    | memory.spec.ts                      | 记忆 tab                                 | Memory tab (agent + reflection)      |
| 6 | Switch active fund            | dashboard.spec.ts                   | 首页 fund picker                         | Home → tap another fund card         |
| 7 | Switch language (zh ↔ en)     | preferences.spec.ts                 | 我的 → 语言                              | More → language tap                  |

For each row, record **pass / fail / N/A** in the release ticket. Failures
block the release.

---

## 2. Performance baselines

Targets are taken from the original plan and validated as part of the QA
gate. **Sustained breaches** of any single line block a Production push;
single-spike breaches require a Sentry incident before proceeding.

| Metric                      | Target          | Measurement                                            |
|-----------------------------|-----------------|--------------------------------------------------------|
| APK size (release / signed) | < 30 MB         | `ls -lh android/android/app/build/outputs/apk/release` |
| Cold app start (Android)    | < 2.0 s         | `adb shell am start -W com.fundai.platform`            |
| Time-to-interactive (first) | < 3.0 s         | Sentry performance → first-content-paint span          |
| API p95 latency             | < 800 ms        | `/api/metrics` → Prometheus `histogram_quantile`       |
| API error rate (5xx)        | < 0.1 %         | Prometheus rules in `prometheus/alerts.yml`            |
| Server boot                 | < 30 s          | `docker compose up`-to-`/api/health` 200               |

Run `scripts/perf-baseline.sh` (template provided) to record the numbers at
each release into `docs/perf-history.csv` so we can graph drift over time.

---

## 3. OWASP MASVS L1 self-check (Android)

Tick the line, attach a screenshot or build-log link in the release ticket.

- [ ] **V1 (architecture)**  Threat model is current — devices, data, and
      surface are documented in `docs/SYSTEM_SPEC.md`.
- [ ] **V2 (data storage)**  No PII / token in plaintext in `AsyncStorage`
      / SharedPreferences. Token lives in `react-native-keychain`
      (`secureStore.ts`).
- [ ] **V2.4 (clipboard)**  No automatic clipboard writes of sensitive data.
- [ ] **V3 (crypto)**  TLS only; no custom crypto.
- [ ] **V4 (auth)**  Failed-login lockout works (`failed_login_attempts` +
      `locked_until` in `users`). Tested via login.spec.ts.
- [ ] **V5 (network)**  SSL pinning is configured for **production** hosts
      in `android/src/lib/security.ts` → `hostFingerprints`.
- [ ] **V6 (platform)**  `FLAG_SECURE` is on (`enableScreenCapturePrevention`
      called in `App.tsx`).
- [ ] **V7 (code quality)**  ProGuard / R8 on (`proguard-rules.pro`
      referenced from `build.gradle`).
- [ ] **V7.4 (third-party libs)**  `npm audit` reviewed; no high / critical
      runtime advisories.
- [ ] **V8 (resilience)**  Root / hook detection runs and reports a
      Sentry event (`telemetry.ts` → `security.compromised`). Decision on
      whether to block sign-in for rooted devices is documented in the
      release ticket.

Anything unchecked blocks Play Internal promotion.

---

## 4. Web + Miniapp readiness

### Web

- [ ] `npm run build` produces a `web/dist/` < 5 MB compressed.
- [ ] `web/playwright-report` shows all spec files passing.
- [ ] CSP / security headers configured in the reverse proxy (see
      `docker-compose.prod.yml`).
- [ ] Cookie names + SameSite=Lax verified.

### Miniapp

- [ ] AppID swapped in `miniapp/project.config.json` (see
      `docs/MINIAPP_DEPLOYMENT.md`).
- [ ] `node scripts/validate-miniapp.mjs miniapp` is green.
- [ ] All `wx.request` URLs use HTTPS — checked in `miniapp/utils/api.js`.
- [ ] `request_legalDomain` whitelist in mp.weixin.qq.com matches the
      production API host.
- [ ] `mixed` plan badge renders (smoke test: open Decisions tab with a
      `mixed` plan in the DB).

---

## 5. Android — Play Store promotion ladder

### 5.1 Play Internal Track (every tagged release)

1. Locally tag the release: `git tag v0.2.0 && git push origin v0.2.0`.
2. CI's `android-release` job builds a signed APK and (when
   `PLAY_SERVICE_ACCOUNT_JSON` is configured) uploads to Internal Track.
3. The team's Play Console review group (set in Play → Internal testing →
   Testers) gets the build within ~minutes.
4. The release engineer runs the §1 smoke test on a real device.

### 5.2 Closed Beta (every 2 internal releases or 1 week, whichever sooner)

1. From the Play Console, promote the Internal release to **Closed Testing
   → Closed Beta** track (no rebuild — Play just re-flags the artifact).
2. Recruit ≥ 10 testers from the beta group. Track responses for 72 h.
3. Critical bugs (P0 / P1) → rollback to the previous Internal release and
   issue a `v0.2.1-hotfix` patch.

### 5.3 Production (after Closed Beta + sign-off)

1. Final §1 + §2 + §3 sweep is recorded in the release ticket.
2. Promote Closed Beta → Production in the Play Console with a **staged
   rollout** (start 10 %, double daily until 100 %).
3. Monitor `crash_free_users` in Sentry — < 99.0 % triggers an immediate
   halt of the rollout.

---

## 6. WeChat Miniapp — Submit & Release

1. Confirm the §4 Miniapp checklist is fully checked.
2. Push the Miniapp from WeChat Developer Tools (上传).
3. mp.weixin.qq.com → 版本管理 → 提交审核. Use category **金融-理财**
   and attach the regulatory license (see
   `docs/MINIAPP_DEPLOYMENT.md` §5).
4. Review SLA is typically 1-3 business days; once approved click 发布.
5. On release, monitor:
   - `wx-login` failure rate via Prometheus (`miniapp_wechat_login_total` etc.)
   - Server 401 spikes (often signal AppSecret rotation drift).

---

## 7. Rollback procedure

A failed production deployment recovers in three steps:

1. **Server**: `docker compose down && git checkout v0.1.0 && docker compose
   up -d --build` (compose tags follow git tags 1:1).
2. **Web**: re-deploy the previous `web/dist/` artifact stored in the
   release bucket.
3. **Miniapp**: 版本管理 → 体验版 → 回退至上一审核通过的版本.
4. **Android**: Play Console → Production track → **Halt rollout** + open a
   `v0.x.y-hotfix` branch off the previous tag.

All four steps are reversible within < 15 minutes if the artifacts are
preserved per the team's retention policy (90 days in S3 / OSS).

---

## 8. Sign-off

The release ticket must record:

- Release version + git SHA.
- Pre-flight (§0) full log.
- Three-surface smoke (§1) results.
- Performance baseline (§2) numbers.
- MASVS L1 (§3) tick list.
- Play / Miniapp store URLs.
- Owner on-call for the next 24 h.

Sign-off requires at least one engineer + one product lead.
