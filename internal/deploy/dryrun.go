package deploy

import (
	"context"
	"fmt"
	"io"
	"sync"
)

// dryRunner prints mutating commands instead of running them. Read-only
// queries run on the inner runner, so the plan shows the containers that
// would really be stopped or pruned.
type dryRunner struct {
	inner    Runner
	out      io.Writer
	redactor *Redactor
	mu       sync.Mutex
}

func (r *dryRunner) print(host, marker, text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fmt.Fprintf(r.out, "%-15s %s %s\n", host, marker, r.redactor.Redact(text))
}

func (r *dryRunner) Run(ctx context.Context, host string, cmd Command) (Result, error) {
	if cmd.ReadOnly {
		r.print(host, "?", cmd.Script)
		if r.inner == nil {
			return Result{}, nil
		}
		return r.inner.Run(ctx, host, cmd)
	}
	r.print(host, "$", cmd.Script)
	if cmd.Stdin != nil {
		r.print(host, "<", "stdin: "+cmd.StdinNote)
	}
	return Result{}, nil
}

func (r *dryRunner) Stream(_ context.Context, host string, cmd Command, _ io.Writer) error {
	r.print(host, "$", cmd.Script)
	return nil
}

func (r *dryRunner) Interactive(_ context.Context, host, script string) error {
	r.print(host, "$", script)
	return nil
}

func (r *dryRunner) Close() error {
	if r.inner == nil {
		return nil
	}
	return r.inner.Close()
}

// dryLocal prints local commands. Output (git queries) still runs.
type dryLocal struct {
	inner    LocalRunner
	out      io.Writer
	redactor *Redactor
}

func (l *dryLocal) Output(ctx context.Context, dir string, argv []string) (string, error) {
	return l.inner.Output(ctx, dir, argv)
}

func (l *dryLocal) Run(_ context.Context, _ string, argv []string, stdin []byte, env []string) error {
	fmt.Fprintf(l.out, "%-15s $ %s\n", "local", l.redactor.Redact(Join(argv...)))
	if stdin != nil {
		fmt.Fprintf(l.out, "%-15s < stdin: value not shown\n", "local")
	}
	if len(env) > 0 {
		fmt.Fprintf(l.out, "%-15s   with %d FORGE_* variables\n", "local", len(env))
	}
	return nil
}
