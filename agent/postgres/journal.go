package postgres

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/jackc/pgx/v5"

	"github.com/rocketinsidecorp/forge-go/agent"
	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/postgres"
	"github.com/rocketinsidecorp/forge-go/tenancy"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

var (
	ErrExecutionNotStarted               = errors.New("agent execution is not pending")
	ErrActorMismatch                     = errors.New("agent journal actor does not match authenticated principal")
	failureCodePattern                   = regexp.MustCompile(`^[a-z][a-z0-9_.-]{2,63}$`)
	_                      agent.Journal = (*Journal)(nil)
)

type Journal struct {
	db *postgres.DB
}

func NewJournal(db *postgres.DB) (*Journal, error) {
	if db == nil {
		return nil, errors.New("agent PostgreSQL database is required")
	}
	return &Journal{db: db}, nil
}

func (j *Journal) Begin(ctx context.Context, attempt agent.Attempt) (bool, error) {
	if err := validateAttemptTenant(ctx, attempt, false); err != nil {
		return false, err
	}
	created := false
	err := j.db.InTenantTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO forge_agent_executions
				(execution_id, intent_id, actor_id, tenant_id, action, effect, status)
			VALUES ($1, $2, $3, $4, $5, $6, 'reserved')
			ON CONFLICT (tenant_id, actor_id, intent_id) DO NOTHING
		`, attempt.ExecutionID, attempt.IntentID, attempt.ActorID, attempt.TenantID,
			attempt.Action, int16(attempt.Effect))
		if err != nil {
			return postgres.Translate(err)
		}
		created = tag.RowsAffected() == 1
		return nil
	})
	return created, err
}

func (j *Journal) Reject(ctx context.Context, attempt agent.Attempt, failureCode string) error {
	if err := validateAttemptTenant(ctx, attempt, true); err != nil {
		return err
	}
	if !failureCodePattern.MatchString(failureCode) {
		return errors.New("agent failure code is invalid")
	}
	created := false
	err := j.db.InTenantTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO forge_agent_executions
				(execution_id, intent_id, actor_id, tenant_id, action, effect,
				 status, failure_code, completed_at)
			VALUES ($1, $2, $3, $4, $5, $6, 'denied', $7, clock_timestamp())
			ON CONFLICT (tenant_id, actor_id, intent_id) DO NOTHING
		`, attempt.ExecutionID, attempt.IntentID, attempt.ActorID, attempt.TenantID,
			attempt.Action, int16(attempt.Effect), failureCode)
		if err != nil {
			return postgres.Translate(err)
		}
		created = tag.RowsAffected() == 1
		return nil
	})
	if err != nil {
		return err
	}
	if !created {
		return agent.ErrIntentReplayed
	}
	return nil
}

func (j *Journal) Finish(ctx context.Context, executionID, actorID uuid.UUID, outcome agent.Outcome, failureCode string) error {
	if executionID.Version() != 7 || executionID.Variant() != 2 {
		return errors.New("agent execution ID must be a UUIDv7")
	}
	if actorID.Version() != 7 || actorID.Variant() != 2 {
		return errors.New("agent execution actor ID must be a UUIDv7")
	}
	principal, authenticated := auth.FromContext(ctx)
	if !authenticated || principal.SubjectID() != actorID {
		return ErrActorMismatch
	}
	if outcome != agent.OutcomeSucceeded && outcome != agent.OutcomeFailed && outcome != agent.OutcomeUnresolved {
		return errors.New("agent execution outcome is invalid")
	}
	if outcome == agent.OutcomeSucceeded && failureCode != "" ||
		outcome != agent.OutcomeSucceeded && !failureCodePattern.MatchString(failureCode) {
		return errors.New("agent execution failure code is invalid")
	}
	tenant, err := tenancy.Require(ctx)
	if err != nil {
		return err
	}
	if principal.Tenant().ID != tenant.ID {
		return ErrActorMismatch
	}
	updated := false
	err = j.db.InTenantTx(ctx, func(tx pgx.Tx) error {
		var code any
		if failureCode != "" {
			code = failureCode
		}
		tag, err := tx.Exec(ctx, `
			UPDATE forge_agent_executions
			SET status = $2, failure_code = $3, completed_at = clock_timestamp()
			WHERE execution_id = $1 AND tenant_id = $4 AND actor_id = $5 AND status = 'reserved'
		`, executionID, string(outcome), code, tenant.ID, actorID)
		if err != nil {
			return postgres.Translate(err)
		}
		updated = tag.RowsAffected() == 1
		return nil
	})
	if err != nil {
		return err
	}
	if !updated {
		return ErrExecutionNotStarted
	}
	return nil
}

func validateAttemptTenant(ctx context.Context, attempt agent.Attempt, allowUnknownEffect bool) error {
	if err := attempt.Validate(); err != nil {
		return err
	}
	if !allowUnknownEffect && attempt.Effect == agent.EffectUnknown {
		return errors.New("registered agent action must declare its effect")
	}
	tenant, err := tenancy.Require(ctx)
	if err != nil {
		return err
	}
	if tenant.ID != attempt.TenantID {
		return fmt.Errorf("agent journal tenant does not match the attempt")
	}
	principal, authenticated := auth.FromContext(ctx)
	if !authenticated || principal.SubjectID() != attempt.ActorID || principal.Tenant().ID != attempt.TenantID {
		return ErrActorMismatch
	}
	return nil
}
