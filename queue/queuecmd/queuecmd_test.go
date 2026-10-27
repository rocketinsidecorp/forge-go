package queuecmd_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/rocketinsidecorp/forge-go/command"
	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/queue"
	"github.com/rocketinsidecorp/forge-go/queue/queuecmd"
)

// queuecmd.Commands is the jobs command (tested in queue/internal/jobscmd)
// backed by the caller's opener.
func TestCommandsIsTheJobsCommandBackedByTheOpener(t *testing.T) {
	t.Parallel()
	openErr := errors.New("no database here")
	commands := queuecmd.Commands(func(context.Context, config.Database) (queue.Admin, func() error, error) {
		return nil, nil, openErr
	})
	if len(commands) != 1 || commands[0].Name != "jobs" {
		t.Fatalf("commands = %+v, want the jobs command", commands)
	}
	cfg := config.Default()
	cfg.Database.URL = config.NewSecret("postgres://u:p@127.0.0.1/db")
	env := command.Env{Config: cfg, Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	if err := commands[0].Run(t.Context(), env, []string{"status"}); !errors.Is(err, openErr) {
		t.Fatalf("status = %v, want the opener's error", err)
	}
	if err := commands[0].Run(t.Context(), env, nil); !errors.Is(err, command.ErrUsage) {
		t.Fatalf("no subcommand = %v, want ErrUsage", err)
	}
}
