package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestPercentileIsNearestRank(t *testing.T) {
	t.Parallel()
	sorted := make([]time.Duration, 100)
	for i := range sorted {
		sorted[i] = time.Duration(i+1) * time.Millisecond
	}
	for p, want := range map[float64]time.Duration{50: 50 * time.Millisecond, 95: 95 * time.Millisecond, 99: 99 * time.Millisecond, 100: 100 * time.Millisecond, 0: time.Millisecond} {
		if got := percentile(sorted, p); got != want {
			t.Errorf("p%v = %v, want %v", p, got, want)
		}
	}
	if got := percentile(nil, 50); got != 0 {
		t.Errorf("percentile of nothing = %v", got)
	}
}

func TestMedianOfRuns(t *testing.T) {
	t.Parallel()
	got := medianOf([]stats{{RequestsPerSecond: 10, P99Ms: 1}, {RequestsPerSecond: 30, P99Ms: 9}, {RequestsPerSecond: 20, P99Ms: 5}})
	if got.RequestsPerSecond != 20 || got.P99Ms != 5 {
		t.Fatalf("median = %+v", got)
	}
	even := medianOf([]stats{{RequestsPerSecond: 10}, {RequestsPerSecond: 20}})
	if even.RequestsPerSecond != 15 {
		t.Fatalf("even median = %+v", even)
	}
}

func TestReportMarkdownHasOneRowPerScenario(t *testing.T) {
	t.Parallel()
	run := stats{RequestsPerSecond: 1234.4, P50Ms: 0.5, P95Ms: 2, P99Ms: 12.34}
	var out bytes.Buffer
	cfg := config{concurrency: 16, duration: 10 * time.Second, warmup: 2 * time.Second, runs: 1, rows: 50, format: "markdown"}
	if err := report(&out, cfg, []result{{Scenario: scenarioHealth, Runs: []stats{run}, Median: run}}); err != nil {
		t.Fatal(err)
	}
	if want := "| `health` | 1234 | 1234-1234 | 0.50 | 2.00 | 12.3 | 0 |"; !strings.Contains(out.String(), want) {
		t.Fatalf("report lacks %q:\n%s", want, out.String())
	}
}

func TestValidateRejectsBadSettings(t *testing.T) {
	t.Parallel()
	valid := config{concurrency: 1, runs: 1, duration: time.Second, rows: 50, password: "x", format: "markdown", scenarios: allScenarios}
	if err := valid.validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*config){
		"no password":      func(c *config) { c.password = "" },
		"unknown scenario": func(c *config) { c.scenarios = []string{"nope"} },
		"too many rows":    func(c *config) { c.rows = 201 },
		"no workers":       func(c *config) { c.concurrency = 0 },
		"format":           func(c *config) { c.format = "csv" },
	} {
		c := valid
		mutate(&c)
		if err := c.validate(); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}
