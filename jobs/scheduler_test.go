package jobs

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// The scheduler tests run in a synctest bubble: its clock starts at
// 2000-01-01T00:00:00Z, a multiple of every interval used here, and only
// advances when every goroutine in the bubble is blocked, so the runs below
// happen at exact instants without real sleeps.

func TestSchedulerRunsJobsOnInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var counter atomic.Int32
		s := NewScheduler(SchedulerOptions{MaxConcurrentJobs: 2})
		err := s.Register("counter", ScheduleSpec{Every: 1500 * time.Millisecond}, func(ctx context.Context) error {
			counter.Add(1)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		s.Start()
		// Ticks at 1.5 s, 3 s and 4.5 s.
		// fake-clock: advances the synctest bubble's clock past the ticks under test.
		time.Sleep(4600 * time.Millisecond)
		synctest.Wait()
		if err := s.Stop(); err != nil {
			t.Fatalf("Stop error: %v", err)
		}
		if got := counter.Load(); got != 3 {
			t.Fatalf("runs = %d, want 3", got)
		}
	})
}

func TestScheduleSpecNextDailyAt(t *testing.T) {
	at := DailyTime{Hour: 12, Minute: 30}
	spec := ScheduleSpec{DailyAt: &at}
	tests := []struct {
		name  string
		after time.Time
		want  time.Time
	}{
		{
			name:  "later today",
			after: time.Date(2026, time.October, 4, 12, 0, 0, 0, time.UTC),
			want:  time.Date(2026, time.October, 4, 12, 30, 0, 0, time.UTC),
		},
		{
			name:  "tomorrow after today's run",
			after: time.Date(2026, time.October, 4, 12, 30, 0, 0, time.UTC),
			want:  time.Date(2026, time.October, 5, 12, 30, 0, 0, time.UTC),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := spec.nextRun(test.after)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Equal(test.want) {
				t.Fatalf("next run = %s, want %s", got, test.want)
			}
		})
	}
}

func TestSchedulerValidatesSchedule(t *testing.T) {
	s := NewScheduler(SchedulerOptions{})
	// No schedule
	if err := s.Register("bad", ScheduleSpec{}, func(context.Context) error { return nil }); err == nil {
		t.Fatal("expected error for empty schedule")
	}
	// Multiple schedules
	if err := s.Register("bad", ScheduleSpec{Every: time.Second, DailyAt: &DailyTime{Hour: 1}}, func(context.Context) error { return nil }); err == nil {
		t.Fatal("expected error for multiple schedules")
	}
	// Invalid Every
	if err := s.Register("bad", ScheduleSpec{Every: 100 * time.Millisecond}, func(context.Context) error { return nil }); err == nil {
		t.Fatal("expected error for Every < 1 second")
	}
	// Valid Every
	if err := s.Register("good", ScheduleSpec{Every: time.Second}, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("unexpected error for valid schedule: %v", err)
	}
	if err := s.Register("cron", ScheduleSpec{Cron: "*/15 9-17 * * 1-5"}, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("unexpected error for valid cron schedule: %v", err)
	}
	for _, expression := range []string{"", "* * *", "60 * * * *", "*/0 * * * *", "1-4-6 * * * *", "0 0 31 2 *"} {
		if err := s.Register("invalid-cron-"+expression, ScheduleSpec{Cron: expression}, func(context.Context) error { return nil }); err == nil {
			t.Errorf("accepted invalid cron expression %q", expression)
		}
	}
}

func TestSchedulerConcurrencyBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var running atomic.Int32
		var maxRunning atomic.Int32
		s := NewScheduler(SchedulerOptions{MaxConcurrentJobs: 2})
		for _, name := range []string{"slow-1", "slow-2", "slow-3"} {
			err := s.Register(name, ScheduleSpec{Every: 1500 * time.Millisecond}, func(ctx context.Context) error {
				current := running.Add(1)
				defer running.Add(-1)
				for previous := maxRunning.Load(); current > previous; previous = maxRunning.Load() {
					if maxRunning.CompareAndSwap(previous, current) {
						break
					}
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(1200 * time.Millisecond):
					return nil
				}
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		s.Start()
		// fake-clock: advances the synctest bubble's clock past the ticks under test.
		time.Sleep(5 * time.Second)
		if err := s.Stop(); err != nil {
			t.Fatalf("Stop error: %v", err)
		}
		if got := maxRunning.Load(); got != 2 {
			t.Fatalf("max running = %d, want the bound, 2", got)
		}
	})
}

