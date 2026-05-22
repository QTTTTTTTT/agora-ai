// Command preview_prompt is a one-shot operator tool that prints
// the LLM-facing prompt sections (SleeveScorecard + LessonReplay)
// for a given fund, using the exact pure builders the wiring
// layer calls in production. Use it to confirm that the
// attribution flywheel has produced the inputs PR-3A7 / PR-3A10
// expect before the next PM decision call.
//
// Usage:
//
//	go run ./cmd/preview_prompt -fund-id=<uuid>
//
// Reads DATABASE_URL from env. Prints two clearly-labelled
// blocks; either may be empty when the fund has no closed lots
// or no recent attribution memories.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	_ "github.com/lib/pq"

	"github.com/fundai/server/internal/attribution"
	"github.com/fundai/server/internal/repository"
)

func main() {
	fundID := flag.String("fund-id", "", "fund UUID to preview prompt sections for")
	flag.Parse()
	if strings.TrimSpace(*fundID) == "" {
		log.Fatal("missing -fund-id")
	}

	dsn := firstNonEmpty(os.Getenv("DATABASE_URL"), os.Getenv("APP_DATABASE_URL"))
	if dsn == "" {
		log.Fatal("DATABASE_URL or APP_DATABASE_URL must be set")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()
	if err := db.PingContext(context.Background()); err != nil {
		log.Fatalf("ping db: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// ---------- SleeveScorecard (PR-3A7) ----------
	lotRepo := repository.NewLotRepo(db)
	memRepo := repository.NewMemoryRepo(db)
	svc := attribution.NewService(lotRepo, memRepo)
	report, err := svc.BuildReport(ctx, *fundID, attribution.DefaultLookbackDays)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Fatalf("build attribution report: %v", err)
	}
	var scorecard attribution.PromptScorecard
	if report != nil {
		scorecard = attribution.BuildPromptScorecard(*report, attribution.PromptScorecardOptions{})
	}

	// ---------- LessonReplay (PR-3A10) ----------
	mems, err := memRepo.ListByFund(ctx, *fundID, attribution.MemoryLayer, 50)
	if err != nil {
		log.Fatalf("list attribution memories: %v", err)
	}
	replay := attribution.BuildLessonReplay(mems, time.Now().UTC(), attribution.LessonReplayOptions{})

	// ---------- Render ----------
	fmt.Println("================== PROMPT PREVIEW ==================")
	fmt.Printf("fund_id: %s\n", *fundID)
	fmt.Printf("window: %d days\n\n", attribution.DefaultLookbackDays)

	fmt.Println("--- sleeveScorecard (PR-3A7) ---")
	if strings.TrimSpace(scorecard.Summary) == "" {
		fmt.Println("(empty — no rows met the sample-size floor)")
	} else {
		fmt.Println(scorecard.Summary)
	}
	fmt.Printf("\nrows = %d, window = %q\n", len(scorecard.Rows), scorecard.Window)

	fmt.Println("\n--- lessonReplay (PR-3A10) ---")
	if strings.TrimSpace(replay.Summary) == "" {
		fmt.Println("(empty — no recent attribution memories survived dedup/lookback filter)")
	} else {
		fmt.Println(replay.Summary)
	}
	fmt.Printf("\nrows = %d, window = %q, memories pulled = %d\n",
		len(replay.Rows), replay.Window, len(mems))

	fmt.Println("====================================================")
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
