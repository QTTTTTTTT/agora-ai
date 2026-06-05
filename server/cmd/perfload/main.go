// Command perfload is a small in-tree HTTP load generator used to
// produce active-traffic perf baselines (latency percentiles, QPS,
// error rate) without taking on a third-party dep like wrk / hey /
// k6. Designed for the perf-load-baseline.sh driver but useful
// standalone too:
//
//	go run ./cmd/perfload -url http://localhost:8080/api/health -duration 30s -concurrency 50
//
// Outputs a human-readable summary on stdout and, when -json is set,
// a machine-parseable single-line JSON object suitable for piping
// into tooling. Cancellation: SIGINT / SIGTERM stop the run early
// and still print the partial summary so an aborted load test
// still gives you the numbers you accumulated up to that point.
//
// What this is NOT: a fully-featured benchmark tool. There's no
// keep-alive control, no warm-up phase, no constant-arrival-rate
// mode, no scriptable scenarios. It's deliberately ~150 lines of
// Go so anyone reading the perf playbook can audit the methodology
// in 5 minutes. For real cross-load calibration (the "H5" item
// from the production-readiness review) we should still graduate
// to k6 or vegeta — this tool exists so that we have *some*
// reproducible baseline today instead of waiting for that.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

func main() {
	url := flag.String("url", "http://localhost:8080/api/health", "target URL")
	duration := flag.Duration("duration", 30*time.Second, "test duration")
	concurrency := flag.Int("concurrency", 50, "number of concurrent workers")
	timeout := flag.Duration("timeout", 5*time.Second, "per-request timeout")
	method := flag.String("method", "GET", "HTTP method")
	asJSON := flag.Bool("json", false, "emit a single JSON line instead of human summary")
	warmup := flag.Duration("warmup", 2*time.Second, "warmup window — its requests are dropped from the percentile sample")
	flag.Parse()

	if *concurrency <= 0 {
		fmt.Fprintln(os.Stderr, "concurrency must be > 0")
		os.Exit(2)
	}
	if *duration <= 0 {
		fmt.Fprintln(os.Stderr, "duration must be > 0")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *duration+*warmup)
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	client := &http.Client{
		Timeout: *timeout,
		Transport: &http.Transport{
			MaxIdleConns:        *concurrency * 2,
			MaxIdleConnsPerHost: *concurrency * 2,
			MaxConnsPerHost:     *concurrency * 2,
			IdleConnTimeout:     30 * time.Second,
		},
	}

	type sample struct {
		latencyNs int64
		status    int
		err       bool
	}
	samples := make(chan sample, *concurrency*4)

	var (
		totalSent atomic.Int64
		totalDone atomic.Int64
		totalErr  atomic.Int64
		total5xx  atomic.Int64
	)
	cutover := time.Now().Add(*warmup)

	var wg sync.WaitGroup
	for i := 0; i < *concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if ctx.Err() != nil {
					return
				}
				totalSent.Add(1)
				start := time.Now()
				req, err := http.NewRequestWithContext(ctx, *method, *url, nil)
				if err != nil {
					totalErr.Add(1)
					if errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
						return
					}
					continue
				}
				resp, err := client.Do(req)
				lat := time.Since(start).Nanoseconds()
				if err != nil {
					totalErr.Add(1)
					if errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
						return
					}
					continue
				}
				_ = resp.Body.Close()
				totalDone.Add(1)
				if resp.StatusCode >= 500 {
					total5xx.Add(1)
				}
				if start.After(cutover) {
					select {
					case samples <- sample{latencyNs: lat, status: resp.StatusCode}:
					default:
					}
				}
			}
		}()
	}

	doneCh := make(chan struct{})
	go func() {
		wg.Wait()
		close(samples)
		close(doneCh)
	}()
	<-doneCh

	latencies := make([]int64, 0, 1024)
	for s := range samples {
		latencies = append(latencies, s.latencyNs)
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })

	pctl := func(p float64) float64 {
		if len(latencies) == 0 {
			return 0
		}
		idx := int(float64(len(latencies)-1) * p)
		return float64(latencies[idx]) / 1e6
	}

	var avgMs float64
	if len(latencies) > 0 {
		var sum int64
		for _, v := range latencies {
			sum += v
		}
		avgMs = float64(sum) / float64(len(latencies)) / 1e6
	}

	measured := *duration
	totalReqs := totalDone.Load()
	qps := float64(totalReqs) / measured.Seconds()
	errRate := 0.0
	if totalSent.Load() > 0 {
		errRate = float64(totalErr.Load()) / float64(totalSent.Load())
	}
	fiveXXRate := 0.0
	if totalReqs > 0 {
		fiveXXRate = float64(total5xx.Load()) / float64(totalReqs)
	}

	if *asJSON {
		out := map[string]any{
			"url":          *url,
			"duration_s":   measured.Seconds(),
			"concurrency":  *concurrency,
			"sent":         totalSent.Load(),
			"done":         totalReqs,
			"errors":       totalErr.Load(),
			"five_xx":      total5xx.Load(),
			"qps":          qps,
			"avg_ms":       avgMs,
			"p50_ms":       pctl(0.50),
			"p95_ms":       pctl(0.95),
			"p99_ms":       pctl(0.99),
			"err_rate":     errRate,
			"five_xx_rate": fiveXXRate,
		}
		_ = json.NewEncoder(os.Stdout).Encode(out)
		return
	}

	fmt.Printf("perfload — %s\n", *url)
	fmt.Printf("  duration       %s (warmup %s, dropped from percentiles)\n", *duration, *warmup)
	fmt.Printf("  concurrency    %d\n", *concurrency)
	fmt.Printf("  requests sent  %d\n", totalSent.Load())
	fmt.Printf("  requests done  %d\n", totalReqs)
	fmt.Printf("  errors         %d  (%.4f rate)\n", totalErr.Load(), errRate)
	fmt.Printf("  5xx            %d  (%.4f rate)\n", total5xx.Load(), fiveXXRate)
	fmt.Printf("  qps            %.1f\n", qps)
	fmt.Printf("  latency        avg=%.1fms  p50=%.1f  p95=%.1f  p99=%.1f\n", avgMs, pctl(0.50), pctl(0.95), pctl(0.99))
	if total5xx.Load() > 0 {
		fmt.Fprintln(os.Stderr, "WARN: server returned 5xx — see /api/health and server logs")
	}
}
