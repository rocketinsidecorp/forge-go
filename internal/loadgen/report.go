package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"
)

func notifyContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// result is one scenario over all its runs.
type result struct {
	Scenario string  `json:"scenario"`
	Runs     []stats `json:"runs"`
	Median   stats   `json:"median"`
	Errors   int     `json:"errors"`
}

// stats are the figures of one run, or the median of each figure over runs.
type stats struct {
	RequestsPerSecond float64 `json:"requests_per_second"`
	P50Ms             float64 `json:"p50_ms"`
	P95Ms             float64 `json:"p95_ms"`
	P99Ms             float64 `json:"p99_ms"`
}

func millis(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func statsOf(s sample) stats {
	return stats{
		RequestsPerSecond: float64(s.ok) / s.elapsed.Seconds(),
		P50Ms:             millis(percentile(s.latencies, 50)),
		P95Ms:             millis(percentile(s.latencies, 95)),
		P99Ms:             millis(percentile(s.latencies, 99)),
	}
}

func median(values []float64) float64 {
	sorted := slices.Sorted(slices.Values(values))
	n := len(sorted)
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

func medianOf(runs []stats) stats {
	column := func(pick func(stats) float64) float64 {
		values := make([]float64, len(runs))
		for i, run := range runs {
			values[i] = pick(run)
		}
		return median(values)
	}
	return stats{
		RequestsPerSecond: column(func(s stats) float64 { return s.RequestsPerSecond }),
		P50Ms:             column(func(s stats) float64 { return s.P50Ms }),
		P95Ms:             column(func(s stats) float64 { return s.P95Ms }),
		P99Ms:             column(func(s stats) float64 { return s.P99Ms }),
	}
}

// measure warms every scenario up, then measures cfg.runs rounds of all of
// them, one window each, in a fixed order.
func measure(ctx context.Context, cfg config, t *target, progress io.Writer) ([]result, error) {
	if cfg.warmup > 0 {
		for _, name := range cfg.scenarios {
			fmt.Fprintf(progress, "warm-up %s (%s)\n", name, cfg.warmup)
			t.drive(ctx, t.scenarios[name], cfg.concurrency, cfg.warmup)
		}
	}
	results := make([]result, len(cfg.scenarios))
	for i, name := range cfg.scenarios {
		results[i].Scenario = name
	}
	for round := 1; round <= cfg.runs; round++ {
		for i, name := range cfg.scenarios {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			fmt.Fprintf(progress, "run %d/%d %s (%s)\n", round, cfg.runs, name, cfg.duration)
			s := t.drive(ctx, t.scenarios[name], cfg.concurrency, cfg.duration)
			if s.ok == 0 {
				return nil, fmt.Errorf("%s: no request succeeded (%d errors)", name, s.errors)
			}
			results[i].Runs = append(results[i].Runs, statsOf(s))
			results[i].Errors += s.errors
		}
	}
	for i := range results {
		results[i].Median = medianOf(results[i].Runs)
	}
	return results, nil
}

// report writes the results in the format shared by Forge-Go and Forge-Rust.
func report(w io.Writer, cfg config, results []result) error {
	if cfg.format == "json" {
		encoder := json.NewEncoder(w)
		encoder.SetIndent("", "  ")
		return encoder.Encode(map[string]any{
			"concurrency":      cfg.concurrency,
			"duration_seconds": cfg.duration.Seconds(),
			"warmup_seconds":   cfg.warmup.Seconds(),
			"runs":             cfg.runs,
			"index_rows":       cfg.rows,
			"results":          results,
		})
	}
	fmt.Fprintf(w, "concurrency %d, %s per run, %s warm-up, %d runs, index of %d rows; figures are medians over the runs\n\n",
		cfg.concurrency, cfg.duration, cfg.warmup, cfg.runs, cfg.rows)
	fmt.Fprintln(w, "| Scenario | Requests/s | Requests/s (min-max) | p50 ms | p95 ms | p99 ms | Errors |")
	fmt.Fprintln(w, "| --- | ---: | ---: | ---: | ---: | ---: | ---: |")
	for _, r := range results {
		lo, hi := r.Runs[0].RequestsPerSecond, r.Runs[0].RequestsPerSecond
		for _, run := range r.Runs {
			lo, hi = min(lo, run.RequestsPerSecond), max(hi, run.RequestsPerSecond)
		}
		fmt.Fprintf(w, "| `%s` | %.0f | %.0f-%.0f | %s | %s | %s | %d |\n", r.Scenario,
			r.Median.RequestsPerSecond, lo, hi, ms(r.Median.P50Ms), ms(r.Median.P95Ms), ms(r.Median.P99Ms), r.Errors)
	}
	return nil
}

func ms(v float64) string {
	if v < 10 {
		return strings.TrimSpace(fmt.Sprintf("%.2f", v))
	}
	return fmt.Sprintf("%.1f", v)
}
