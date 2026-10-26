// Package jobs provides an in-process scheduler for recurring background
// work. Each replica runs its own scheduler; job authors must use advisory
// locks or idempotent outbox events for exactly-once semantics across
// replicas.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/rocketinsidecorp/forge-go/telemetry"
)

// Job is a unit of background work. It receives a context that is canceled
// on scheduler shutdown and returns an error if the work failed. Jobs must
// be safe for concurrent invocation (the scheduler may run multiple jobs
// concurrently up to the configured bound).
type Job func(context.Context) error

// ScheduleSpec describes when a job should run. Exactly one of the fields
// must be non-zero/non-empty.
type ScheduleSpec struct {
	// Every runs the job at a fixed interval.
	Every time.Duration
	// DailyAt runs the job once per day at the given UTC time (hour, minute).
	DailyAt *DailyTime
	// Cron runs the job on a standard 5-field cron schedule (UTC).
	Cron string
}

// DailyTime represents a time of day in UTC.
type DailyTime struct {
	Hour   int // 0-23
	Minute int // 0-59
}

// Validate checks that exactly one schedule field is set and values are valid.
func (s ScheduleSpec) Validate() error {
	count := 0
	if s.Every > 0 {
		count++
	}
	if s.DailyAt != nil {
		count++
		if s.DailyAt.Hour < 0 || s.DailyAt.Hour > 23 || s.DailyAt.Minute < 0 || s.DailyAt.Minute > 59 {
			return errors.New("DailyAt hour must be 0-23 and minute 0-59")
		}
	}
	if s.Cron != "" {
		count++
		if _, err := parseCron(s.Cron); err != nil {
			return fmt.Errorf("invalid cron schedule: %w", err)
		}
	}
	if count != 1 {
		return errors.New("ScheduleSpec requires exactly one of Every, DailyAt, or Cron")
	}
	if s.Every > 0 && s.Every < time.Second {
		return errors.New("every interval must be at least 1 second")
	}
	return nil
}

// nextRun returns the next time after 'after' that the schedule fires.
// For Every, it's the next multiple of the interval after 'after'.
// For DailyAt, it's the next occurrence of that time-of-day.
// For Cron, it uses numeric 5-field cron syntax in UTC.
func (s ScheduleSpec) nextRun(after time.Time) (time.Time, error) {
	if s.Every > 0 {
		// Align to interval boundary after 'after'
		nanos := after.UnixNano()
		intervalNanos := s.Every.Nanoseconds()
		nextNanos := ((nanos / intervalNanos) + 1) * intervalNanos
		return time.Unix(0, nextNanos).UTC(), nil
	}
	if s.DailyAt != nil {
		next := time.Date(after.Year(), after.Month(), after.Day(), s.DailyAt.Hour, s.DailyAt.Minute, 0, 0, time.UTC)
		if !next.After(after) {
			next = next.Add(24 * time.Hour)
		}
		return next, nil
	}
	return time.Time{}, errors.New("no schedule set")
}

// Scheduler runs jobs on a schedule. It is not a distributed scheduler;
// each replica runs its own instance. Jobs that must run exactly once per
// schedule tick across replicas should use postgres.TryAdvisoryXactLock.
type Scheduler struct {
	jobs       map[string]scheduledJob
	opts       SchedulerOptions
	mu         sync.Mutex
	ctx        context.Context
	cancel     context.CancelFunc
	concurrent chan struct{}
	tracer     *telemetry.Tracer
	duration   *telemetry.Histogram
	started    bool
	stopped    bool
	running    int           // job goroutines that have not returned
	drained    chan struct{} // closed once the scheduler is stopped and no job goroutine is running
}

type scheduledJob struct {
	spec ScheduleSpec
	job  Job
	cron *cronSchedule
}

// SchedulerOptions bounds scheduler behavior.
type SchedulerOptions struct {
	// MaxConcurrentJobs bounds the number of jobs running simultaneously.
	// Default 4 (zero), maximum 256; Validate refuses anything else.
	MaxConcurrentJobs int
	// OnError receives job failures. When nil, failures are sent to the
	// standard structured logger. The callback may be invoked concurrently.
	OnError func(jobName string, err error)
	// Telemetry traces each run with a span (the root of a new trace) and
	// records forge.scheduler.job.duration by job name and outcome
	// (succeeded, failed, timeout). Error messages are never recorded. Nil
	// disables it.
	Telemetry *telemetry.Telemetry
}