func TestSchedulerEnforcesGlobalConcurrencyAndCanStartOnlyOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var running atomic.Int32
		var peak atomic.Int32
		var started atomic.Int32
		release := make(chan struct{})
		s := NewScheduler(SchedulerOptions{MaxConcurrentJobs: 2})
		for _, name := range []string{"one", "two", "three"} {
			if err := s.Register(name, ScheduleSpec{Every: 3 * time.Second}, func(ctx context.Context) error {
				current := running.Add(1)
				for previous := peak.Load(); current > previous; previous = peak.Load() {
					if peak.CompareAndSwap(previous, current) {
						break
					}
				}
				started.Add(1)
				select {
				case <-ctx.Done():
				case <-release:
				}
				running.Add(-1)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		s.Start()
		s.Start()
		if err := s.Register("late", ScheduleSpec{Every: time.Second}, func(context.Context) error { return nil }); err == nil {
			t.Fatal("registered a job after Start")
		}
		// All three are due at 3 s; two take the slots and the third waits.
		// fake-clock: advances the synctest bubble's clock past the ticks under test.
		time.Sleep(3 * time.Second)
		synctest.Wait()
		if got := started.Load(); got != 2 {
			t.Fatalf("started = %d at the first tick, want 2 (MaxConcurrentJobs)", got)
		}
		close(release)
		synctest.Wait()
		if got := started.Load(); got != 3 {
			t.Fatalf("started = %d once a slot was free, want 3", got)
		}
		if err := s.Stop(); err != nil {
			t.Fatal(err)
		}
		if got := peak.Load(); got != 2 {
			t.Fatalf("peak concurrency = %d, want 2", got)
		}
	})
}

func TestSchedulerReportsJobErrors(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		want := errors.New("job failed")
		errorsSeen := make(chan error, 8)
		s := NewScheduler(SchedulerOptions{
			MaxConcurrentJobs: 1,
			OnError:           func(_ string, err error) { errorsSeen <- err },
		})
		if err := s.Register("broken", ScheduleSpec{Every: time.Second}, func(context.Context) error { return want }); err != nil {
			t.Fatal(err)
		}
		s.Start()
		// fake-clock: advances the synctest bubble's clock past the ticks under test.
		time.Sleep(time.Second)
		synctest.Wait()
		select {
		case got := <-errorsSeen:
			if !errors.Is(got, want) {
				t.Fatalf("reported error = %v, want %v", got, want)
			}
		default:
			t.Fatal("job failure was not reported")
		}
		if err := s.Stop(); err != nil {
			t.Fatal(err)
		}
	})
}

// runRecorder records when a job ran and what OnError received, from inside
// a synctest bubble.
type runRecorder struct {
	mu      sync.Mutex
	runs    []time.Time
	skipped []*SkippedTicksError
	other   []error
}

func (r *runRecorder) ran() {
	r.mu.Lock()
	r.runs = append(r.runs, time.Now().UTC())
	r.mu.Unlock()
}

func (r *runRecorder) onError(_ string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var skipped *SkippedTicksError
	if errors.As(err, &skipped) {
		r.skipped = append(r.skipped, skipped)
		return
	}
	r.other = append(r.other, err)
}

// offsets returns the runs as offsets from the bubble's start.
func (r *runRecorder) offsets(start time.Time) []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	var offsets []time.Duration
	for _, run := range r.runs {
		offsets = append(offsets, run.Sub(start))
	}
	return offsets
}

