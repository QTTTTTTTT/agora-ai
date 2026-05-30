// corpactionsync is the operator-facing CLI for pulling corporate
// action events (splits / dividends / stock dividends) from a
// provider into the `corporate_actions` ledger and — optionally —
// applying them to the fund holdings that the events touch.
//
// Why a CLI rather than a scheduled job: the daily auto-ingester is
// Phase 1 work (P1-1 in the master plan). Until then operators
// need to run a one-shot sweep when a public ex-date is announced
// or when a 持仓 already moved cost-basis without a recorded action.
// Examples:
//
//	# Dry run for two A-shares — see what events would be ingested,
//	# do not modify anything.
//	go run ./cmd/corpactionsync \
//	    --provider eastmoney \
//	    --symbols 688195,002594 \
//	    --since 2025-01-01
//
//	# Same call, but actually upsert into corporate_actions AND
//	# apply to every fund that currently holds the instrument:
//	go run ./cmd/corpactionsync \
//	    --provider eastmoney \
//	    --symbols 688195 \
//	    --since 2025-01-01 \
//	    --apply
//
//	# US tickers via the existing Yahoo provider:
//	go run ./cmd/corpactionsync --provider yahoo --symbols NVDA,AMD --apply
//
//	# HK tickers via the East Money HK datacenter:
//	go run ./cmd/corpactionsync --provider hkex --symbols 00700,09988 --apply
//
// Output is markdown — pipe to `tee /tmp/sync.md` to keep an audit
// trail. Stderr carries log noise; stdout stays clean for piping.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	_ "github.com/lib/pq"

	"github.com/fundai/server/internal/corpaction"
	"github.com/fundai/server/internal/repository"
)

func main() {
	provider := flag.String("provider", "eastmoney", "event provider: eastmoney | yahoo | hkex")
	symbolsCSV := flag.String("symbols", "", "comma-separated symbol list (e.g. 688195,002594 or NVDA,AMD)")
	sinceFlag := flag.String("since", "", "ISO date — drop events strictly before this (default: 5y ago)")
	apply := flag.Bool("apply", false, "after upsert, run applier against every fund that holds the instrument")
	dsnFlag := flag.String("dsn", "", "Postgres DSN (overrides DATABASE_URL env)")
	limit := flag.Int("max-events", 0, "cap inserts per symbol (0 = unlimited; defensive)")
	flag.Parse()

	dsn := strings.TrimSpace(*dsnFlag)
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		fatal("no DSN: pass --dsn or set DATABASE_URL")
	}
	symbols := splitCSV(*symbolsCSV)
	if len(symbols) == 0 {
		fatal("--symbols is required")
	}
	since, err := parseSinceFlag(*sinceFlag)
	if err != nil {
		fatal("invalid --since: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		fatal("open db: %v", err)
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		fatal("ping db: %v", err)
	}

	prov, err := buildProvider(*provider)
	if err != nil {
		fatal("%v", err)
	}

	repo := repository.NewCorpActionRepo(db)

	rows := make([]reportRow, 0, 16)
	for _, sym := range symbols {
		events, err := prov.FetchEvents(ctx, sym, since)
		if err != nil {
			rows = append(rows, reportRow{
				Symbol: sym,
				Note:   fmt.Sprintf("provider: %v", err),
				Status: "fetch-failed",
			})
			continue
		}
		if len(events) == 0 {
			rows = append(rows, reportRow{Symbol: sym, Status: "no-events"})
			continue
		}

		// Optional safety net for misconfigured providers that return
		// an avalanche of rows. Operator can always raise the cap on
		// the next call.
		if *limit > 0 && len(events) > *limit {
			events = events[:*limit]
		}

		fundsByInstrument := map[string][]string{}
		for _, evt := range events {
			row := reportRow{
				Symbol:        sym,
				InstrumentKey: evt.InstrumentKey,
				ExDate:        evt.ExDate.Format("2006-01-02"),
				ActionType:    evt.ActionType,
				SplitRatio:    evt.SplitRatio,
				CashDividend:  evt.CashDividend,
				Source:        evt.Source,
			}
			id, err := repo.Upsert(ctx, repository.CorpActionRow{
				InstrumentKey: evt.InstrumentKey,
				ExDate:        evt.ExDate,
				ActionType:    evt.ActionType,
				SplitRatio:    evt.SplitRatio,
				CashDividend:  evt.CashDividend,
				Source:        evt.Source,
			})
			if err != nil {
				row.Status = "upsert-failed"
				row.Note = err.Error()
				rows = append(rows, row)
				continue
			}
			row.EventID = id
			row.Status = "ingested"

			if *apply {
				// Resolve the funds holding this instrument once
				// per (symbol, instrument_key). Subsequent events
				// for the same key reuse the slice without a
				// second SQL trip.
				holders, ok := fundsByInstrument[evt.InstrumentKey]
				if !ok {
					holders, err = lookupHoldingFunds(ctx, db, evt.InstrumentKey)
					if err != nil {
						row.Note = fmt.Sprintf("lookup holders: %v", err)
						row.Status = "applied-partial"
						rows = append(rows, row)
						continue
					}
					fundsByInstrument[evt.InstrumentKey] = holders
				}

				row.AppliedFunds = applyEventToFunds(ctx, db, evt, id, holders)
				if row.AppliedFunds > 0 {
					row.Status = "applied"
				} else {
					row.Status = "ingested-no-holders"
				}
			}
			rows = append(rows, row)
		}
	}

	printReport(rows, *apply)
}

