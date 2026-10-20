package devloop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// DefaultStopGrace bounds how long the old application may take to drain
// before it is killed; it exceeds the default HTTP shutdown timeout.
const DefaultStopGrace = 35 * time.Second

// BuildError is a failed build; Output is the compiler's output.
type BuildError struct{ Output string }

func (e *BuildError) Error() string { return "build failed:\n" + e.Output }

// Runner builds the application, runs it, and on every Change builds again.
// The old application keeps running until a new build succeeded; only then
// is it stopped (SIGTERM, wait) and the new one started. A failed build
// never stops the running application.
//
// Run is single-threaded: Build, Start, Ready and the Observer callbacks are
// always called from the goroutine that called Run.
type Runner struct {
	// Dir holds the built binaries; the caller creates and removes it.
	Dir string
	// Name is the binary's base name.
	Name string
	// Build compiles the application to the given path.
	Build func(ctx context.Context, output string) error
	// Start runs a built binary.
	Start func(ctx context.Context, binary string) (Instance, error)
	// Ready blocks until the started application serves, or fails. Nil
	// means ready at once.
	Ready func(ctx context.Context, instance Instance) error
	// StopGrace is passed to Instance.Stop; zero means DefaultStopGrace.
	StopGrace time.Duration
	// Logf reports progress to the developer; nil discards.
	Logf func(format string, args ...any)
	// Observer is told what happens, so the browser can follow.
	Observer Observer
}

// Observer receives the runner's state changes. Every field is optional.
type Observer struct {
	// Building is called when a build starts.
	Building func()
	// BuildFailed is called with the compiler output when a build fails; the
	// previous application, if any, keeps running.
	BuildFailed func(output string)
	// Stopping is called before the old application is stopped.
	Stopping func()
	// Started is called when the new application is ready.
	Started func(instance Instance)
	// Crashed is called when the running application exits by itself.
	Crashed func()
	// StartFailed is called when the new application could not start or did
	// not become ready.
	StartFailed func(err error)
}

type runState struct {
	current    Instance
	binary     string // the binary current runs, or the last good build
	generation int
}

// Run builds and starts the application, then follows changes until ctx
// ends or changes is closed, and stops the application before returning. The
// first change batch is the initial build, so a build failure at start does
// not end Run: the developer fixes the code and saves.
func (r *Runner) Run(ctx context.Context, changes <-chan Change) error {
	if r.Build == nil || r.Start == nil || r.Dir == "" || r.Name == "" {
		return errors.New("devloop: runner needs Dir, Name, Build and Start")
	}
	state := &runState{}
	defer r.stop(state)
	next := Change{Count: 1}
	for {
		for next.Count > 0 && ctx.Err() == nil {
			next = r.apply(ctx, state, next, changes)
		}
		if ctx.Err() != nil {
			return nil
		}
		var exited <-chan struct{}
		if state.current != nil {
			exited = state.current.Done()
		}
		select {
		case <-ctx.Done():
			return nil
		case change, ok := <-changes:
			if !ok {
				return nil
			}
			next = change
		case <-exited:
			r.logf("application exited; waiting for changes")
			if r.Observer.Crashed != nil {
				r.Observer.Crashed()
			}
			state.current = nil
			next = r.waitForChange(ctx, changes)
			if next.Count == 0 {
				return nil
			}
		}
	}
}

func (r *Runner) waitForChange(ctx context.Context, changes <-chan Change) Change {
	select {
	case <-ctx.Done():
		return Change{}
	case change, ok := <-changes:
		if !ok {
			return Change{}
		}
		return change
	}
}

// apply handles one change. When more changes arrive while it builds, the
// finished build is stale: apply returns the newer change's batch merged for
// the caller to apply again. A zero Change means nothing is pending.
func (r *Runner) apply(ctx context.Context, state *runState, change Change, changes <-chan Change) Change {
	for {
		if ctx.Err() != nil {
			return Change{}
		}
		binary := state.binary
		if !change.EnvOnly || binary == "" {
			state.generation++
			built := filepath.Join(r.Dir, r.Name+"-"+strconv.Itoa(state.generation))
			r.logf("building")
			if r.Observer.Building != nil {
				r.Observer.Building()
			}
			if err := r.Build(ctx, built); err != nil {
				_ = os.Remove(built)
				if ctx.Err() != nil {
					return Change{}
				}
				output := err.Error()
				if build := (*BuildError)(nil); errors.As(err, &build) {
					output = build.Output
				}
				r.logf("build failed; %s\n%s", r.stillServing(state), output)
				if r.Observer.BuildFailed != nil {
					r.Observer.BuildFailed(output)
				}
				return r.pending(changes)
			}
			if newer := r.pending(changes); newer.Count > 0 {
				// The tree changed while building: this build is stale.
				_ = os.Remove(built)
				change = newer
				continue
			}
			binary = built
		}
		r.restart(ctx, state, binary)
		return r.pending(changes)
	}
}

func (r *Runner) stillServing(state *runState) string {
	if state.current != nil {
		return "the previous build keeps running"
	}
	return "nothing is running yet; save a fix to build again"
}

// pending returns a change that arrived meanwhile, or the zero Change.
func (r *Runner) pending(changes <-chan Change) Change {
	select {
	case change, ok := <-changes:
		if ok {
			return change
		}
	default:
	}
	return Change{}
}

func (r *Runner) restart(ctx context.Context, state *runState, binary string) {
	previous := state.binary
	if state.current != nil {
		r.logf("restarting")
		if r.Observer.Stopping != nil {
			r.Observer.Stopping()
		}
		r.stop(state)
	}
	instance, err := r.Start(ctx, binary)
	if err != nil {
		r.startFailed(fmt.Errorf("start application: %w", err))
		return
	}
	state.current, state.binary = instance, binary
	if r.Ready != nil {
		if err := r.Ready(ctx, instance); err != nil {
			if ctx.Err() == nil {
				r.startFailed(err)
			}
			select {
			case <-instance.Done():
				state.current = nil // it is gone; the next change starts afresh
			default:
			}
			return
		}
	}
	if previous != "" && previous != binary {
		_ = os.Remove(previous)
	}
	r.logf("running")
	if r.Observer.Started != nil {
		r.Observer.Started(instance)
	}
}

func (r *Runner) startFailed(err error) {
	r.logf("%v", err)
	if r.Observer.StartFailed != nil {
		r.Observer.StartFailed(err)
	}
}

func (r *Runner) stop(state *runState) {
	if state.current == nil {
		return
	}
	grace := r.StopGrace
	if grace <= 0 {
		grace = DefaultStopGrace
	}
	if err := state.current.Stop(grace); err != nil {
		r.logf("%v", err)
	}
	state.current = nil
}

func (r *Runner) logf(format string, args ...any) {
	if r.Logf != nil {
		r.Logf(format, args...)
	}
}
