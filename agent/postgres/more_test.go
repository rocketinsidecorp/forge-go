package postgres_test

import (
	"errors"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/agent"
	agentpostgres "github.com/rocketinsidecorp/forge-go/agent/postgres"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"
	"github.com/rocketinsidecorp/forge-go/tenancy"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

func TestJournalRefusesWhatDoesNotMatchTheAuthenticatedPrincipal(t *testing.T) {
	t.Parallel()
	if _, err := agentpostgres.NewJournal(nil); err == nil {
		t.Fatal("a nil database was accepted")
	}
	db := postgrestest.NewMigrated(t, agentpostgres.Migrations())
	journal, err := agentpostgres.NewJournal(db)
	if err != nil {
		t.Fatal(err)
	}
	tenant, actor, ctx := authenticatedContext(t)
	attempt := newAttempt(t, tenant.ID, actor.SubjectID(), "reports.read")

	otherTenant, err := tenancy.New(uuid.MustNew())
	if err != nil {
		t.Fatal(err)
	}
	wrongTenant := attempt
	wrongTenant.TenantID = otherTenant.ID
	wrongActor := attempt
	wrongActor.ActorID = uuid.MustNew()
	unknownEffect := attempt
	unknownEffect.Effect = agent.EffectUnknown
	malformed := attempt
	malformed.Action = "X"

	for name, bad := range map[string]agent.Attempt{"tenant": wrongTenant, "actor": wrongActor, "unknown effect": unknownEffect, "malformed": malformed} {
		if created, err := journal.Begin(ctx, bad); err == nil || created {
			t.Errorf("Begin accepted an attempt with a wrong %s", name)
		}
	}
	// A rejection may name an unknown action (effect unknown) but nothing else.
	if err := journal.Reject(ctx, unknownEffect, "action.unknown"); err != nil {
		t.Errorf("Reject of an unregistered action: %v", err)
	}
	for name, bad := range map[string]agent.Attempt{"tenant": wrongTenant, "actor": wrongActor} {
		if err := journal.Reject(ctx, bad, "policy.denied"); err == nil {
			t.Errorf("Reject accepted a wrong %s", name)
		}
	}
	if err := journal.Reject(ctx, newAttempt(t, tenant.ID, actor.SubjectID(), "reports.read"), "Not A Code"); err == nil {
		t.Error("Reject accepted an invalid failure code")
	}
	if err := journal.Reject(ctx, unknownEffect, "action.unknown"); !errors.Is(err, agent.ErrIntentReplayed) && err != nil {
		t.Errorf("rejecting the same intent twice: %v", err)
	}
}

func TestJournalFinishValidatesItsArguments(t *testing.T) {
	t.Parallel()
	db := postgrestest.NewMigrated(t, agentpostgres.Migrations())
	journal, err := agentpostgres.NewJournal(db)
	if err != nil {
		t.Fatal(err)
	}
	tenant, actor, ctx := authenticatedContext(t)
	attempt := newAttempt(t, tenant.ID, actor.SubjectID(), "reports.read")
	if created, err := journal.Begin(ctx, attempt); err != nil || !created {
		t.Fatalf("Begin = %v, %v", created, err)
	}
	finish := func(execution, who uuid.UUID, outcome agent.Outcome, code string) error {
		return journal.Finish(ctx, execution, who, outcome, code)
	}
	actorID := actor.SubjectID()
	if err := finish(uuid.UUID{}, actorID, agent.OutcomeSucceeded, ""); err == nil {
		t.Error("a non-UUIDv7 execution ID was accepted")
	}
	if err := finish(attempt.ExecutionID, uuid.UUID{}, agent.OutcomeSucceeded, ""); err == nil {
		t.Error("a non-UUIDv7 actor ID was accepted")
	}
	if err := finish(attempt.ExecutionID, uuid.MustNew(), agent.OutcomeSucceeded, ""); !errors.Is(err, agentpostgres.ErrActorMismatch) {
		t.Errorf("another actor: %v", err)
	}
	if err := finish(attempt.ExecutionID, actorID, agent.OutcomeDenied, "policy.denied"); err == nil {
		t.Error("the denied outcome is the journal's own, not Finish's")
	}
	if err := finish(attempt.ExecutionID, actorID, agent.OutcomeSucceeded, "execution.oops"); err == nil {
		t.Error("a success with a failure code was accepted")
	}
	if err := finish(attempt.ExecutionID, actorID, agent.OutcomeFailed, ""); err == nil {
		t.Error("a failure without a code was accepted")
	}
	// A tenant in the context other than the principal's is a mismatch.
	otherTenant, _ := tenancy.New(uuid.MustNew())
	mismatched, err := tenancy.WithContext(ctx, otherTenant)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Finish(mismatched, attempt.ExecutionID, actorID, agent.OutcomeSucceeded, ""); !errors.Is(err, agentpostgres.ErrActorMismatch) {
		t.Errorf("tenant mismatch: %v", err)
	}
	if err := finish(attempt.ExecutionID, actorID, agent.OutcomeSucceeded, ""); err != nil {
		t.Fatal(err)
	}
	if err := finish(attempt.ExecutionID, actorID, agent.OutcomeSucceeded, ""); !errors.Is(err, agentpostgres.ErrExecutionNotStarted) {
		t.Errorf("finishing twice: %v", err)
	}
}

func TestReconciliationRefusesInvalidRequests(t *testing.T) {
	t.Parallel()
	db := postgrestest.NewMigrated(t, agentpostgres.Migrations())
	journal, err := agentpostgres.NewJournal(db)
	if err != nil {
		t.Fatal(err)
	}
	tenant, actor, ctx := authenticatedContext(t, agent.PermissionReconcile)
	_, _, unprivileged := authenticatedContext(t)

	if _, err := journal.ListReconciliationCandidates(unprivileged, nil, 10); !errors.Is(err, agentpostgres.ErrReconciliationForbidden) {
		t.Errorf("list without the permission: %v", err)
	}
	if _, err := journal.ResolveReconciliation(unprivileged, uuid.MustNew(), agentpostgres.ReconciliationEffectApplied); !errors.Is(err, agentpostgres.ErrReconciliationForbidden) {
		t.Errorf("resolve without the permission: %v", err)
	}
	if _, err := journal.ListReconciliationCandidates(t.Context(), nil, 10); err == nil {
		t.Error("an unauthenticated list was accepted")
	}
	otherTenant, _ := tenancy.New(uuid.MustNew())
	mismatched, err := tenancy.WithContext(ctx, otherTenant)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.ListReconciliationCandidates(mismatched, nil, 10); !errors.Is(err, agentpostgres.ErrActorMismatch) {
		t.Errorf("tenant mismatch: %v", err)
	}
	for _, limit := range []int{-1, agentpostgres.MaxReconciliationPageSize + 1} {
		if _, err := journal.ListReconciliationCandidates(ctx, nil, limit); err == nil {
			t.Errorf("limit %d was accepted", limit)
		}
	}
	for name, cursor := range map[string]*agentpostgres.ReconciliationCursor{
		"foreign tenant": {TenantID: otherTenant.ID, CreatedAt: time.Now(), ExecutionID: uuid.MustNew()},
		"zero time":      {TenantID: tenant.ID, ExecutionID: uuid.MustNew()},
		"bad execution":  {TenantID: tenant.ID, CreatedAt: time.Now()},
	} {
		if _, err := journal.ListReconciliationCandidates(ctx, cursor, 10); !errors.Is(err, agentpostgres.ErrInvalidReconciliation) {
			t.Errorf("cursor with a %s: %v", name, err)
		}
	}
	if page, err := journal.ListReconciliationCandidates(ctx, nil, 0); err != nil || len(page.Items) != 0 || page.Next != nil {
		t.Errorf("an empty tenant: %+v, %v", page, err)
	}
	if _, err := journal.ResolveReconciliation(ctx, uuid.UUID{}, agentpostgres.ReconciliationEffectApplied); !errors.Is(err, agentpostgres.ErrInvalidReconciliation) {
		t.Errorf("bad execution ID: %v", err)
	}
	if _, err := journal.ResolveReconciliation(ctx, uuid.MustNew(), "maybe"); !errors.Is(err, agentpostgres.ErrInvalidReconciliation) {
		t.Errorf("bad result: %v", err)
	}
	if _, err := journal.ResolveReconciliation(ctx, uuid.MustNew(), agentpostgres.ReconciliationEffectApplied); !errors.Is(err, agentpostgres.ErrNotReconciliationCandidate) {
		t.Errorf("unknown execution: %v", err)
	}
	// A reserved execution may still be running, so it cannot be resolved.
	reserved := newAttempt(t, tenant.ID, actor.SubjectID(), "reports.read")
	if _, err := journal.Begin(ctx, reserved); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.ResolveReconciliation(ctx, reserved.ExecutionID, agentpostgres.ReconciliationEffectApplied); !errors.Is(err, agentpostgres.ErrNotReconciliationCandidate) {
		t.Errorf("reserved execution: %v", err)
	}
}

func TestJournalFailsWhenItsTablesAreUnavailable(t *testing.T) {
	t.Parallel()
	for _, table := range []string{"forge_agent_executions", "forge_agent_reconciliations"} {
		t.Run(table, func(t *testing.T) {
			t.Parallel()
			db := postgrestest.NewMigrated(t, agentpostgres.Migrations())
			journal, err := agentpostgres.NewJournal(db)
			if err != nil {
				t.Fatal(err)
			}
			tenant, actor, ctx := authenticatedContext(t, agent.PermissionReconcile)
			unresolved := newAttempt(t, tenant.ID, actor.SubjectID(), "payments.capture")
			if _, err := journal.Begin(ctx, unresolved); err != nil {
				t.Fatal(err)
			}
			if err := journal.Finish(ctx, unresolved.ExecutionID, actor.SubjectID(), agent.OutcomeUnresolved, "execution.unresolved"); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(ctx, "ALTER TABLE "+table+" RENAME TO "+table+"_gone"); err != nil {
				t.Fatal(err)
			}
			fresh := newAttempt(t, tenant.ID, actor.SubjectID(), "reports.read")
			results := map[string]error{
				"list": func() error { _, err := journal.ListReconciliationCandidates(ctx, nil, 10); return err }(),
				"resolve": func() error {
					_, err := journal.ResolveReconciliation(ctx, unresolved.ExecutionID, agentpostgres.ReconciliationEffectApplied)
					return err
				}(),
			}
			if table == "forge_agent_executions" {
				results["begin"] = func() error { _, err := journal.Begin(ctx, fresh); return err }()
				results["reject"] = journal.Reject(ctx, fresh, "policy.denied")
				results["finish"] = journal.Finish(ctx, unresolved.ExecutionID, actor.SubjectID(), agent.OutcomeFailed, "execution.failed")
			}
			for name, err := range results {
				if err == nil {
					t.Errorf("%s succeeded without %s", name, table)
				}
				if errors.Is(err, agentpostgres.ErrNotReconciliationCandidate) || errors.Is(err, agentpostgres.ErrExecutionNotStarted) || errors.Is(err, agent.ErrIntentReplayed) {
					t.Errorf("%s reported %v for an unavailable table", name, err)
				}
			}
		})
	}
}
