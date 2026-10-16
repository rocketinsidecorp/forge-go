package auth

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/rocketinsidecorp/forge-go/uuid"
)

type SecurityEventKind string

const (
	SecurityLoginSucceeded        SecurityEventKind = "auth.login.succeeded"
	SecurityLoginFailed           SecurityEventKind = "auth.login.failed"
	SecurityLoginThrottled        SecurityEventKind = "auth.login.throttled"
	SecuritySessionRevoked        SecurityEventKind = "auth.session.revoked"
	SecuritySessionsRevoked       SecurityEventKind = "auth.sessions.revoked"
	SecuritySessionRejected       SecurityEventKind = "auth.session.rejected"
	SecurityAuthorizationDenied   SecurityEventKind = "auth.authorization.denied"
	SecurityRecoveryRequested     SecurityEventKind = "auth.recovery.requested"
	SecurityPasswordReset         SecurityEventKind = "auth.password.reset"
	SecurityMFAEnrollmentStarted  SecurityEventKind = "auth.mfa.enrollment_started"
	SecurityMFAEnabled            SecurityEventKind = "auth.mfa.enabled"
	SecurityMFAChallengeFailed    SecurityEventKind = "auth.mfa.challenge_failed"
	SecurityMFAChallengeSucceeded SecurityEventKind = "auth.mfa.challenge_succeeded"
	SecurityMFABackupCodeUsed     SecurityEventKind = "auth.mfa.backup_code_used"
	SecurityMFARotationStarted    SecurityEventKind = "auth.mfa.rotation_started"
	SecurityMFARotated            SecurityEventKind = "auth.mfa.rotated"
	SecurityMFADisabled           SecurityEventKind = "auth.mfa.disabled"
)

type SecurityOutcome string

const (
	SecurityOutcomeSucceeded SecurityOutcome = "succeeded"
	SecurityOutcomeDenied    SecurityOutcome = "denied"
	SecurityOutcomeFailed    SecurityOutcome = "failed"
)

var securityEventKindPattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{2,95}$`)

// SecurityEvent is deliberately closed over identifiers and digests. It has no
// arbitrary metadata map, so normal audit calls cannot accidentally attach a
// password, bearer token, recovery link, TOTP seed, or backup code.
type SecurityEvent struct {
	Kind             SecurityEventKind
	Outcome          SecurityOutcome
	ActorID          uuid.UUID
	SubjectID        uuid.UUID
	MembershipID     uuid.UUID
	AccountDigest    Digest
	SourceDigest     Digest
	CredentialDigest Digest
	OccurredAt       time.Time
}

func (event SecurityEvent) Validate() error {
	if !securityEventKindPattern.MatchString(string(event.Kind)) {
		return fmt.Errorf("security event kind %q is invalid", event.Kind)
	}
	switch event.Outcome {
	case SecurityOutcomeSucceeded, SecurityOutcomeDenied, SecurityOutcomeFailed:
	default:
		return errors.New("security event outcome is invalid")
	}
	for name, id := range map[string]uuid.UUID{
		"actor":      event.ActorID,
		"subject":    event.SubjectID,
		"membership": event.MembershipID,
	} {
		if id == (uuid.UUID{}) {
			continue
		}
		if id.Version() != 7 || id.Variant() != 2 {
			return fmt.Errorf("security event %s ID must be a UUIDv7", name)
		}
	}
	return nil
}

type SecurityAuditError struct {
	Cause error
	// OperationApplied is true when the security-sensitive state transition
	// already committed and cannot safely be retried merely because audit
	// persistence failed.
	OperationApplied bool
}

func (e *SecurityAuditError) Error() string { return "security audit failed" }

func (e *SecurityAuditError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func securityAuditError(err error, operationApplied bool) error {
	if err == nil {
		return nil
	}
	return &SecurityAuditError{Cause: err, OperationApplied: operationApplied}
}

type SecurityAuditor interface {
	RecordSecurityEvent(ctx context.Context, event SecurityEvent) error
}

type SecurityAuditorFunc func(context.Context, SecurityEvent) error

func (fn SecurityAuditorFunc) RecordSecurityEvent(ctx context.Context, event SecurityEvent) error {
	if fn == nil {
		return errors.New("security auditor is nil")
	}
	return fn(ctx, event)
}