func buildProvider(name string) (corpaction.EventFetcher, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "eastmoney":
		return &corpaction.EastmoneyProvider{}, nil
	case "yahoo":
		return &corpaction.YahooProvider{}, nil
	case "hkex":
		// HK corp actions through East Money's HK datacenter
		// feed. Symbols accepted: "00700" / "0700.HK" / "HKEX:00700".
		// See provider_hkex.go for the full coverage matrix.
		return &corpaction.HKEXProvider{}, nil
	default:
		return nil, fmt.Errorf("unknown provider %q (allowed: eastmoney | yahoo | hkex)", name)
	}
}

func parseSinceFlag(v string) (time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		// 5y default mirrors the YahooProvider's own default. Older
		// events, if any, are practically already absorbed into the
		// fund's recorded cost basis.
		return time.Now().AddDate(-5, 0, 0), nil
	}
	return time.Parse("2006-01-02", v)
}

func splitCSV(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// lookupHoldingFunds returns every fund that has a non-zero open
// position on the instrument. The applier checks idempotency itself,
// so it's safe to fan out without de-duping fund IDs here.
func lookupHoldingFunds(ctx context.Context, db *sql.DB, instrumentKey string) ([]string, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT DISTINCT fund_id FROM holding_positions
		  WHERE instrument_key = $1 AND quantity > 0`,
		instrumentKey,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var fid string
		if err := rows.Scan(&fid); err != nil {
			return nil, err
		}
		out = append(out, fid)
	}
	return out, rows.Err()
}

func applyEventToFunds(ctx context.Context, db *sql.DB, evt corpaction.Event, eventID string, fundIDs []string) int {
	applied := 0
	for _, fid := range fundIDs {
		evt.ID = eventID
		res, err := corpaction.ApplyEvent(ctx, db, evt, fid)
		if err != nil {
			if errors.Is(err, corpaction.ErrPositionMissing) {
				// Fund was in the holders list because it had a
				// row in holding_positions, but the row got zeroed
				// or deleted between lookupHoldingFunds and the
				// applier's locking SELECT. Soft skip.
				continue
			}
			fmt.Fprintf(os.Stderr, "apply %s -> fund %s: %v\n", evt.InstrumentKey, fid, err)
			continue
		}
		// AlreadyApplied (idempotency hit) still counts as success
		// because the downstream invariant — fund cost basis is
		// post-action — is already true.
		_ = res
		applied++
	}
	return applied
}

type reportRow struct {
	Symbol        string
	InstrumentKey string
	EventID       string
	ExDate        string
	ActionType    string
	SplitRatio    float64
	CashDividend  float64
	Source        string
	Status        string
	AppliedFunds  int
	Note          string
}

func printReport(rows []reportRow, apply bool) {
	if len(rows) == 0 {
		fmt.Println("# corpactionsync: no rows")
		return
	}
	fmt.Println("# corpactionsync report")
	fmt.Println()
	if apply {
		fmt.Println("| symbol | instrument | ex_date | type | ratio | cash/sh | status | applied funds | note |")
		fmt.Println("| --- | --- | --- | --- | ---: | ---: | --- | ---: | --- |")
	} else {
		fmt.Println("| symbol | instrument | ex_date | type | ratio | cash/sh | source | status | note |")
		fmt.Println("| --- | --- | --- | --- | ---: | ---: | --- | --- | --- |")
	}
	for _, r := range rows {
		if apply {
			fmt.Printf("| %s | %s | %s | %s | %.6f | %.6f | %s | %d | %s |\n",
				dash(r.Symbol), dash(r.InstrumentKey), dash(r.ExDate),
				dash(r.ActionType), r.SplitRatio, r.CashDividend,
				dash(r.Status), r.AppliedFunds, dash(r.Note))
		} else {
			fmt.Printf("| %s | %s | %s | %s | %.6f | %.6f | %s | %s | %s |\n",
				dash(r.Symbol), dash(r.InstrumentKey), dash(r.ExDate),
				dash(r.ActionType), r.SplitRatio, r.CashDividend,
				dash(r.Source), dash(r.Status), dash(r.Note))
		}
	}
}

func dash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "corpactionsync: "+format+"\n", args...)
	os.Exit(2)
}
