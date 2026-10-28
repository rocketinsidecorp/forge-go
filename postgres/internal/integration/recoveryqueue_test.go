package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/auth"
	authpostgres "github.com/rocketinsidecorp/forge-go/auth/postgres"
	"github.com/rocketinsidecorp/forge-go/auth/recoveryqueue"
	"github.com/rocketinsidecorp/forge-go/postgres"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"
	"github.com/rocketinsidecorp/forge-go/queue"
	queuepostgres "github.com/rocketinsidecorp/forge-go/queue/postgres"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

type senderFunc func(context.Context, auth.PasswordRecoveryMessage) error

func (f senderFunc) SendPasswordRecovery(ctx context.Context, message auth.PasswordRecoveryMessage) error {
	return f(ctx, message)
}

type fixture struct {
	db       *postgres.DB
	store    *queuepostgres.Store
	registry *queue.Registry
	service  *auth.RecoveryService
	worker   *queue.Worker
}

// newFixture wires the recovery service to a real PostgreSQL job queue. The
// worker is created but not started.
func newFixture(t *testing.T, sender auth.RecoverySender) *fixture {
	t.Helper()
	db := postgrestest.NewMigratedSets(t, authpostgres.Migrations(), queuepostgres.Migrations())
	repo, err := authpostgres.New(db)
	if err != nil {
		t.Fatal(err)
	}
	store, err := queuepostgres.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	registry := queue.NewRegistry()
	dispatcher, err := recoveryqueue.NewDispatcher(registry, store)
	if err != nil {
		t.Fatal(err)
	}
	service, err := auth.NewRecoveryService(repo, sender,
		auth.WithRecoveryDispatcher(dispatcher), auth.WithRecoveryResponseTime(0), auth.WithRecoveryAuditor(repo))
	if err != nil {
		t.Fatal(err)
	}
	if err := recoveryqueue.Register(registry, service); err != nil {
		t.Fatal(err)
	}
	worker, err := queue.NewWorker(store, registry, queue.WorkerOptions{PollInterval: 20 * time.Millisecond, MaxPollInterval: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	userID, orgID, tenantID, membershipID := uuid.MustNew(), uuid.MustNew(), uuid.MustNew(), uuid.MustNew()
	ctx := context.Background()
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{"INSERT INTO forge_users (id, email, email_normalized, password_hash) VALUES ($1, 'Alice@Example.com', 'alice@example.com', 'test-only')", []any{userID}},
		{"INSERT INTO forge_organizations (id, name) VALUES ($1, 'Acme')", []any{orgID}},
		{"INSERT INTO forge_tenants (id, organization_id, slug) VALUES ($1, $2, 'acme')", []any{tenantID, orgID}},
		{"INSERT INTO forge_memberships (id, user_id, tenant_id) VALUES ($1, $2, $3)", []any{membershipID, userID, tenantID}},
	} {
		if _, err := db.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	return &fixture{db: db, store: store, registry: registry, service: service, worker: worker}
}

func (f *fixture) jobs(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.db.QueryRow(context.Background(), "SELECT count(*) FROM forge_jobs WHERE kind = $1", recoveryqueue.Kind).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRequestsAreQueuedAndDeliveredByTheWorkerToTheStoredAddress(t *testing.T) {
	delivered := make(chan auth.PasswordRecoveryMessage, 2)
	release := make(chan struct{})
	f := newFixture(t, senderFunc(func(ctx context.Context, message auth.PasswordRecoveryMessage) error {
		delivered <- message
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}))
	ctx := context.Background()

	// The worker is not running and the sender would block: a request that
	// answers proves nothing account-dependent ran on the request path.
	for _, typed := range []string{" ALICE@example.com ", "nobody@example.com"} {
		if err := f.service.Request(ctx, auth.RecoveryRequest{Email: typed, TenantSlug: "acme", Source: "192.0.2.1"}); err != nil {
			t.Fatalf("%q: %v", typed, err)
		}
	}
	if got := f.jobs(t); got != 2 {
		t.Fatalf("%d jobs queued, want 2 (one per accepted request, known or not)", got)
	}
	var tokens int
	if err := f.db.QueryRow(ctx, "SELECT count(*) FROM forge_password_recovery").Scan(&tokens); err != nil || tokens != 0 {
		t.Fatalf("%d tokens issued on the request path (err=%v)", tokens, err)
	}

	// The stored payload names the canonical login and holds no token.
	var payload string
	if err := f.db.QueryRow(ctx, "SELECT payload::text FROM forge_jobs WHERE kind = $1 ORDER BY enqueued_at LIMIT 1", recoveryqueue.Kind).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var stored map[string]string
	if err := json.Unmarshal([]byte(payload), &stored); err != nil {
		t.Fatal(err)
	}
	if stored["email"] != "alice@example.com" || stored["tenant"] != "acme" || len(stored["account"]) != 64 || len(stored["source"]) != 64 || len(stored) != 4 {
		t.Fatalf("payload=%v", stored)
	}

	if err := f.worker.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		close(release)
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = f.worker.Shutdown(shutdown)
	})
	select {
	case message := <-delivered:
		if message.Email != "Alice@Example.com" || message.TenantSlug != "acme" || message.Token.Reveal() == "" {
			t.Fatalf("delivered %q for tenant %q: want the stored address, not the typed one", message.Email, message.TenantSlug)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the worker never delivered")
	}
	select {
	case message := <-delivered:
		t.Fatalf("the unknown account received a message: %q", message.Email)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestRepeatedRequestsForOneAccountQueueOneJob(t *testing.T) {
	f := newFixture(t, senderFunc(func(context.Context, auth.PasswordRecoveryMessage) error { return nil }))
	for i := 0; i < 5; i++ {
		if err := f.service.Request(context.Background(), auth.RecoveryRequest{Email: "alice@example.com", TenantSlug: "acme"}); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.jobs(t); got != 1 {
		t.Fatalf("%d jobs queued for five identical requests, want 1", got)
	}
	if err := f.service.Request(context.Background(), auth.RecoveryRequest{Email: "other@example.com", TenantSlug: "acme"}); err != nil {
		t.Fatal(err)
	}
	if got := f.jobs(t); got != 2 {
		t.Fatalf("%d jobs, want a second one for another login", got)
	}
}

func TestLookAlikeLoginsAreNeverQueued(t *testing.T) {
	f := newFixture(t, senderFunc(func(context.Context, auth.PasswordRecoveryMessage) error { return nil }))
	for _, typed := range []string{"alice@İxample.com", "Klice@example.com", "alice@example.com x"} {
		if err := f.service.Request(context.Background(), auth.RecoveryRequest{Email: typed, TenantSlug: "acme"}); err != nil {
			t.Fatalf("%q: %v", typed, err)
		}
	}
	if got := f.jobs(t); got != 0 {
		t.Fatalf("%d jobs queued for look-alike logins", got)
	}
}

func TestFailedDeliveryIsRetriedWithANewTokenAndTheOldOneStopsWorking(t *testing.T) {
	attempts := make(chan auth.PasswordRecoveryMessage, 5)
	failFirst := true
	f := newFixture(t, senderFunc(func(_ context.Context, message auth.PasswordRecoveryMessage) error {
		attempts <- message
		if failFirst {
			failFirst = false
			return errors.New("temporary SMTP failure")
		}
		return nil
	}))
	if err := f.service.Request(context.Background(), auth.RecoveryRequest{Email: "alice@example.com", TenantSlug: "acme"}); err != nil {
		t.Fatal(err)
	}
	// Retry after a short backoff.
	retry := queue.Backoff{Base: 10 * time.Millisecond, Max: 20 * time.Millisecond}
	worker, err := queue.NewWorker(f.store, f.registry, queue.WorkerOptions{PollInterval: 10 * time.Millisecond, MaxPollInterval: 20 * time.Millisecond, Backoff: retry})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = worker.Shutdown(shutdown)
	})
	var first, second auth.PasswordRecoveryMessage
	for i, into := range []*auth.PasswordRecoveryMessage{&first, &second} {
		select {
		case *into = <-attempts:
		case <-time.After(15 * time.Second):
			t.Fatalf("attempt %d never ran", i+1)
		}
	}
	if first.Token.Digest() == second.Token.Digest() {
		t.Fatal("a retry reused the token whose delivery failed")
	}
	repo, err := authpostgres.New(f.db)
	if err != nil {
		t.Fatal(err)
	}
	if usable, err := repo.RecoveryTokenUsable(context.Background(), first.Token.Digest()); err != nil || usable {
		t.Fatalf("the failed delivery's token is still usable (%v, %v)", usable, err)
	}
	if usable, err := repo.RecoveryTokenUsable(context.Background(), second.Token.Digest()); err != nil || !usable {
		t.Fatalf("the delivered token is not usable (%v, %v)", usable, err)
	}
}

func TestConstructorsRejectMissingParts(t *testing.T) {
	if _, err := recoveryqueue.NewDispatcher(nil, nil); err == nil {
		t.Error("a dispatcher without a registry or inserter was accepted")
	}
	if err := recoveryqueue.Register(queue.NewRegistry(), nil); err == nil || !strings.Contains(err.Error(), "processor") {
		t.Errorf("Register without a processor: %v", err)
	}
}
