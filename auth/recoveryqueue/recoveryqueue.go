// Package recoveryqueue moves password-recovery requests onto Forge's
// persistent job queue (package queue): an adapter between auth's
// RecoveryDispatcher and queue's Registry, so the request page answers
// without looking an account up, issuing a token or sending mail, whatever
// the mail server's latency, and a failed delivery is retried with backoff.
//
// Wire it where the application has a queue store (queue/postgres.Store,
// queue/mysql.Store or queue/sqlite.Store) and a worker:
//
//	registry := queue.NewRegistry()
//	dispatcher, err := recoveryqueue.NewDispatcher(registry, store)
//	service, err := auth.NewRecoveryService(recoveryStore, sender, auth.WithRecoveryDispatcher(dispatcher))
//	err = recoveryqueue.Register(registry, service)
//	worker, err := queue.NewWorker(store, registry, queue.WorkerOptions{})
//	err = worker.Start(ctx) // and register worker.Shutdown with App.OnShutdown
//
// Applications without a persistent queue use auth.RecoveryQueue instead.
//
// # What is stored
//
// The job payload is the canonical login and tenant slug the visitor named
// (whether or not an account has them) and two SHA-256 digests for the audit
// event. It never holds a token: the token is created when the job runs and
// leaves the process only through the RecoverySender. The login is personal
// data kept until the job is pruned (queue.WorkerOptions.RetainSucceeded,
// 24 hours by default; dead jobs stay until an operator discards them), so
// keep the retention short.
//
// While a job for one login and tenant is pending or running, further
// requests for them store nothing (an idempotency key), so repeating the
// request cannot queue unbounded work.
package recoveryqueue

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/queue"
)

const (
	// Kind is the job kind of a recovery request.
	Kind = "auth.password_recovery"

	defaultMaxAttempts = 5
	defaultTimeout     = 30 * time.Second
)

// payload is the stored job. Field names are part of the stored format.
type payload struct {
	Email   string `json:"email"`
	Tenant  string `json:"tenant"`
	Account string `json:"account"`
	Source  string `json:"source,omitempty"`
}

// Dispatcher is an auth.RecoveryDispatcher that enqueues jobs.
type Dispatcher struct {
	registry *queue.Registry
	inserter queue.Inserter
	options  []queue.EnqueueOption
}

var _ auth.RecoveryDispatcher = (*Dispatcher)(nil)

// NewDispatcher returns a dispatcher that stores jobs with inserter (a queue
// Store) for the kind that Register adds to registry. Options apply to every
// job (for example queue.WithQueue).
func NewDispatcher(registry *queue.Registry, inserter queue.Inserter, options ...queue.EnqueueOption) (*Dispatcher, error) {
	if registry == nil || inserter == nil {
		return nil, errors.New("recoveryqueue: registry and inserter are required")
	}
	return &Dispatcher{registry: registry, inserter: inserter, options: options}, nil
}

// DispatchRecovery enqueues the request. A request already pending or running
// for the same login and tenant is a success that stores nothing.
func (d *Dispatcher) DispatchRecovery(ctx context.Context, job auth.RecoveryJob) error {
	body := payload{
		Email:   job.Email,
		Tenant:  job.TenantSlug,
		Account: hex.EncodeToString(job.Key.Account[:]),
	}
	if job.Key.HasSource {
		body.Source = hex.EncodeToString(job.Key.Source[:])
	}
	digest := sha256.Sum256([]byte(job.TenantSlug + "\x00" + job.Email))
	options := append([]queue.EnqueueOption{queue.WithKey(Kind + ":" + hex.EncodeToString(digest[:]))}, d.options...)
	if _, err := queue.Enqueue(ctx, d.inserter, d.registry, Kind, body, options...); err != nil {
		return fmt.Errorf("recoveryqueue: enqueue: %w", err)
	}
	return nil
}

// Register adds the handler of Kind to registry. processor is the
// auth.RecoveryService. A failed attempt (a delivery failure, a database
// error) is retried up to 5 times with the worker's backoff, and each retry
// issues a new token and invalidates the previous one, so only the newest
// link works; options override the defaults (queue.HandlerMaxAttempts,
// queue.HandlerTimeout, ...).
func Register(registry *queue.Registry, processor auth.RecoveryProcessor, options ...queue.HandlerOption) error {
	if processor == nil {
		return errors.New("recoveryqueue: processor is required")
	}
	options = append([]queue.HandlerOption{queue.HandlerMaxAttempts(defaultMaxAttempts), queue.HandlerTimeout(defaultTimeout)}, options...)
	return queue.Register(registry, Kind, func(ctx context.Context, body payload) error {
		job := auth.RecoveryJob{Email: body.Email, TenantSlug: body.Tenant}
		if err := decodeDigest(body.Account, &job.Key.Account); err != nil {
			return queue.Permanent(err)
		}
		if body.Source != "" {
			if err := decodeDigest(body.Source, &job.Key.Source); err != nil {
				return queue.Permanent(err)
			}
			job.Key.HasSource = true
		}
		return processor.Process(ctx, job)
	}, options...)
}

func decodeDigest(text string, into *[sha256.Size]byte) error {
	raw, err := hex.DecodeString(text)
	if err != nil || len(raw) != sha256.Size {
		return errors.New("recoveryqueue: stored digest is malformed")
	}
	copy(into[:], raw)
	return nil
}
