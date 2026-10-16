package postgres_test

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rocketinsidecorp/forge-go/agent"
	agentpostgres "github.com/rocketinsidecorp/forge-go/agent/postgres"
	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"
	"github.com/rocketinsidecorp/forge-go/tenancy"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

func TestListReconciliationCandidatesUsesBoundedTenantScopedCursor(t *testing.T) {
	t.Parallel()
	db := postgrestest.NewMigrated(t, agentpostgres.Migrations())
	journal, err := agentpostgres.NewJournal(db)
	if err != nil {
		t.Fatal(err)
	}
	tenant, actor, ctx := authenticatedContext(t, agent.PermissionReconcile)
	reserved := newAttempt(t, tenant.ID, actor.SubjectID(), "reports.read")
	unresolved := newAttempt(t, tenant.ID, actor.SubjectID(), "payments.capture")
	succeeded := newAttempt(t, tenant.ID, actor.SubjectID(), "orders.create")
	for _, attempt := range []agent.Attempt{reserved, unresolved, succeeded} {
		created, err := journal.Begin(ctx, attempt)
		if err != nil || !created {
			t.Fatalf("Begin(%s)=(%v,%v)", attempt.Action, created, err)
		}
	}
	if err := journal.Finish(ctx, unresolved.ExecutionID, unresolved.ActorID, agent.OutcomeUnresolved, "execution.unresolved"); err != nil {
		t.Fatal(err)
	}
	if err := journal.Finish(ctx, succeeded.ExecutionID, succeeded.ActorID, agent.OutcomeSucceeded, ""); err != nil {
		t.Fatal(err)
	}

	first, err := journal.ListReconciliationCandidates(ctx, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 1 || first.Next == nil {
		t.Fatalf("first page=%+v, want one item and next cursor", first)
	}
	second, err := journal.ListReconciliationCandidates(ctx, first.Next, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Items) != 1 || second.Next != nil {
		t.Fatalf("second page=%+v, want one item and no next cursor", second)
	}
	items := append(first.Items, second.Items...)
	if sort.SliceIsSorted(items, func(i, j int) bool {
		if items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].ExecutionID.Compare(items[j].ExecutionID) < 0
		}
		return items[i].CreatedAt.Before(items[j].CreatedAt)
	}) == false {
		t.Fatalf("reconciliation pages are not in stable order: %+v", items)
	}
	seen := map[uuid.UUID]bool{}
	unresolvedFound := false
	for _, candidate := range items {
		if seen[candidate.ExecutionID] {
			t.Fatalf("duplicate reconciliation candidate %s", candidate.ExecutionID)
		}
		seen[candidate.ExecutionID] = true
		if candidate.Status != "reserved" && candidate.Status != "unresolved" {
			t.Fatalf("unexpected candidate status %q", candidate.Status)
		}
		if candidate.ExecutionID == unresolved.ExecutionID {
			unresolvedFound = true
			if candidate.FailureCode != "execution.unresolved" || candidate.CompletedAt == nil {
				t.Fatalf("unresolved candidate metadata=%+v", candidate)
			}
		}
	}
	if len(seen) != 2 || seen[succeeded.ExecutionID] || !seen[reserved.ExecutionID] || !seen[unresolved.ExecutionID] {
		t.Fatalf("candidate IDs=%v; want only reserved and unresolved records", seen)
	}
	if !unresolvedFound {
		t.Fatal("unresolved candidate was not returned")
	}
	_, _, otherTenantCtx := authenticatedContext(t, agent.PermissionReconcile)
	otherTenantPage, err := journal.ListReconciliationCandidates(otherTenantCtx, nil, 10)
	if err != nil || len(otherTenantPage.Items) != 0 {
		t.Fatalf("another tenant got reconciliation candidates=%+v err=%v", otherTenantPage.Items, err)
	}
}

func TestListReconciliationCandidatesRequiresPermissionAndValidBounds(t *testing.T) {
	t.Parallel()
	db := postgrestest.NewMigrated(t, agentpostgres.Migrations())
	journal, err := agentpostgres.NewJournal(db)
	if err != nil {
		t.Fatal(err)
	}
	_, _, allowedCtx := authenticatedContext(t, agent.PermissionReconcile)
	_, _, deniedCtx := authenticatedContext(t)
	if _, err := journal.ListReconciliationCandidates(deniedCtx, nil, 10); !errors.Is(err, agentpostgres.ErrReconciliationForbidden) {
		t.Fatalf("missing permission error=%v, want ErrReconciliationForbidden", err)
	}
	if _, err := journal.ListReconciliationCandidates(context.Background(), nil, 10); !errors.Is(err, auth.ErrCredentialsRequired) {
		t.Fatalf("missing authentication error=%v, want ErrCredentialsRequired", err)
	}
	for _, limit := range []int{-1, agentpostgres.MaxReconciliationPageSize + 1} {
		if _, err := journal.ListReconciliationCandidates(allowedCtx, nil, limit); err == nil {
			t.Errorf("limit %d was accepted", limit)
		}
	}
	foreignTenant, _ := tenancy.New(uuid.MustNew())
	foreignCursor := &agentpostgres.ReconciliationCursor{TenantID: foreignTenant.ID, CreatedAt: time.Now(), ExecutionID: uuid.MustNew()}
	if _, err := journal.ListReconciliationCandidates(allowedCtx, foreignCursor, 10); !errors.Is(err, agentpostgres.ErrInvalidReconciliation) {
		t.Fatalf("foreign cursor error=%v, want ErrInvalidReconciliation", err)
	}
}

