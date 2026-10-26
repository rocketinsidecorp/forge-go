// Package ticktest is the conformance suite every jobs.TickClaimer must pass.
// It takes two claimers on one database, as two replicas of an application
// would hold, and checks that each tick of a job is claimed once, that ticks
// only move forward, that ticks ahead of the database clock are refused, and
// that two schedulers sharing the database run each tick once.
//
//	func TestTickClaimer(t *testing.T) {
//	    ticktest.Run(t, func(t testing.TB) (jobs.TickClaimer, jobs.TickClaimer) { ... two claimers on one fresh, migrated database ... })
//	}
package ticktest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/jobs"
)

// NewClaimers returns two claimers on the same database, each with its own
// connections, which the test cleans up.
type NewClaimers func(t testing.TB) (jobs.TickClaimer, jobs.TickClaimer)

// Run executes the conformance suite.
func Run(t *testing.T, newClaimers NewClaimers) {
	t.Helper()
	t.Run("EachTickIsClaimedOnce", func(t *testing.T) { once(t, newClaimers) })
	t.Run("TicksOnlyMoveForward", func(t *testing.T) { forward(t, newClaimers) })
	t.Run("TicksAheadOfTheDatabaseAreRefused", func(t *testing.T) { early(t, newClaimers) })
	t.Run("ConcurrentClaimsOfOneTick", func(t *testing.T) { concurrent(t, newClaimers) })
	t.Run("TwoSchedulersRunEachTickOnce", func(t *testing.T) { twoSchedulers(t, newClaimers) })
}

// base is a tick well behind any database clock, truncated to the
// microsecond every adapter stores.
func base() time.Time { return time.Now().UTC().Add(-time.Hour).Truncate(time.Second) }

func claim(t *testing.T, claimer jobs.TickClaimer, job string, tick time.Time) bool {
	t.Helper()
	claimed, err := claimer.ClaimTick(context.Background(), job, tick)
	if err != nil {
		t.Fatalf("ClaimTick(%s, %s): %v", job, tick, err)
	}
	return claimed
}

func once(t *testing.T, newClaimers NewClaimers) {
	a, b := newClaimers(t)
	tick := base()
	if !claim(t, a, "report", tick) {
		t.Fatal("the first claim of a tick was refused")
	}
	if claim(t, b, "report", tick) || claim(t, a, "report", tick) {
		t.Fatal("a tick was claimed twice")
	}
	if claim(t, b, "report", tick.In(time.FixedZone("x", 3600))) {
		t.Fatal("the same instant in another time zone was claimed again")
	}
	if !claim(t, b, "cleanup", tick) {
		t.Fatal("another job's tick is blocked by this one")
	}
	if !claim(t, b, "report", tick.Add(time.Minute)) {
		t.Fatal("the next tick of the job was refused")
	}
	if !claim(t, a, "report", tick.Add(time.Minute+time.Microsecond)) {
		t.Fatal("a tick one microsecond later was refused")
	}
}

func forward(t *testing.T, newClaimers NewClaimers) {
	a, b := newClaimers(t)
	tick := base()
	if !claim(t, a, "report", tick.Add(2*time.Second)) {
		t.Fatal("first claim refused")
	}
	// A replica that lags behind can never claim an earlier tick, even one
	// nobody ran.
	if claim(t, b, "report", tick.Add(time.Second)) || claim(t, b, "report", tick) {
		t.Fatal("a tick before the recorded one was claimed")
	}
}

func early(t *testing.T, newClaimers NewClaimers) {
	a, b := newClaimers(t)
	tick := base()
	future := time.Now().UTC().Add(time.Hour)
	claimed, err := a.ClaimTick(context.Background(), "report", future)
	var earlyErr *jobs.EarlyTickError
	if claimed || !errors.As(err, &earlyErr) {
		t.Fatalf("ClaimTick an hour ahead = %v, %v; want an *EarlyTickError", claimed, err)
	}
	if earlyErr.Wait < 50*time.Minute || earlyErr.Wait > 70*time.Minute {
		t.Fatalf("EarlyTickError.Wait = %s, want about an hour", earlyErr.Wait)
	}
	// The refused tick was not recorded: an earlier one is still claimable.
	if !claim(t, b, "report", tick) {
		t.Fatal("a refused future tick blocked the job's ticks")
	}
}

