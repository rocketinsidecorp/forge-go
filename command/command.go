// Package command holds the contract between an application binary's runtime
// and the extra subcommands packages contribute to it, such as the migration
// commands of postgres, mysql and sqlite or the "jobs" command of the queue
// adapters (ADR 0052).
//
// It is a leaf: it imports only the standard library and config, so the
// adapters that contribute commands never depend on the root forge package,
// which composes them. The root re-exports every name here
// (forge.Command, forge.CommandEnv, forge.ErrUsage); applications may use
// either spelling, they are the same types and the same error.
package command

import (
	"context"
	"errors"
	"io"
	"log/slog"

	"github.com/rocketinsidecorp/forge-go/config"
)

// Command is an additional subcommand of the application binary, such as the
// migration commands contributed by the postgres package. Commands ship inside
// the production image, so operational tasks need no extra tooling.
type Command struct {
	Name    string
	Summary string
	Run     func(ctx context.Context, env Env, args []string) error
}

// ErrUsage marks a Command error caused by invalid arguments; forge.Execute
// exits with status 2 for it instead of 1. Wrap it:
// fmt.Errorf("%w: ...", command.ErrUsage).
var ErrUsage = errors.New("usage")

// Env is what a Command receives from the runtime (forge.CommandEnv).
type Env struct {
	Config config.Config
	Logger *slog.Logger
	Stdout io.Writer
	Stderr io.Writer
	// Stdin is the process's standard input (os.Stdin under forge.Main), for
	// commands that read a secret piped to them or typed at a terminal.
	// Secrets never belong in arguments, which other local users can list.
	// It is nil when the caller of forge.Execute provides none.
	Stdin io.Reader
	// LookupEnv reads the process environment, like os.LookupEnv, for
	// command-specific variables that are not part of Config. It is never
	// nil.
	LookupEnv func(string) (string, bool)
}
