package postgres_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rocketinsidecorp/forge-go/agent"
	agentpostgres "github.com/rocketinsidecorp/forge-go/agent/postgres"
	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/auth/authhttp"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"
	"github.com/rocketinsidecorp/forge-go/tenancy"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

func TestJournalPersistsLifecycleAndRejectsReplay(t *testing.T) {
	db := postgrestest.NewMigrated(t, agentpostgres.Migrations())
	journal, err := agentpostgres.NewJournal(db)
	if err != nil {
		t.Fatal(err)
	}
	tenant, actor, ctx := authenticatedContext(t)
	intentID := uuid.MustNew()
	attempt := agent.Attempt{
		ExecutionID: uuid.MustNew(), IntentID: intentID, ActorID: actor.SubjectID(), TenantID: tenant.ID,
		Action: "orders.create", Effect: agent.EffectMutation,
	}
	created, err := journal.Begin(ctx, attempt)
	if err != nil || !created {
		t.Fatalf("Begin()=(%v,%v), want (true,nil)", created, err)
	}
	duplicate := attempt
	duplicate.ExecutionID = uuid.MustNew()
	if created, err := journal.Begin(ctx, duplicate); err != nil || created {
		t.Fatalf("duplicate Begin()=(%v,%v), want (false,nil)", created, err)
	}
	if err := journal.Finish(ctx, attempt.ExecutionID, attempt.ActorID, agent.OutcomeSucceeded, ""); err != nil {
		t.Fatalf("Finish(): %v", err)
	}
	if err := journal.Finish(ctx, attempt.ExecutionID, attempt.ActorID, agent.OutcomeSucceeded, ""); !errors.Is(err, agentpostgres.ErrExecutionNotStarted) {
		t.Fatalf("second Finish()=%v, want ErrExecutionNotStarted", err)
	}
	if err := journal.Finish(ctx, attempt.ExecutionID, uuid.MustNew(), agent.OutcomeFailed, "execution.failed"); !errors.Is(err, agentpostgres.ErrActorMismatch) {
		t.Fatalf("cross-actor Finish()=%v, want ErrActorMismatch", err)
	}

	var status string
	if err := db.InTenantTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT status FROM forge_agent_executions WHERE execution_id = $1", attempt.ExecutionID).Scan(&status)
	}); err != nil {
		t.Fatal(err)
	}
	if status != "succeeded" {
		t.Fatalf("persisted status=%q, want succeeded", status)
	}

	rejected := attempt
	rejected.ExecutionID = uuid.MustNew()
	rejected.IntentID = uuid.MustNew()
	if err := journal.Reject(ctx, rejected, "policy.denied"); err != nil {
		t.Fatalf("Reject(): %v", err)
	}
	if err := journal.Reject(ctx, rejected, "policy.denied"); !errors.Is(err, agent.ErrIntentReplayed) {
		t.Fatalf("duplicate Reject()=%v, want ErrIntentReplayed", err)
	}
}

func TestJournalPersistsUnresolvedOutcome(t *testing.T) {
	db := postgrestest.NewMigrated(t, agentpostgres.Migrations())
	journal, err := agentpostgres.NewJournal(db)
	if err != nil {
		t.Fatal(err)
	}
	tenant, actor, ctx := authenticatedContext(t)
	attempt := agent.Attempt{ExecutionID: uuid.MustNew(), IntentID: uuid.MustNew(), ActorID: actor.SubjectID(), TenantID: tenant.ID, Action: "payments.capture", Effect: agent.EffectExternal}
	if created, err := journal.Begin(ctx, attempt); err != nil || !created {
		t.Fatalf("Begin()=(%v,%v)", created, err)
	}
	if err := journal.Finish(ctx, attempt.ExecutionID, attempt.ActorID, agent.OutcomeUnresolved, "execution.unresolved"); err != nil {
		t.Fatalf("Finish unresolved: %v", err)
	}
	var status, failureCode string
	if err := db.InTenantTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT status, failure_code FROM forge_agent_executions WHERE execution_id = $1", attempt.ExecutionID).Scan(&status, &failureCode)
	}); err != nil {
		t.Fatal(err)
	}
	if status != "unresolved" || failureCode != "execution.unresolved" {
		t.Fatalf("status=%q failure_code=%q, want unresolved durable state", status, failureCode)
	}
}

