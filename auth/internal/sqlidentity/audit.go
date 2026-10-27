package sqlidentity

import (
	"context"

	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/uuid"
	"github.com/rocketinsidecorp/forge-go/web"
)

var _ auth.SecurityAuditor = (*Repository)(nil)

// RecordSecurityEvent stores event in forge_security_audit_events after
// validating it. It implements auth.SecurityAuditor.
func (r *Repository) RecordSecurityEvent(ctx context.Context, event auth.SecurityEvent) error {
	return r.recordSecurityEvent(ctx, r.db, event)
}

// recordSecurityEvent writes event through db, which may be the transaction
// whose change it records.
func (r *Repository) recordSecurityEvent(ctx context.Context, db db, event auth.SecurityEvent) error {
	if err := event.Validate(); err != nil {
		return err
	}
	// IDs come from the application (UUIDv7), not a database default.
	id, err := uuid.New()
	if err != nil {
		return err
	}
	occurredAt, args := "{now}", []any{
		id,
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
	if !event.OccurredAt.IsZero() {
		occurredAt, args = "?", append(args, r.dialect.Time(event.OccurredAt))
	}
	_, err = db.ExecContext(ctx, r.q(`
		INSERT INTO forge_security_audit_events
			(id, kind, outcome, actor_id, subject_id, membership_id,
			 account_digest, source_digest, credential_digest, request_id, occurred_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, `+occurredAt+`)
	`), args...)
	return r.translate(err)
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
