// factorlab is the G1 #3 backtest harness CLI. Given a fixture
// (synthetic by default, or a real-CSV directory), it runs the
// MVP factor strategies and prints a markdown comparison table.
//
// Usage:
//
//	# Run the synthetic fixture with all MVP strategies.
//	go run ./cmd/factorlab/ > report.md
//
//	# Run against a frozen real-OHLC CSV directory.
//	go run ./cmd/factorlab/ --fixture ./testdata/us_largecap_2024
//
//	# Tune slippage / start NAV.
//	go run ./cmd/factorlab/ --slippage 10 --nav 100000
//
//	# Pick a subset of strategies.
//	go run ./cmd/factorlab/ --strategies momentum_12_1m,low_beta
//
// Output: markdown to stdout. Pipe to `tee /tmp/report.md` (or
// redirect) for review. The fixture's window, the strategies'
// per-day rebalance behaviour, and the headline metrics are
// fully reproducible given the same seed + fixture.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/fundai/server/internal/factorlab"
)

func main() {
	fixturePath := flag.String("fixture", "", "directory of per-symbol CSV files (empty = synthetic fixture)")
	slippageBps := flag.Float64("slippage", 5.0, "one-sided slippage charged on turnover, in bps")
	startNav := flag.Float64("nav", 1.0, "start NAV (unit-free by default)")
	strategiesCSV := flag.String("strategies", "", "comma-separated subset (empty = run all MVP strategies)")
	seed := flag.Int64("seed", 42, "PRNG seed for the synthetic fixture (ignored when --fixture is set)")
	flag.Parse()

	fixture, err := loadFixture(*fixturePath, *seed)
	if err != nil {
		fmt.Fprintf(os.Stderr, "factorlab: %v\n", err)
		os.Exit(2)
	}

	strats := pickStrategies(*strategiesCSV)
	if len(strats) == 0 {
		fmt.Fprintln(os.Stderr, "factorlab: no strategies selected (check --strategies)")
		os.Exit(2)
	}

	sim := &factorlab.Simulator{
		StartNav:    *startNav,
		SlippageBps: *slippageBps,
	}
	results := sim.Run(fixture, strats)
	if len(results) == 0 {
		fmt.Fprintln(os.Stderr, "factorlab: simulator returned no results (fixture too short?)")
		os.Exit(2)
	}

	fmt.Print(factorlab.RenderMarkdown(results))
}

func loadFixture(path string, seed int64) (*factorlab.Fixture, error) {
	if strings.TrimSpace(path) == "" {
		return factorlab.BuildSynthFixture(factorlab.SynthOptions{Seed: seed}), nil
	}
	return factorlab.LoadFixture(path)
}

func pickStrategies(csv string) []factorlab.Strategy {
	all := []factorlab.Strategy{
		factorlab.EqualWeightLong{},
		factorlab.Momentum12_1M{},
		factorlab.LowBeta{},
		factorlab.LowVol{},
	}
	csv = strings.TrimSpace(csv)
	if csv == "" {
		return all
	}
	want := make(map[string]bool)
	for _, s := range strings.Split(csv, ",") {
		want[strings.ToLower(strings.TrimSpace(s))] = true
	}
	out := make([]factorlab.Strategy, 0, len(want))
	for _, s := range all {
		if want[s.Name()] {
			out = append(out, s)
		}
	}
	return out
}
