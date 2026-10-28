// Package queuecmd provides the "jobs" subcommand of an application binary
// for any queue.Admin. The queue adapters (queue/postgres, queue/mysql,
// queue/sqlite) offer it ready-made as their Commands(); use this package
// directly to back the command with another store. The commands ship inside
// the production image, so operators inspect and repair the queue with no
// extra tooling:
//
//	app jobs status                          counts per queue
//	app jobs dead [-queue Q] [-kind K] [-limit N]   list dead jobs
//	app jobs show ID                         one job (never its payload)
//	app jobs retry ID                        move a dead job back to pending
//	app jobs discard ID                      delete a dead job permanently
//	app jobs prune [-older 24h]              delete finished jobs
//
// queuecmd imports no store; the stores delegate to it (ADR 0052, as amended
// by ADR 0053).
package queuecmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/rocketinsidecorp/forge-go/command"
	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/queue"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

// Opener connects to the configured database and returns the queue's admin
// surface and a function that releases the connection. Adapters provide it.
type Opener func(ctx context.Context, cfg config.Database) (queue.Admin, func() error, error)

const (
	usage        = "jobs status | dead [-queue Q] [-kind K] [-limit N] | show ID | retry ID | discard ID | prune [-older D]"
	maxListLimit = 1000
	errorColumn  = 80
)

// Commands returns the "jobs" command backed by open.
func Commands(open Opener) []command.Command {
	return []command.Command{{
		Name:    "jobs",
		Summary: "inspect and repair the background job queue (status, dead, show, retry, discard, prune)",
		Run: func(ctx context.Context, env command.Env, args []string) (err error) {
			if len(args) == 0 {
				return fmt.Errorf("%w: %s", command.ErrUsage, usage)
			}
			if env.Config.Database.URL.IsZero() {
				return errors.New("database is not configured: set FORGE_DATABASE_URL")
			}
			sub, rest := args[0], args[1:]
			if !validSubcommand(sub) {
				return fmt.Errorf("%w: %s", command.ErrUsage, usage)
			}
			// Parse arguments before connecting: a typo must not cost a
			// database round trip.
			run, err := parse(sub, rest, env)
			if err != nil {
				return err
			}
			admin, closeFn, err := open(ctx, env.Config.Database)
			if err != nil {
				return err
			}
			defer func() { err = errors.Join(err, closeFn()) }()
			return run(ctx, admin)
		},
	}}
}

func validSubcommand(name string) bool {
	switch name {
	case "status", "dead", "show", "retry", "discard", "prune":
		return true
	}
	return false
}

type action func(ctx context.Context, admin queue.Admin) error

func parse(sub string, args []string, env command.Env) (action, error) {
	flags := flag.NewFlagSet("jobs "+sub, flag.ContinueOnError)
	flags.SetOutput(env.Stderr)
	var (
		queueName = flags.String("queue", "", "only this queue")
		kind      = flags.String("kind", "", "only this kind")
		limit     = flags.Int("limit", 25, "maximum rows")
		older     = flags.Duration("older", 24*time.Hour, "minimum age of finished jobs")
	)
	if err := flags.Parse(args); err != nil {
		return nil, fmt.Errorf("%w: %w", command.ErrUsage, err)
	}
	out := env.Stdout
	switch sub {
	case "status":
		if flags.NArg() != 0 {
			return nil, fmt.Errorf("%w: jobs status", command.ErrUsage)
		}
		return func(ctx context.Context, admin queue.Admin) error { return status(ctx, admin, out) }, nil
	case "dead":
		if flags.NArg() != 0 || *limit < 1 || *limit > maxListLimit {
			return nil, fmt.Errorf("%w: jobs dead [-queue Q] [-kind K] [-limit 1-%d]", command.ErrUsage, maxListLimit)
		}
		return func(ctx context.Context, admin queue.Admin) error {
			return dead(ctx, admin, out, *queueName, *kind, *limit)
		}, nil
	case "prune":
		if flags.NArg() != 0 || *older <= 0 {
			return nil, fmt.Errorf("%w: jobs prune [-older D] with D > 0", command.ErrUsage)
		}
		return func(ctx context.Context, admin queue.Admin) error { return prune(ctx, admin, out, *older) }, nil
	}
	// show, retry, discard take one ID.
	if flags.NArg() != 1 {
		return nil, fmt.Errorf("%w: jobs %s ID", command.ErrUsage, sub)
	}
	id, err := uuid.Parse(flags.Arg(0))
	if err != nil {
		return nil, fmt.Errorf("%w: %q is not a job ID (UUIDv7)", command.ErrUsage, flags.Arg(0))
	}
	switch sub {
	case "show":
		return func(ctx context.Context, admin queue.Admin) error { return show(ctx, admin, out, id) }, nil
	case "retry":
		return func(ctx context.Context, admin queue.Admin) error {
			if err := admin.Retry(ctx, id); err != nil {
				return explain(err, id)
			}
			_, err := fmt.Fprintf(out, "retrying %s\n", id)
			return err
		}, nil
	default: // discard
		return func(ctx context.Context, admin queue.Admin) error {
			if err := admin.Discard(ctx, id); err != nil {
				return explain(err, id)
			}
			_, err := fmt.Fprintf(out, "discarded %s\n", id)
			return err
		}, nil
	}
}