func TestJournalRequiresMatchingTenantContext(t *testing.T) {
	t.Parallel()
	db := postgrestest.NewMigrated(t, agentpostgres.Migrations())
	journal, err := agentpostgres.NewJournal(db)
	if err != nil {
		t.Fatal(err)
	}
	tenantA, actor, ctx := authenticatedContext(t)
	tenantB, _ := tenancy.New(uuid.MustNew())
	attempt := agent.Attempt{
		ExecutionID: uuid.MustNew(), IntentID: uuid.MustNew(), ActorID: actor.SubjectID(), TenantID: tenantB.ID,
		Action: "orders.read", Effect: agent.EffectReadOnly,
	}
	if _, err := journal.Begin(ctx, attempt); err == nil {
		t.Fatal("journal accepted a context for a different tenant")
	}
	forgedActor := attempt
	forgedActor.ExecutionID = uuid.MustNew()
	forgedActor.TenantID = tenantA.ID
	forgedActor.ActorID = uuid.MustNew()
	if _, err := journal.Begin(ctx, forgedActor); !errors.Is(err, agentpostgres.ErrActorMismatch) {
		t.Fatalf("journal Begin() with forged actor=%v, want ErrActorMismatch", err)
	}
}

func TestJournalRLSHidesAnotherTenantsExecutions(t *testing.T) {
	t.Parallel()
	db := postgrestest.NewMigrated(t, agentpostgres.Migrations())
	journal, err := agentpostgres.NewJournal(db)
	if err != nil {
		t.Fatal(err)
	}
	tenantA, actorA, _ := authenticatedContext(t)
	ctx := context.Background()
	role := "forge_agent_rls_" + strings.ReplaceAll(uuid.MustNew().String(), "-", "")[:12]
	quotedRole := pgx.Identifier{role}.Sanitize()
	if _, err := db.Exec(ctx, "CREATE ROLE "+quotedRole+" NOLOGIN"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(context.Background(), "DROP OWNED BY "+quotedRole)
		_, _ = db.Exec(context.Background(), "DROP ROLE "+quotedRole)
	})
	if _, err := db.Exec(ctx, "GRANT SELECT ON forge_agent_executions TO "+quotedRole); err != nil {
		t.Fatal(err)
	}
	tenantB, _ := tenancy.New(uuid.MustNew())
	ctxA, err := authenticatedContextFor(t, tenantA, actorA)
	if err != nil {
		t.Fatal(err)
	}
	attempt := agent.Attempt{
		ExecutionID: uuid.MustNew(), IntentID: uuid.MustNew(), ActorID: actorA.SubjectID(), TenantID: tenantA.ID,
		Action: "orders.read", Effect: agent.EffectReadOnly,
	}
	if created, err := journal.Begin(ctxA, attempt); err != nil || !created {
		t.Fatalf("seed tenant A journal: created=%v err=%v", created, err)
	}
	otherActor := testPrincipal(t, tenantB)
	ctxB, err := authenticatedContextFor(t, tenantB, otherActor)
	if err != nil {
		t.Fatal(err)
	}
	var visible int
	err = db.InTenantTx(ctxB, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctxB, "SET LOCAL ROLE "+quotedRole); err != nil {
			return err
		}
		return tx.QueryRow(ctxB, "SELECT count(*) FROM forge_agent_executions").Scan(&visible)
	})
	if err != nil {
		t.Fatal(err)
	}
	if visible != 0 {
		t.Fatalf("tenant B saw %d tenant A execution rows", visible)
	}
}

func authenticatedContext(t *testing.T, permissions ...auth.Permission) (tenancy.Tenant, auth.Principal, context.Context) {
	t.Helper()
	tenant, err := tenancy.New(uuid.MustNew())
	if err != nil {
		t.Fatal(err)
	}
	principal := testPrincipal(t, tenant, permissions...)
	ctx, err := authenticatedContextFor(t, tenant, principal)
	if err != nil {
		t.Fatal(err)
	}
	return tenant, principal, ctx
}

func testPrincipal(t *testing.T, tenant tenancy.Tenant, permissions ...auth.Permission) auth.Principal {
	t.Helper()
	principal, err := auth.NewPrincipal(uuid.MustNew(), tenant, permissions...)
	if err != nil {
		t.Fatal(err)
	}
	return principal
}

func authenticatedContextFor(t *testing.T, tenant tenancy.Tenant, principal auth.Principal) (context.Context, error) {
	t.Helper()
	if principal.Tenant().ID != tenant.ID {
		return nil, errors.New("test principal tenant mismatch")
	}
	token, err := auth.NewToken()
	if err != nil {
		return nil, err
	}
	middleware, err := authhttp.NewMiddleware(auth.ResolverFunc(func(_ context.Context, digest auth.Digest) (auth.Session, bool, error) {
		if digest != token.Digest() {
			return auth.Session{}, false, nil
		}
		return auth.Session{Principal: principal, ExpiresAt: time.Now().Add(time.Hour)}, true, nil
	}))
	if err != nil {
		return nil, err
	}
	var authenticated context.Context
	handler := middleware.Authenticate(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		authenticated = r.Context()
	}))
	request := httptest.NewRequest(http.MethodGet, "/agent-journal-test", nil)
	request.Header.Set("Authorization", "Bearer "+token.Reveal())
	handler.ServeHTTP(httptest.NewRecorder(), request)
	if authenticated == nil {
		return nil, errors.New("authentication middleware did not establish context")
	}
	return authenticated, nil
}