// maxConcurrentJobs is the largest MaxConcurrentJobs; defaultConcurrentJobs
// is what zero means.
const (
	maxConcurrentJobs     = 256
	defaultConcurrentJobs = 4
)

// Validate reports options NewScheduler would silently change: a negative
// MaxConcurrentJobs, or one above 256 (zero is valid and means the default,
// 4). Call it where the options are built to refuse a misconfigured scheduler
// at startup, like the other constructors that return an error; NewScheduler
// itself keeps clamping, so existing callers are unaffected.
func (o SchedulerOptions) Validate() error {
	if o.MaxConcurrentJobs < 0 || o.MaxConcurrentJobs > maxConcurrentJobs {
		return fmt.Errorf("jobs: MaxConcurrentJobs must be between 0 (the default, %d) and %d, got %d", defaultConcurrentJobs, maxConcurrentJobs, o.MaxConcurrentJobs)
	}
	return nil
}

// NewScheduler creates a scheduler with the given options. Zero or negative
// MaxConcurrentJobs uses the default and a value above 256 is lowered to 256;
// SchedulerOptions.Validate reports both instead.
func NewScheduler(opts SchedulerOptions) *Scheduler {
	if opts.MaxConcurrentJobs < 1 {
		opts.MaxConcurrentJobs = defaultConcurrentJobs
	}
	if opts.MaxConcurrentJobs > maxConcurrentJobs {
		opts.MaxConcurrentJobs = maxConcurrentJobs
	}
	if opts.OnError == nil {
		opts.OnError = func(name string, err error) {
			slog.Default().Error("scheduled job failed", "job", name, "error", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Scheduler{
		jobs:       make(map[string]scheduledJob),
		opts:       opts,
		ctx:        ctx,
		cancel:     cancel,
		concurrent: make(chan struct{}, opts.MaxConcurrentJobs),
		drained:    make(chan struct{}),
	}
	if opts.Telemetry != nil {
		s.tracer = opts.Telemetry.Tracer(telemetry.ScopeName + "/jobs")
		duration, err := opts.Telemetry.Metrics().Histogram(MetricJobDuration, "Duration of scheduled job runs.", "s", telemetry.DurationBuckets,
			"forge.scheduler.job", "forge.scheduler.outcome")
		if err != nil {
			// Only a conflicting metric definition fails; tracing still works.
			opts.OnError("telemetry", err)
		}
		s.duration = duration
	}
	return s
}

// MetricJobDuration is the histogram of scheduled job runs.
const MetricJobDuration = "forge.scheduler.job.duration"

// run executes one scheduled run with its telemetry.
func (s *Scheduler) run(ctx context.Context, name string, job Job) error {
	started := time.Now()
	ctx, span := s.tracer.Start(ctx, "job "+name, telemetry.SpanKindInternal, telemetry.String("forge.scheduler.job", name))
	err := job(ctx)
	outcome := "succeeded"
	switch {
	case err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded):
		outcome = "timeout"
	case err != nil:
		outcome = "failed"
	}
	if span.IsRecording() {
		span.SetAttributes(telemetry.String("forge.scheduler.outcome", outcome))
		span.RecordError(err)
	}
	span.End()
	s.duration.Record(time.Since(started).Seconds(), name, outcome)
	return err
}

// Register adds a job with a unique name and schedule. It must be called
// before Start. The name is used for logging and must be unique.
func (s *Scheduler) Register(name string, spec ScheduleSpec, job Job) error {
	if name == "" {
		return errors.New("job name is required")
	}
	if err := spec.Validate(); err != nil {
		return fmt.Errorf("invalid schedule for job %q: %w", name, err)
	}
	if job == nil {
		return errors.New("job function is required")
	}
	if spec.DailyAt != nil {
		daily := *spec.DailyAt
		spec.DailyAt = &daily
	}
	var cron *cronSchedule
	if spec.Cron != "" {
		var err error
		cron, err = parseCron(spec.Cron)
		if err != nil {
			return fmt.Errorf("invalid cron schedule for job %q: %w", name, err)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || s.stopped {
		return errors.New("jobs must be registered before the scheduler starts")
	}
	if _, exists := s.jobs[name]; exists {
		return fmt.Errorf("job %q already registered", name)
	}
	s.jobs[name] = scheduledJob{spec: spec, job: job, cron: cron}
	return nil
}

// Start begins running all registered jobs on their schedules. It is
// idempotent and returns immediately; jobs run in owned goroutines until Stop.
func (s *Scheduler) Start() {
	s.mu.Lock()
	if s.started || s.stopped {
		s.mu.Unlock()
		return
	}
	s.started = true
	jobs := make(map[string]scheduledJob, len(s.jobs))
	for name, sj := range s.jobs {
		jobs[name] = sj
	}
	s.running = len(jobs)
	s.mu.Unlock()
	for name, sj := range jobs {
		go s.runJob(name, sj)
	}
}

// Stop cancels the scheduler context and waits for all in-flight jobs to
// complete, however long that takes. It is safe to call more than once. Prefer
// StopContext when a shutdown budget applies.
func (s *Scheduler) Stop() error {
	return s.StopContext(context.Background())
}

// StopContext stops scheduling, cancels the context every job receives and
// waits for running jobs to return until ctx is done. It returns nil once all
// jobs have returned and ctx.Err() when ctx ended first, in which case jobs
// that ignore cancellation keep running: the scheduler starts nothing new and
// owns no goroutine beyond those jobs' own, which end when the jobs return. It
// is safe to call more than once and concurrently, and a later call may wait
// again for the same jobs. A nil ctx waits without a deadline.
func (s *Scheduler) StopContext(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	s.stopped = true
	s.cancel()
	s.closeDrainedLocked()
	s.mu.Unlock()
	select {
	case <-s.drained:
		return nil
	case <-ctx.Done():
		// Finishing together with the deadline is not a timeout.
		select {
		case <-s.drained:
			return nil
		default:
			return ctx.Err()
		}
	}
}

// jobGoroutineDone records that a job goroutine returned.
func (s *Scheduler) jobGoroutineDone() {
	s.mu.Lock()
	s.running--
	s.closeDrainedLocked()
	s.mu.Unlock()
}

// closeDrainedLocked closes drained once the scheduler is stopped and no job
// goroutine runs. Start and Register refuse once stopped is set, so running
// cannot grow afterwards. s.mu must be held.
func (s *Scheduler) closeDrainedLocked() {
	if !s.stopped || s.running > 0 {
		return
	}
	select {
	case <-s.drained:
	default:
		close(s.drained)
	}
}

func (s *Scheduler) runJob(name string, sj scheduledJob) {
	defer s.jobGoroutineDone()
	nextRun := time.Now().UTC()
	for {
		var err error
		if sj.cron != nil {
			nextRun, err = sj.cron.nextRun(nextRun)
		} else {
			nextRun, err = sj.spec.nextRun(nextRun)
		}
		if err != nil {
			s.opts.OnError(name, err)
			return
		}
		wait := time.Until(nextRun)
		if wait < 0 {
			wait = 0
		}
		timer := time.NewTimer(wait)
		select {
		case <-s.ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return
		case <-timer.C:
		}
		select {
		case <-s.ctx.Done():
			return
		case s.concurrent <- struct{}{}:
		}
		if s.ctx.Err() != nil {
			<-s.concurrent
			return
		}
		jobCtx, cancel := context.WithTimeout(s.ctx, jobTimeout(sj.spec))
		if err := s.run(jobCtx, name, sj.job); err != nil {
			s.opts.OnError(name, err)
		}
		cancel()
		<-s.concurrent
	}
}

func jobTimeout(spec ScheduleSpec) time.Duration {
	if spec.Every > 0 {
		return min(spec.Every, 5*time.Minute)
	}
	if spec.DailyAt != nil {
		return 5 * time.Minute
	}
	return 10 * time.Minute // cron jobs may be longer-running
}