func concurrent(t *testing.T, newClaimers NewClaimers) {
	a, b := newClaimers(t)
	tick := base()
	var claimed atomic.Int32
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := range 16 {
		claimer := a
		if i%2 == 1 {
			claimer = b
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := claimer.ClaimTick(context.Background(), "report", tick)
			if err != nil {
				errs <- err
				return
			}
			if ok {
				claimed.Add(1)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if claimed.Load() != 1 {
		t.Fatalf("%d of 16 concurrent claims of one tick succeeded, want 1", claimed.Load())
	}
}

// recorder counts, per tick, the claims attempted and won through it.
type recorder struct {
	claimer  jobs.TickClaimer
	mu       *sync.Mutex
	attempts map[time.Time]int
	won      map[time.Time]int
}

func (r recorder) ClaimTick(ctx context.Context, job string, tick time.Time) (bool, error) {
	claimed, err := r.claimer.ClaimTick(ctx, job, tick)
	r.mu.Lock()
	defer r.mu.Unlock()
	key := tick.UTC()
	r.attempts[key]++
	if claimed && err == nil {
		r.won[key]++
	}
	return claimed, err
}

// twoSchedulers runs two schedulers on one database until both have tried
// the same three ticks, then checks that each tick was won once and that the
// job ran once per won tick. It keys on the ticks the schedulers claim, not
// on wall-clock time, so a slow machine only makes it take longer.
func twoSchedulers(t *testing.T, newClaimers NewClaimers) {
	a, b := newClaimers(t)
	var mu sync.Mutex
	attempts, won := map[time.Time]int{}, map[time.Time]int{}
	var runs atomic.Int32
	var errs []error
	newScheduler := func(claimer jobs.TickClaimer) *jobs.Scheduler {
		s := jobs.NewScheduler(jobs.SchedulerOptions{
			Ticks: recorder{claimer: claimer, mu: &mu, attempts: attempts, won: won},
			OnError: func(_ string, err error) {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			},
		})
		if err := s.Register("tick", jobs.ScheduleSpec{Every: time.Second}, func(context.Context) error {
			runs.Add(1)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return s
	}
	first, second := newScheduler(a), newScheduler(b)
	first.Start()
	second.Start()
	triedByBoth := func() int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, tries := range attempts {
			if tries == 2 {
				n++
			}
		}
		return n
	}
	deadline := time.Now().Add(60 * time.Second)
	for triedByBoth() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("the schedulers tried only %d tick(s) in a minute", triedByBoth())
		}
		time.Sleep(20 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := first.StopContext(ctx); err != nil {
		t.Fatal(err)
	}
	if err := second.StopContext(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(errs) > 0 {
		t.Fatalf("scheduler errors: %v", errs)
	}
	total := 0
	for tick, tries := range attempts {
		if won[tick] > 1 || (tries == 2 && won[tick] != 1) {
			t.Errorf("tick %s: %d attempts, won %d times", tick.Format(time.RFC3339Nano), tries, won[tick])
		}
		total += won[tick]
	}
	// A claim won just before Stop may not have run its job.
	if n := int(runs.Load()); n > total || n < total-2 {
		t.Fatalf("the job ran %d times for %d won ticks", n, total)
	}
	if total < 3 {
		t.Fatalf("only %d tick(s) won", total)
	}
}

// BusyReplicas is the pool regression scenario of ADR 0047: two replicas,
// each with its own small connection pool, run four jobs aligned on the same
// two-second ticks, each job using its replica's database through work.
// Claiming a tick must not hold a connection while jobs run, so every run
// succeeds and each tick of each job runs once. Pass claimers built on the
// same small pools that work uses.
func BusyReplicas(t *testing.T, a, b jobs.TickClaimer, work [2]func(context.Context) error) {
	t.Helper()
	const jobCount = 4
	var mu sync.Mutex
	attempts, won := map[string]int{}, map[string]int{}
	var runs atomic.Int32
	var stopping atomic.Bool
	var errs []error
	newScheduler := func(claimer jobs.TickClaimer, use func(context.Context) error) *jobs.Scheduler {
		s := jobs.NewScheduler(jobs.SchedulerOptions{
			MaxConcurrentJobs: jobCount,
			Ticks: claimFunc(func(ctx context.Context, job string, tick time.Time) (bool, error) {
				claimed, err := claimer.ClaimTick(ctx, job, tick)
				mu.Lock()
				defer mu.Unlock()
				key := job + "@" + tick.UTC().Format(time.RFC3339Nano)
				attempts[key]++
				if claimed && err == nil {
					won[key]++
				}
				return claimed, err
			}),
			OnError: func(name string, err error) {
				if stopping.Load() && errors.Is(err, context.Canceled) {
					return // a tick won just as the test stopped the schedulers
				}
				mu.Lock()
				errs = append(errs, fmt.Errorf("%s: %w", name, err))
				mu.Unlock()
			},
		})
		for i := range jobCount {
			if err := s.Register(fmt.Sprintf("busy-%d", i), jobs.ScheduleSpec{Every: 2 * time.Second}, func(ctx context.Context) error {
				defer runs.Add(1)
				return use(ctx)
			}); err != nil {
				t.Fatal(err)
			}
		}
		return s
	}
	first, second := newScheduler(a, work[0]), newScheduler(b, work[1])
	first.Start()
	second.Start()
	triedByBoth := func() int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, tries := range attempts {
			if tries == 2 {
				n++
			}
		}
		return n
	}
	wonTicks := func() int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, wins := range won {
			n += wins
		}
		return n
	}
	deadline := time.Now().Add(90 * time.Second)
	// Stop only once every won tick's run has returned: Stop cancels runs.
	for triedByBoth() < 2*jobCount || int(runs.Load()) < wonTicks() {
		if time.Now().After(deadline) {
			t.Fatalf("after 90s: %d job tick(s) tried by both replicas, %d of %d runs finished", triedByBoth(), runs.Load(), wonTicks())
		}
		time.Sleep(5 * time.Millisecond)
	}
	stopping.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := first.StopContext(ctx); err != nil {
		t.Fatal(err)
	}
	if err := second.StopContext(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(errs) > 0 {
		t.Fatalf("%d run(s) failed, starved of connections or ticks: %v", len(errs), errs)
	}
	for key, tries := range attempts {
		if won[key] > 1 || (tries == 2 && won[key] != 1) {
			t.Errorf("%s: %d attempts, won %d times", key, tries, won[key])
		}
	}
}

type claimFunc func(ctx context.Context, job string, tick time.Time) (bool, error)

func (f claimFunc) ClaimTick(ctx context.Context, job string, tick time.Time) (bool, error) {
	return f(ctx, job, tick)
}
