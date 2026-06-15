package main

// loop_locale.go — locale plumbing for Loop / scheduler entry points.
//
// HTTP requests already carry the user's locale via the X-User-Language
// header (see requestLogger middleware in main.go). Background loops
// have no request — they wake on a timer, fan out per fund, and write
// rows that the user later reads in the UI. If the language those rows
// are produced in doesn't match the user's preference, the en-US user
// sees Chinese in the dashboard despite the front end being fully
// English.
//
// This file plugs that hole. ctxWithFundLocale and ctxWithUserLocale
// resolve the right locale for a (fund, owner) pair and attach it to
// the per-iteration context. Three different keys are written so every
// existing reader (cmd/server.LanguageFromContext, marketdata.Language,
// i18nmsg.FromCtx) sees the same value:
//
//   - userLanguageKey : consumed by the existing wiring_adapters.go
//     branches that already call LanguageFromContext.
//   - marketdata.WithLanguage : consumed by the marketdata translator.
//   - i18nmsg.WithLocale     : consumed by the new bundle in Step 1
//     plus everything we'll migrate in Steps 4 / 10.
//
// The helpers are deliberately read-mostly: they NEVER call out to a
// network, NEVER block on locks the loop scheduler doesn't already
// hold, and NEVER fail loud — a DB error degrades to LocaleZH so the
// loop continues making forward progress (the worst case is a row
// produced in zh-CN, which is the historical behaviour anyway).

import (
	"context"
	"database/sql"
	"strings"

	"github.com/fundai/server/internal/i18nmsg"
	"github.com/fundai/server/internal/marketdata"
)

// loopOriginContextKey distinguishes context values stamped by a loop
// from those stamped by the HTTP middleware. Useful for metric labels
// and debug logging — a translator that sees a "!!MISSING:" miss can
// inspect the origin to know whether the gap was triggered by an HTTP
// caller or a background tick.
type loopOriginContextKey string

const loopOriginKey loopOriginContextKey = "loopOrigin"

// withLoopOrigin tags ctx with a short identifier (e.g. "ab_shadow",
// "agent_self_learning"). Loop entry points call this once per tick so
// every downstream metric / log can be filtered by the originating
// loop without parsing the call stack.
func withLoopOrigin(ctx context.Context, name string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(name) == "" {
		return ctx
	}
	return context.WithValue(ctx, loopOriginKey, name)
}

// loopOriginFromCtx is the inverse of withLoopOrigin. Returns "" for
// HTTP-driven contexts so loop-only metric paths can short-circuit on
// the non-loop case.
func loopOriginFromCtx(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(loopOriginKey).(string)
	return v
}

// localeWriter is the minimal slice of *FundRepo we need here.
// Threading the full repo through every loop would couple this file to
// half the dependency graph; the interface keeps the helper testable
// with a fake.
type fundLocaleLookup interface {
	GetPreferredLanguage(ctx context.Context, fundID string) (string, error)
}

// ctxWithFundLocale resolves the locale for a single fund and stamps it
// onto ctx via every reader's preferred key. fundID may be empty (some
// loops, like the daily-picks publisher, don't have a per-fund scope);
// in that case the helper falls back to the publisher / system default
// of zh-CN, identical to the historical behaviour, and returns ctx
// unchanged so callers can treat the helper as idempotent.
//
// Errors are intentionally swallowed: a transient DB hiccup must not
// block a loop that was already going to run with the historical
// default. The metric counter on i18nmsg covers the visible "missing
// translation" symptom if anything goes wrong further downstream.
func ctxWithFundLocale(ctx context.Context, repo fundLocaleLookup, fundID string) context.Context {
	loc := i18nmsg.LocaleZH
	fundID = strings.TrimSpace(fundID)
	if repo != nil && fundID != "" {
		if raw, err := repo.GetPreferredLanguage(ctx, fundID); err == nil {
			loc = i18nmsg.Normalize(raw)
		}
	}
	return applyLocale(ctx, loc)
}

// userLocaleLookup mirrors fundLocaleLookup but for users. We don't
// have a dedicated user repo (see Step 2 notes) so the helper accepts a
// raw *sql.DB and runs the COALESCE itself. Same forgiving error
// semantics as ctxWithFundLocale.
func ctxWithUserLocale(ctx context.Context, db *sql.DB, userID string) context.Context {
	loc := i18nmsg.LocaleZH
	userID = strings.TrimSpace(userID)
	if db != nil && userID != "" {
		var raw sql.NullString
		err := db.QueryRowContext(ctx, `
			SELECT COALESCE(NULLIF(TRIM(preferred_language), ''), 'zh-CN')
			FROM users
			WHERE id = $1
			LIMIT 1
		`, userID).Scan(&raw)
		if err == nil {
			loc = i18nmsg.Normalize(raw.String)
		}
	}
	return applyLocale(ctx, loc)
}

// ctxWithFundLocaleByDB is the *sql.DB-only counterpart of
// ctxWithFundLocale, for loops that don't have a *FundRepo handy
// (drawdown, promotion, activity_retention). The COALESCE chain
// matches FundRepo.GetPreferredLanguage so the resolved locale is
// identical regardless of which entry point a loop uses.
func ctxWithFundLocaleByDB(ctx context.Context, db *sql.DB, fundID string) context.Context {
	loc := i18nmsg.LocaleZH
	fundID = strings.TrimSpace(fundID)
	if db != nil && fundID != "" {
		var raw sql.NullString
		err := db.QueryRowContext(ctx, `
			SELECT COALESCE(
			    NULLIF(TRIM(f.preferred_language), ''),
			    NULLIF(TRIM(u.preferred_language), ''),
			    'zh-CN'
			)
			FROM funds f
			LEFT JOIN fund_companies c ON c.id = f.company_id
			LEFT JOIN users u          ON u.id = c.owner_id
			WHERE f.id = $1
		`, fundID).Scan(&raw)
		if err == nil {
			loc = i18nmsg.Normalize(raw.String)
		}
	}
	return applyLocale(ctx, loc)
}

// applyLocale stamps the resolved locale onto every context key the
// codebase reads from. Centralising this here means future readers
// only have to update a single line to register a new context key.
func applyLocale(ctx context.Context, loc i18nmsg.Locale) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	// cmd/server.LanguageFromContext — pre-existing readers in
	// wiring_adapters.go already key off this value.
	ctx = context.WithValue(ctx, userLanguageKey, UserLanguage(loc))
	// marketdata translator (independent ctx key, populated by the
	// HTTP middleware as well; replicate here so every quote
	// translation honours the loop's locale).
	ctx = marketdata.WithLanguage(ctx, string(loc))
	// i18nmsg bundle — used by Step 4 prompts and Step 10 fallbacks.
	ctx = i18nmsg.WithLocale(ctx, loc)
	return ctx
}
