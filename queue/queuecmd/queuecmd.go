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
// The implementation lives in an internal package shared with the adapters,
// so neither depends on the other (ADR 0052).
package queuecmd

import (
	"github.com/rocketinsidecorp/forge-go/command"
	"github.com/rocketinsidecorp/forge-go/queue/internal/jobscmd"
)

// Opener connects to the configured database and returns the queue's admin
// surface and a function that releases the connection. Adapters provide it.
type Opener = jobscmd.Opener

// Commands returns the "jobs" command backed by open.
func Commands(open Opener) []command.Command {
	return jobscmd.Commands(open)
}
