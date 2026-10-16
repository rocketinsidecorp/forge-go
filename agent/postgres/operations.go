package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/rocketinsidecorp/forge-go/agent"
	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/tenancy"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

const MaxReconciliationPageSize = 100

var (
	ErrReconciliationForbidden    = errors.New("agent reconciliation permission denied")
	ErrInvalidReconciliation      = errors.New("agent reconciliation request is invalid")
	ErrNotReconciliationCandidate = errors.New("agent execution is not awaiting reconciliation")
	ErrAlreadyReconciled          = errors.New("agent execution was already reconciled")
)

type ReconciliationResult string

const (
	ReconciliationEffectApplied    ReconciliationResult = "effect_applied"
	ReconciliationEffectNotApplied ReconciliationResult = "effect_not_applied"
)

// ReconciliationCandidate contains journal metadata only. It deliberately
// excludes action inputs, outputs, prompts, and arbitrary executor errors.
type ReconciliationCandidate struct {
	ExecutionID uuid.UUID
	IntentID    uuid.UUID
	ActorID     uuid.UUID
	Action      string
	Effect      agent.Effect
	Status      string
	FailureCode string
	CreatedAt   time.Time
	CompletedAt *time.Time
}

// ReconciliationCursor continues paging within the cursor's tenant ordered
// by creation time and execution ID. It is not a secret or authorization token.
type ReconciliationCursor struct {
	TenantID    uuid.UUID
	CreatedAt   time.Time
	ExecutionID uuid.UUID
}

// ReconciliationPage contains one bounded page and a cursor only when more
// reserved or unresolved executions exist.
type ReconciliationPage struct {
	Items []ReconciliationCandidate
	Next  *ReconciliationCursor
}

// Reconciliation is a one-time operator decision about an uncertain action
// effect. It records no free-form note or application payload.
type Reconciliation struct {
	ID           uuid.UUID
	ExecutionID  uuid.UUID
	ReconciledBy uuid.UUID
	Result       ReconciliationResult
	CreatedAt    time.Time
}

