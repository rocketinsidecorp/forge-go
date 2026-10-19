package postgres

import (
	"context"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/postgres"
	"github.com/rocketinsidecorp/forge-go/uuid"
	"github.com/rocketinsidecorp/forge-go/web"
)

var _ auth.SecurityAuditor = (*Repository)(nil)

func (r *Repository) RecordSecurityEvent(ctx context.Context, event auth.SecurityEvent) error {
	return recordSecurityEvent(ctx, r.db, event)
}

// execer is satisfied by *postgres.DB and pgx.Tx, so an event can be written
// inside the transaction whose change it records.
type execer interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

func recordSecurityEvent(ctx context.Context, db execer, event auth.SecurityEvent) error {
	if err := event.Validate(); err != nil {
		return err
	}

	args := []any{
		event.Kind,
		event.Outcome,
		nullableUUID(event.ActorID),
		nullableUUID(event.SubjectID),
		nullableUUID(event.MembershipID),
		nullableDigest(event.AccountDigest),
		nullableDigest(event.SourceDigest),
		nullableDigest(event.CredentialDigest),
		nullableString(web.RequestID(ctx)),
	}
	if event.OccurredAt.IsZero() {
		_, err := db.Exec(ctx, `
			INSERT INTO forge_security_audit_events
				(kind, outcome, actor_id, subject_id, membership_id,
				 account_digest, source_digest, credential_digest, request_id, occurred_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, clock_timestamp())
		`, args...)
		return postgres.Translate(err)
	}

	args = append(args, event.OccurredAt.UTC())
	_, err := db.Exec(ctx, `
		INSERT INTO forge_security_audit_events
			(kind, outcome, actor_id, subject_id, membership_id,
			 account_digest, source_digest, credential_digest, request_id, occurred_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, args...)
	return postgres.Translate(err)
}

func nullableUUID(id uuid.UUID) any {
	if id == (uuid.UUID{}) {
		return nil
	}
	return id
}

func nullableDigest(digest auth.Digest) any {
	if digest == (auth.Digest{}) {
		return nil
	}
	return digest[:]
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
