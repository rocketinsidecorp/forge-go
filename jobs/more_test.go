package jobs

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestParseCronRejectsMalformedExpressions(t *testing.T) {
	t.Parallel()
	for expression, want := range map[string]string{
		strings.Repeat("*", 257): "too long",
		"* * * *":                "5 numeric fields",
		"* * * * * *":            "5 numeric fields",
		"x * * * *":              "minute",
		"60 * * * *":             "minute",
		"* 24 * * *":             "hour",
		"* x * * *":              "hour",
		"* * 0 * *":              "day-of-month",
		"* * 32 * *":             "day-of-month",
		"* * * 13 *":             "month",
		"* * * jan *":            "month",
		"* * * * 8":              "day-of-week",
		"1,,2 * * * *":           "empty list item",
		"*/0 * * * *":            "step must be a positive integer",
		"*/x * * * *":            "step must be a positive integer",
		"1-x * * * *":            "range endpoints",
		"5-1 * * * *":            "ascending range",
		"*/2/3 * * * *":          "step must be a positive integer",
		"1** * * * *":            "invalid field syntax",
		"a * * * *":              "minute",
		"0 0 31 2 *":             "",
	} {
		_, err := parseCron(expression)
		if err == nil || (want != "" && !strings.Contains(err.Error(), want)) {
			t.Errorf("parseCron(%.30q) = %v, want an error mentioning %q", expression, err, want)
		}
	}
}

func TestCronDayFieldsFollowTheClassicRules(t *testing.T) {
	t.Parallel()
	// 2026-10-05 is a Monday; the 15th is a Thursday; the 4th a Sunday.
	day := func(d int) time.Time { return time.Date(2026, time.October, d, 0, 0, 0, 0, time.UTC) }
	cases := []struct {
		expression string
		matches    map[int]bool
	}{
		{"0 0 * * *", map[int]bool{5: true, 15: true}},
		{"0 0 15 * *", map[int]bool{5: false, 15: true}},
		{"0 0 * * 1", map[int]bool{5: true, 15: false}},
		// Both restricted: either may match.
		{"0 0 15 * 1", map[int]bool{5: true, 15: true, 6: false}},
		{"0 0 * * 0", map[int]bool{4: true, 5: false}},
		{"0 0 * * 7", map[int]bool{4: true, 5: false}},
		{"0 0 */10 * *", map[int]bool{1: true, 11: true, 21: true, 5: false}},
		{"0 0 5/10 * *", map[int]bool{5: true, 15: true, 25: true, 6: false}},
		{"0 0 1-10/3 * *", map[int]bool{1: true, 4: true, 7: true, 10: true, 2: false}},
		{"0 0 1,15 * *", map[int]bool{1: true, 15: true, 2: false}},
		{"0 0 * 11 *", map[int]bool{5: false}},
	}
	for _, tc := range cases {
		schedule, err := parseCron(tc.expression)
		if err != nil {
			t.Fatalf("%s: %v", tc.expression, err)
		}
		for d, want := range tc.matches {
			if got := schedule.matchesDate(day(d)); got != want {
				t.Errorf("%s on Oct %d: matches = %v, want %v", tc.expression, d, got, want)
			}
		}
	}
	// nextRun lands on the next matching day at the scheduled time.
	schedule, _ := parseCron("30 6 15 * *")
	next, err := schedule.nextRun(time.Date(2026, time.October, 15, 6, 30, 0, 0, time.UTC))
	if err != nil || !next.Equal(time.Date(2026, time.November, 15, 6, 30, 0, 0, time.UTC)) {
		t.Errorf("next = %v, %v", next, err)
	}
}

func TestScheduleSpecValidateAndNextRun(t *testing.T) {
	t.Parallel()
	for name, spec := range map[string]ScheduleSpec{
		"nothing":        {},
		"two":            {Every: time.Minute, Cron: "* * * * *"},
		"hour too big":   {DailyAt: &DailyTime{Hour: 24}},
		"minute too big": {DailyAt: &DailyTime{Minute: 60}},
		"negative":       {DailyAt: &DailyTime{Hour: -1}},
		"too fast":       {Every: time.Millisecond},
		"bad cron":       {Cron: "nope"},
	} {
		if err := spec.Validate(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, err := (ScheduleSpec{}).nextRun(time.Now()); err == nil {
		t.Error("an empty spec has no next run")
	}
	after := time.Date(2026, time.October, 5, 12, 0, 0, 0, time.UTC)
	next, err := ScheduleSpec{DailyAt: &DailyTime{Hour: 12}}.nextRun(after)
	if err != nil || !next.Equal(after.Add(24*time.Hour)) {
		t.Errorf("daily at the current minute must wait a day: %v, %v", next, err)
	}
	for spec, want := range map[ScheduleSpec]time.Duration{
		{Every: time.Second}:    time.Second,
		{Every: time.Hour}:      5 * time.Minute,
		{DailyAt: &DailyTime{}}: 5 * time.Minute,
		{Cron: "* * * * *"}:     10 * time.Minute,
	} {
		if got := jobTimeout(spec); got != want {
			t.Errorf("jobTimeout(%+v) = %v, want %v", spec, got, want)
		}
	}
}

func TestRegisterRejectsInvalidJobs(t *testing.T) {
	t.Parallel()
	s := NewScheduler(SchedulerOptions{MaxConcurrentJobs: 1000})
	if cap(s.concurrent) != 256 {
		t.Errorf("concurrency = %d, want it capped at 256", cap(s.concurrent))
	}
	ok := func(context.Context) error { return nil }
	every := ScheduleSpec{Every: time.Hour}
	if err := s.Register("", every, ok); err == nil {
		t.Error("an empty name was accepted")
	}
	if err := s.Register("bad", ScheduleSpec{}, ok); err == nil {
		t.Error("an empty schedule was accepted")
	}
	if err := s.Register("nofunc", every, nil); err == nil {
		t.Error("a nil job was accepted")
	}
	daily := &DailyTime{Hour: 3}
	if err := s.Register("daily", ScheduleSpec{DailyAt: daily}, ok); err != nil {
		t.Fatal(err)
	}
	daily.Hour = 99 // the scheduler keeps its own copy
	if s.jobs["daily"].spec.DailyAt.Hour != 3 {
		t.Error("the scheduler shares the caller's DailyAt")
	}
	if err := s.Register("daily", every, ok); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Errorf("duplicate: %v", err)
	}
	s.Start()
	if err := s.Register("late", every, ok); err == nil || !strings.Contains(err.Error(), "before the scheduler starts") {
		t.Errorf("after Start: %v", err)
	}
	if err := s.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	s.Start() // a stopped scheduler does not start again
}

// TestFailingJobWithoutAnOnErrorHandlerIsLogged replaces the process-wide
// default logger, so it must not run in parallel with anything that logs.
func TestFailingJobWithoutAnOnErrorHandlerIsLogged(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	s := NewScheduler(SchedulerOptions{})
	ran := make(chan struct{}, 1)
	if err := s.Register("flaky", ScheduleSpec{Every: time.Second}, func(context.Context) error {
		select {
		case ran <- struct{}{}:
		default:
		}
		return errors.New("flaked")
	}); err != nil {
		t.Fatal(err)
	}
	s.Start()
	select {
	case <-ran:
	case <-time.After(10 * time.Second):
		t.Fatal("the job never ran")
	}
	if err := s.Stop(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "scheduled job failed") || !strings.Contains(logs.String(), "flaky") {
		t.Errorf("log = %q", logs.String())
	}
}