// ListReconciliationCandidates returns tenant-scoped metadata for executions
// whose effects may require operator reconciliation. Callers need the
// authenticated agent:reconcile permission. A zero limit uses 25; larger than
// 100 is rejected. This method does not mark candidates as reconciled.
func (j *Journal) ListReconciliationCandidates(ctx context.Context, after *ReconciliationCursor, limit int) (ReconciliationPage, error) {
	if ctx == nil {
		return ReconciliationPage{}, errors.New("agent reconciliation context is required")
	}
	tenant, _, err := reconciliationAuthority(ctx)
	if err != nil {
		return ReconciliationPage{}, err
	}
	if limit == 0 {
		limit = 25
	}
	if limit < 1 || limit > MaxReconciliationPageSize {
		return ReconciliationPage{}, fmt.Errorf("agent reconciliation page size must be between 1 and %d", MaxReconciliationPageSize)
	}
	if after != nil && (after.TenantID != tenant.ID || after.CreatedAt.IsZero() || after.ExecutionID.Version() != 7 || after.ExecutionID.Variant() != 2) {
		return ReconciliationPage{}, ErrInvalidReconciliation
	}

	page := ReconciliationPage{Items: make([]ReconciliationCandidate, 0, limit)}
	err = j.db.InTenantTx(ctx, func(tx pgx.Tx) error {
		query := `
			SELECT execution_id, intent_id, actor_id, action, effect, status,
			       failure_code, created_at, completed_at
			FROM forge_agent_executions
			WHERE tenant_id = $1 AND status IN ('reserved', 'unresolved')
			  AND NOT EXISTS (
				  SELECT 1 FROM forge_agent_reconciliations r
				  WHERE r.tenant_id = forge_agent_executions.tenant_id
				    AND r.execution_id = forge_agent_executions.execution_id
			  )
			ORDER BY created_at, execution_id
			LIMIT $2
		`
		args := []any{tenant.ID, limit + 1}
		if after != nil {
			query = `
				SELECT execution_id, intent_id, actor_id, action, effect, status,
				       failure_code, created_at, completed_at
				FROM forge_agent_executions
				WHERE tenant_id = $1 AND status IN ('reserved', 'unresolved')
				  AND NOT EXISTS (
					  SELECT 1 FROM forge_agent_reconciliations r
					  WHERE r.tenant_id = forge_agent_executions.tenant_id
					    AND r.execution_id = forge_agent_executions.execution_id
				  )
				  AND (created_at, execution_id) > ($2, $3)
				ORDER BY created_at, execution_id
				LIMIT $4
			`
			args = []any{tenant.ID, after.CreatedAt, after.ExecutionID, limit + 1}
		}
		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()

		candidates := make([]ReconciliationCandidate, 0, limit+1)
		for rows.Next() {
			var candidate ReconciliationCandidate
			var failureCode pgtype.Text
			var completedAt pgtype.Timestamptz
			if err := rows.Scan(
				&candidate.ExecutionID, &candidate.IntentID, &candidate.ActorID,
				&candidate.Action, &candidate.Effect, &candidate.Status,
				&failureCode, &candidate.CreatedAt, &completedAt,
			); err != nil {
				return err
			}
			if failureCode.Valid {
				candidate.FailureCode = failureCode.String
			}
			if completedAt.Valid {
				value := completedAt.Time
				candidate.CompletedAt = &value
			}
			candidates = append(candidates, candidate)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(candidates) > limit {
			candidates = candidates[:limit]
			last := candidates[len(candidates)-1]
			page.Next = &ReconciliationCursor{TenantID: tenant.ID, CreatedAt: last.CreatedAt, ExecutionID: last.ExecutionID}
		}
		page.Items = candidates
		return nil
	})
	if err != nil {
		return ReconciliationPage{}, fmt.Errorf("list agent reconciliation candidates: %w", err)
	}
	return page, nil
}

// ResolveReconciliation appends one operator decision for an unresolved
// execution. Reserved executions are not eligible because their action may
// still be running. This method never retries the action or rewrites its
// journal status. Only the authenticated tenant principal with
// agent:reconcile may decide, and an execution can be resolved only once.
func (j *Journal) ResolveReconciliation(ctx context.Context, executionID uuid.UUID, result ReconciliationResult) (Reconciliation, error) {
	tenant, principal, err := reconciliationAuthority(ctx)
	if err != nil {
		return Reconciliation{}, err
	}
	if executionID.Version() != 7 || executionID.Variant() != 2 ||
		(result != ReconciliationEffectApplied && result != ReconciliationEffectNotApplied) {
		return Reconciliation{}, ErrInvalidReconciliation
	}
	reconciliationID, err := uuid.New()
	if err != nil {
		return Reconciliation{}, fmt.Errorf("create agent reconciliation ID: %w", err)
	}
	reconciliation := Reconciliation{
		ID: reconciliationID, ExecutionID: executionID,
		ReconciledBy: principal.SubjectID(), Result: result,
	}
	err = j.db.InTenantTx(ctx, func(tx pgx.Tx) error {
		var status string
		err := tx.QueryRow(ctx, `
			SELECT status FROM forge_agent_executions
			WHERE tenant_id = $1 AND execution_id = $2
			FOR UPDATE
		`, tenant.ID, executionID).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotReconciliationCandidate
		}
		if err != nil {
			return err
		}
		if status != "unresolved" {
			return ErrNotReconciliationCandidate
		}
		var exists bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM forge_agent_reconciliations
				WHERE tenant_id = $1 AND execution_id = $2
			)
		`, tenant.ID, executionID).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return ErrAlreadyReconciled
		}
		return tx.QueryRow(ctx, `
			INSERT INTO forge_agent_reconciliations
				(reconciliation_id, execution_id, tenant_id, reconciled_by, result)
			VALUES ($1, $2, $3, $4, $5)
			RETURNING created_at
		`, reconciliation.ID, executionID, tenant.ID, principal.SubjectID(), string(result)).Scan(&reconciliation.CreatedAt)
	})
	if err != nil {
		return Reconciliation{}, fmt.Errorf("record agent reconciliation: %w", err)
	}
	return reconciliation, nil
}

func reconciliationAuthority(ctx context.Context) (tenancy.Tenant, auth.Principal, error) {
	if ctx == nil {
		return tenancy.Tenant{}, auth.Principal{}, errors.New("agent reconciliation context is required")
	}
	principal, authenticated := auth.FromContext(ctx)
	if !authenticated {
		return tenancy.Tenant{}, auth.Principal{}, auth.ErrCredentialsRequired
	}
	tenant, err := tenancy.Require(ctx)
	if err != nil {
		return tenancy.Tenant{}, auth.Principal{}, err
	}
	if principal.Tenant().ID != tenant.ID {
		return tenancy.Tenant{}, auth.Principal{}, ErrActorMismatch
	}
	if !principal.Can(agent.PermissionReconcile) {
		return tenancy.Tenant{}, auth.Principal{}, ErrReconciliationForbidden
	}
	return tenant, principal, nil
}