// safe escapes control characters and bidirectional overrides in text read
// from the database, which other writers may have stored, so printing it can
// never drive the operator's terminal.
func safe(text string) string { return queue.EscapeUnsafe(text) }

func explain(err error, id uuid.UUID) error {
	switch {
	case errors.Is(err, queue.ErrNotFound):
		return fmt.Errorf("job %s does not exist or is not dead", id)
	case errors.Is(err, queue.ErrKeyInUse):
		return fmt.Errorf("job %s cannot be retried: a newer job with the same kind and idempotency key is active", id)
	}
	return err
}

func status(ctx context.Context, admin queue.Admin, out io.Writer) error {
	stats, err := admin.Stats(ctx)
	if err != nil {
		return err
	}
	if len(stats.Queues) == 0 {
		_, err := fmt.Fprintln(out, "no jobs")
		return err
	}
	table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "QUEUE\tREADY\tSCHEDULED\tRUNNING\tDEAD\tSUCCEEDED\tOLDEST READY")
	now := time.Now()
	for _, q := range stats.Queues {
		oldest := "-"
		if q.OldestReadyAt != nil {
			oldest = now.Sub(*q.OldestReadyAt).Round(time.Second).String()
		}
		fmt.Fprintf(table, "%s\t%d\t%d\t%d\t%d\t%d\t%s\n", safe(q.Queue), q.Ready, q.Scheduled, q.Running, q.Dead, q.Succeeded, oldest)
	}
	return table.Flush()
}

func dead(ctx context.Context, admin queue.Admin, out io.Writer, queueName, kind string, limit int) error {
	table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "ID\tQUEUE\tKIND\tATTEMPTS\tFINISHED\tERROR")
	var cursor *queue.DeadCursor
	listed := 0
	for listed < limit {
		page, err := admin.ListDead(ctx, queue.DeadQuery{Queue: queueName, Kind: kind, After: cursor, Limit: min(limit-listed, queue.MaxPageSize)})
		if err != nil {
			return err
		}
		for _, item := range page.Items {
			finished := "-"
			if item.FinishedAt != nil {
				finished = item.FinishedAt.UTC().Format(time.RFC3339)
			}
			fmt.Fprintf(table, "%s\t%s\t%s\t%d/%d\t%s\t%s\n", item.Job.ID, safe(item.Job.Queue), safe(item.Job.Kind), item.Job.Attempt, item.Job.MaxAttempts, finished, abbreviate(safe(item.LastError), errorColumn))
		}
		listed += len(page.Items)
		if page.Next == nil {
			break
		}
		cursor = page.Next
	}
	if listed == 0 {
		_, err := fmt.Fprintln(out, "no dead jobs")
		return err
	}
	return table.Flush()
}

func show(ctx context.Context, admin queue.Admin, out io.Writer, id uuid.UUID) error {
	info, err := admin.Get(ctx, id)
	if err != nil {
		if errors.Is(err, queue.ErrNotFound) {
			return fmt.Errorf("job %s does not exist", id)
		}
		return err
	}
	job := info.Job
	fmt.Fprintf(out, "id:           %s\n", job.ID)
	fmt.Fprintf(out, "kind:         %s\n", safe(job.Kind))
	fmt.Fprintf(out, "queue:        %s\n", safe(job.Queue))
	fmt.Fprintf(out, "state:        %s\n", safe(string(info.State)))
	fmt.Fprintf(out, "attempts:     %d of %d\n", job.Attempt, job.MaxAttempts)
	fmt.Fprintf(out, "priority:     %d\n", job.Priority)
	fmt.Fprintf(out, "run at:       %s\n", job.RunAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(out, "enqueued at:  %s\n", job.EnqueuedAt.UTC().Format(time.RFC3339))
	if job.Key != "" {
		fmt.Fprintf(out, "key:          %s\n", safe(job.Key))
	}
	if info.WorkerID != "" {
		fmt.Fprintf(out, "worker:       %s\n", safe(info.WorkerID))
	}
	if info.LeaseUntil != nil {
		fmt.Fprintf(out, "lease until:  %s\n", info.LeaseUntil.UTC().Format(time.RFC3339))
	}
	if info.FinishedAt != nil {
		fmt.Fprintf(out, "finished at:  %s\n", info.FinishedAt.UTC().Format(time.RFC3339))
	}
	if info.LastError != "" {
		fmt.Fprintf(out, "last error:   %s\n", safe(info.LastError))
	}
	return nil
}

func prune(ctx context.Context, admin queue.Admin, out io.Writer, older time.Duration) error {
	var total int64
	for {
		n, err := admin.Prune(ctx, older, 1000)
		if err != nil {
			return err
		}
		total += n
		if n < 1000 {
			break
		}
	}
	_, err := fmt.Fprintf(out, "deleted %d finished job(s)\n", total)
	return err
}

func abbreviate(text string, width int) string {
	text = strings.Join(strings.Fields(text), " ")
	if len(text) <= width {
		return text
	}
	runes := []rune(text)
	if len(runes) <= width {
		return text
	}
	return string(runes[:width-3]) + "..."
}