func TestResolveReconciliationIsAuthorizedTenantScopedAndOneTime(t *testing.T) {
	t.Parallel()
	db := postgrestest.NewMigrated(t, agentpostgres.Migrations())
	journal, err := agentpostgres.NewJournal(db)
	if err != nil {
		t.Fatal(err)
	}
	tenant, actor, ctx := authenticatedContext(t, agent.PermissionReconcile)
	unresolved := newAttempt(t, tenant.ID, actor.SubjectID(), "payments.capture")
	reserved := newAttempt(t, tenant.ID, actor.SubjectID(), "reports.read")
	for _, attempt := range []agent.Attempt{unresolved, reserved} {
		if created, err := journal.Begin(ctx, attempt); err != nil || !created {
			t.Fatalf("Begin(%s)=(%v,%v)", attempt.Action, created, err)
		}
	}
	if err := journal.Finish(ctx, unresolved.ExecutionID, unresolved.ActorID, agent.OutcomeUnresolved, "execution.unresolved"); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.ResolveReconciliation(ctx, reserved.ExecutionID, agentpostgres.ReconciliationEffectNotApplied); !errors.Is(err, agentpostgres.ErrNotReconciliationCandidate) {
		t.Fatalf("resolve still-reserved execution=%v, want ErrNotReconciliationCandidate", err)
	}
	if _, err := journal.ResolveReconciliation(ctx, unresolved.ExecutionID, "unknown"); !errors.Is(err, agentpostgres.ErrInvalidReconciliation) {
		t.Fatalf("resolve with invalid result=%v, want ErrInvalidReconciliation", err)
	}
	_, _, deniedCtx := authenticatedContext(t)
	if _, err := journal.ResolveReconciliation(deniedCtx, unresolved.ExecutionID, agentpostgres.ReconciliationEffectApplied); !errors.Is(err, agentpostgres.ErrReconciliationForbidden) {
		t.Fatalf("resolve without permission=%v, want ErrReconciliationForbidden", err)
	}
	_, _, otherTenantCtx := authenticatedContext(t, agent.PermissionReconcile)
	if _, err := journal.ResolveReconciliation(otherTenantCtx, unresolved.ExecutionID, agentpostgres.ReconciliationEffectApplied); !errors.Is(err, agentpostgres.ErrNotReconciliationCandidate) {
		t.Fatalf("cross-tenant resolve=%v, want ErrNotReconciliationCandidate", err)
	}

	results := make(chan error, 2)
	var group sync.WaitGroup
	for _, result := range []agentpostgres.ReconciliationResult{
		agentpostgres.ReconciliationEffectApplied,
		agentpostgres.ReconciliationEffectNotApplied,
	} {
		group.Add(1)
		go func(result agentpostgres.ReconciliationResult) {
			defer group.Done()
			_, err := journal.ResolveReconciliation(ctx, unresolved.ExecutionID, result)
			results <- err
		}(result)
	}
	group.Wait()
	close(results)
	succeeded, alreadyResolved := 0, 0
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, agentpostgres.ErrAlreadyReconciled):
			alreadyResolved++
		default:
			t.Fatalf("concurrent reconciliation returned unexpected error: %v", err)
		}
	}
	if succeeded != 1 || alreadyResolved != 1 {
		t.Fatalf("concurrent resolutions: succeeded=%d already-resolved=%d, want one each", succeeded, alreadyResolved)
	}

	var reconciledBy uuid.UUID
	var result string
	if err := db.InTenantTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT reconciled_by, result FROM forge_agent_reconciliations
			WHERE tenant_id = $1 AND execution_id = $2
		`, tenant.ID, unresolved.ExecutionID).Scan(&reconciledBy, &result)
	}); err != nil {
		t.Fatal(err)
	}
	if reconciledBy != actor.SubjectID() || (result != string(agentpostgres.ReconciliationEffectApplied) && result != string(agentpostgres.ReconciliationEffectNotApplied)) {
		t.Fatalf("persisted reconciler=%s result=%q", reconciledBy, result)
	}
	page, err := journal.ListReconciliationCandidates(ctx, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].ExecutionID != reserved.ExecutionID {
		t.Fatalf("pending candidates after resolution=%+v, want only still-reserved record", page.Items)
	}
}

func newAttempt(t *testing.T, tenantID, actorID uuid.UUID, action string) agent.Attempt {
	t.Helper()
	return agent.Attempt{
		ExecutionID: uuid.MustNew(), IntentID: uuid.MustNew(), ActorID: actorID,
		TenantID: tenantID, Action: action, Effect: agent.EffectMutation,
	}
}