// A run that stalls for ten intervals does not make the scheduler replay
// the ten ticks it missed back to back: the latest one runs once, the nine
// before it are skipped and reported, and the schedule continues on time.
func TestSchedulerRunsAtMostOneCatchUpAfterAStall(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now().UTC()
		recorder := &runRecorder{}
		s := NewScheduler(SchedulerOptions{OnError: recorder.onError})
		var calls atomic.Int32
		if err := s.Register("report", ScheduleSpec{Every: time.Minute}, func(context.Context) error {
			recorder.ran()
			if calls.Add(1) == 1 {
				// Stalls past its timeout and ten more ticks (2m to 11m),
				// as a hung call that ignores its context would.
				// fake-clock: a stalled run, ten intervals long, in the synctest bubble.
				time.Sleep(10*time.Minute + 30*time.Second)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		s.Start()
		// fake-clock: advances the synctest bubble's clock past the ticks under test.
		time.Sleep(13*time.Minute + 10*time.Second)
		synctest.Wait()
		if err := s.Stop(); err != nil {
			t.Fatal(err)
		}
		// 1m, then the stall until 11m30s, one catch-up for the 11m tick,
		// then 12m and 13m on time.
		want := []time.Duration{time.Minute, 11*time.Minute + 30*time.Second, 12 * time.Minute, 13 * time.Minute}
		if got := recorder.offsets(start); !slices.Equal(got, want) {
			t.Fatalf("runs at %v, want %v", got, want)
		}
		if len(recorder.skipped) != 1 || recorder.skipped[0].Skipped != 9 || recorder.skipped[0].Behind != 9*time.Minute+30*time.Second {
			t.Fatalf("skipped reports = %+v, want one of 9 ticks, 9m30s behind", recorder.skipped)
		}
		if len(recorder.other) != 0 {
			t.Fatalf("other errors = %v", recorder.other)
		}
	})
}

// With MaxLateness below the interval, a missed tick that is already too
// late is skipped too, and the next run comes from the current time.
func TestSchedulerSkipsATickLaterThanMaxLateness(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now().UTC()
		recorder := &runRecorder{}
		s := NewScheduler(SchedulerOptions{OnError: recorder.onError, MaxLateness: minMaxLateness})
		var calls atomic.Int32
		if err := s.Register("report", ScheduleSpec{Every: 5 * time.Minute}, func(context.Context) error {
			recorder.ran()
			if calls.Add(1) == 1 {
				// fake-clock: a run that outlasts a tick, until 11m40s: the 10m tick is 100s late.
				time.Sleep(400 * time.Second)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		s.Start()
		// fake-clock: advances the synctest bubble's clock past the ticks under test.
		time.Sleep(15*time.Minute + 10*time.Second)
		synctest.Wait()
		if err := s.Stop(); err != nil {
			t.Fatal(err)
		}
		if got, want := recorder.offsets(start), []time.Duration{5 * time.Minute, 15 * time.Minute}; !slices.Equal(got, want) {
			t.Fatalf("runs at %v, want %v", got, want)
		}
		if len(recorder.skipped) != 1 || recorder.skipped[0].Skipped != 1 || recorder.skipped[0].Behind != 100*time.Second {
			t.Fatalf("skipped reports = %+v, want one of 1 tick, 100s behind", recorder.skipped)
		}
	})
}

// An explicit MaxLateness below a minute is raised to a minute, as in
// Forge-Rust (Schedule::max_lateness): a tick 30s late still runs, where a
// 10s bound would have skipped it, and one 61s late is skipped.
func TestSchedulerRaisesAnExplicitMaxLatenessToAMinute(t *testing.T) {
	t.Parallel()
	for _, explicit := range []time.Duration{time.Nanosecond, time.Second, 10 * time.Second, 59 * time.Second} {
		if got := NewScheduler(SchedulerOptions{MaxLateness: explicit}).opts.MaxLateness; got != minMaxLateness {
			t.Errorf("MaxLateness %s: %s, want %s", explicit, got, minMaxLateness)
		}
	}
	for _, kept := range []time.Duration{0, minMaxLateness, 90 * time.Second, time.Hour} {
		if got := NewScheduler(SchedulerOptions{MaxLateness: kept}).opts.MaxLateness; got != kept {
			t.Errorf("MaxLateness %s changed to %s", kept, got)
		}
	}
	// The default stays one interval, even below a minute.
	s := NewScheduler(SchedulerOptions{})
	if got := s.maxLateness(scheduledJob{spec: ScheduleSpec{Every: 10 * time.Second}}, time.Time{}); got != 10*time.Second {
		t.Errorf("default lateness of Every 10s = %s, want 10s", got)
	}
	synctest.Test(t, func(t *testing.T) {
		start := time.Now().UTC()
		recorder := &runRecorder{}
		s := NewScheduler(SchedulerOptions{OnError: recorder.onError, MaxLateness: 10 * time.Second})
		var calls atomic.Int32
		if err := s.Register("report", ScheduleSpec{Every: time.Minute}, func(context.Context) error {
			recorder.ran()
			if calls.Add(1) == 1 {
				// fake-clock: a run that outlasts a tick, until 2m30s: the 2m tick is 30s late.
				time.Sleep(90 * time.Second)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		s.Start()
		// fake-clock: advances the synctest bubble's clock past the ticks under test.
		time.Sleep(3*time.Minute + 10*time.Second)
		synctest.Wait()
		if err := s.Stop(); err != nil {
			t.Fatal(err)
		}
		want := []time.Duration{time.Minute, 2*time.Minute + 30*time.Second, 3 * time.Minute}
		if got := recorder.offsets(start); !slices.Equal(got, want) {
			t.Fatalf("runs at %v, want %v: a tick 30s late must run under a 10s bound raised to a minute", got, want)
		}
		if len(recorder.skipped) != 0 {
			t.Fatalf("skipped reports = %+v, want none", recorder.skipped)
		}
	})
}

// A tick that became too late while it waited for a concurrency slot is
// skipped rather than run late; the job's later ticks run on time.
func TestSchedulerSkipsATickThatWaitedTooLongForASlot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now().UTC()
		hog, victim := &runRecorder{}, &runRecorder{}
		s := NewScheduler(SchedulerOptions{
			MaxConcurrentJobs: 1,
			MaxLateness:       minMaxLateness,
			OnError: func(name string, err error) {
				if name == "victim" {
					victim.onError(name, err)
				} else {
					hog.onError(name, err)
				}
			},
		})
		var hogCalls atomic.Int32
		if err := s.Register("hog", ScheduleSpec{Every: 10 * time.Minute}, func(context.Context) error {
			hog.ran()
			if hogCalls.Add(1) == 1 {
				// fake-clock: a run that holds the only slot from 10m to 16m10s.
				time.Sleep(370 * time.Second)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		// Every 15m: due at 15m, while the hog holds the slot.
		if err := s.Register("victim", ScheduleSpec{Every: 15 * time.Minute}, func(context.Context) error {
			victim.ran()
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		s.Start()
		// fake-clock: advances the synctest bubble's clock past the ticks under test.
		time.Sleep(30*time.Minute + 10*time.Second)
		synctest.Wait()
		if err := s.Stop(); err != nil {
			t.Fatal(err)
		}
		if got, want := victim.offsets(start), []time.Duration{30 * time.Minute}; !slices.Equal(got, want) {
			t.Fatalf("victim runs at %v, want %v", got, want)
		}
		if len(victim.skipped) != 1 || victim.skipped[0].Skipped != 1 || victim.skipped[0].Behind != 70*time.Second {
			t.Fatalf("victim skipped reports = %+v, want one of 1 tick, 70s behind", victim.skipped)
		}
	})
}

// latestDue steps over a cron schedule's missed ticks one by one.
func TestLatestDueOfACronSchedule(t *testing.T) {
	t.Parallel()
	cron, err := parseCron("*/15 * * * *")
	if err != nil {
		t.Fatal(err)
	}
	job := scheduledJob{spec: ScheduleSpec{Cron: "*/15 * * * *"}, cron: cron}
	first := time.Date(2026, time.October, 5, 8, 0, 0, 0, time.UTC)
	at, passed, err := job.latestDue(first, first.Add(time.Hour+5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if want := first.Add(time.Hour); !at.Equal(want) || passed != 4 {
		t.Fatalf("latestDue = %s, %d; want %s, 4", at, passed, want)
	}
	s := NewScheduler(SchedulerOptions{})
	if got := s.maxLateness(job, at); got != 15*time.Minute {
		t.Fatalf("default lateness of a */15 cron = %s, want 15m", got)
	}
	daily := scheduledJob{spec: ScheduleSpec{DailyAt: &DailyTime{Hour: 3}}}
	if got := s.maxLateness(daily, at); got != 24*time.Hour {
		t.Fatalf("default lateness of a daily job = %s, want 24h", got)
	}
}

func TestNextCronRun(t *testing.T) {
	after := time.Date(2026, time.October, 5, 8, 29, 0, 0, time.UTC) // Monday.
	got, err := nextCronRun("30 8 * * 1-5", after)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, time.October, 5, 8, 30, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("next cron run = %s, want %s", got, want)
	}

	got, err = nextCronRun("0 1 * * 7", time.Date(2026, time.October, 4, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	want = time.Date(2026, time.October, 4, 1, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("Sunday alias next run = %s, want %s", got, want)
	}

	if _, err := nextCronRun("0 0 31 2 *", after); err == nil {
		t.Fatal("impossible calendar schedule should return an error")
	}
}

func TestSchedulerOptionsValidate(t *testing.T) {
	t.Parallel()
	for _, valid := range []int{0, 1, 4, 256} {
		if err := (SchedulerOptions{MaxConcurrentJobs: valid}).Validate(); err != nil {
			t.Errorf("MaxConcurrentJobs %d refused: %v", valid, err)
		}
	}
	for _, invalid := range []int{-1, 257, 1 << 20} {
		if err := (SchedulerOptions{MaxConcurrentJobs: invalid}).Validate(); err == nil {
			t.Errorf("MaxConcurrentJobs %d accepted", invalid)
		}
	}
	if err := (SchedulerOptions{MaxLateness: -time.Second}).Validate(); err == nil {
		t.Error("a negative MaxLateness was accepted")
	}
	if got := NewScheduler(SchedulerOptions{MaxLateness: -time.Second}).opts.MaxLateness; got != 0 {
		t.Errorf("negative MaxLateness: %s, want the default (0)", got)
	}
	// NewScheduler keeps its clamping, so existing callers are unaffected.
	if got := cap(NewScheduler(SchedulerOptions{MaxConcurrentJobs: -3}).concurrent); got != 4 {
		t.Errorf("negative: %d slots, want the default 4", got)
	}
	if got := cap(NewScheduler(SchedulerOptions{MaxConcurrentJobs: 1000}).concurrent); got != 256 {
		t.Errorf("too large: %d slots, want 256", got)
	}
}
