package postgres_test

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/auth"
	authpostgres "github.com/rocketinsidecorp/forge-go/auth/postgres"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"
	"github.com/rocketinsidecorp/forge-go/uuid"
	"github.com/rocketinsidecorp/forge-go/web"
)

func TestSecurityAuditPersistsStructuredEvent(t *testing.T) {
	db := postgrestest.NewMigrated(t, authpostgres.Migrations())
	repo, err := authpostgres.New(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := web.WithRequestID(context.Background(), "req-audit-1")
	subjectID := uuid.MustNew()
	membershipID := uuid.MustNew()
	accountDigest := auth.Digest(sha256.Sum256([]byte("account-key")))
	sourceDigest := auth.Digest(sha256.Sum256([]byte("source-key")))
	credentialDigest := auth.Digest(sha256.Sum256([]byte("credential-key")))
	occurredAt := time.Date(2026, 9, 28, 0, 45, 0, 0, time.UTC)

	err = repo.RecordSecurityEvent(ctx, auth.SecurityEvent{
		Kind:             auth.SecurityLoginSucceeded,
		Outcome:          auth.SecurityOutcomeSucceeded,
		SubjectID:        subjectID,
		MembershipID:     membershipID,
		AccountDigest:    accountDigest,
		SourceDigest:     sourceDigest,
		CredentialDigest: credentialDigest,
		OccurredAt:       occurredAt,
	})
	if err != nil {
		t.Fatal(err)
	}

	var (
		kind, outcome, requestID             string
		gotSubject, gotMembership            uuid.UUID
		gotAccount, gotSource, gotCredential []byte
		gotOccurred                          time.Time
	)
	if err := db.QueryRow(ctx, `
		SELECT kind, outcome, subject_id, membership_id,
		       account_digest, source_digest, credential_digest, request_id, occurred_at
		FROM forge_security_audit_events
	`).Scan(
		&kind, &outcome, &gotSubject, &gotMembership,
		&gotAccount, &gotSource, &gotCredential, &requestID, &gotOccurred,
	); err != nil {
		t.Fatal(err)
	}
	if kind != string(auth.SecurityLoginSucceeded) || outcome != string(auth.SecurityOutcomeSucceeded) {
		t.Fatalf("kind=%q outcome=%q", kind, outcome)
	}
	if gotSubject != subjectID || gotMembership != membershipID {
		t.Fatalf("subject=%s membership=%s", gotSubject, gotMembership)
	}
	if string(gotAccount) != string(accountDigest[:]) ||
		string(gotSource) != string(sourceDigest[:]) ||
		string(gotCredential) != string(credentialDigest[:]) {
		t.Fatalf("account=%x source=%x credential=%x", gotAccount, gotSource, gotCredential)
	}
	if requestID != "req-audit-1" || !gotOccurred.Equal(occurredAt) {
		t.Fatalf("request_id=%q occurred_at=%v", requestID, gotOccurred)
	}
}

func TestSecurityAuditUsesDatabaseClockByDefault(t *testing.T) {
	db := postgrestest.NewMigrated(t, authpostgres.Migrations())
	repo, err := authpostgres.New(db)
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now().UTC().Add(-time.Second)
	if err := repo.RecordSecurityEvent(context.Background(), auth.SecurityEvent{
		Kind:    auth.SecuritySessionsRevoked,
		Outcome: auth.SecurityOutcomeSucceeded,
		ActorID: uuid.MustNew(),
	}); err != nil {
		t.Fatal(err)
	}
	after := time.Now().UTC().Add(time.Second)

	var occurredAt time.Time
	if err := db.QueryRow(context.Background(), `
		SELECT occurred_at FROM forge_security_audit_events
	`).Scan(&occurredAt); err != nil {
		t.Fatal(err)
	}
	if occurredAt.Before(before) || occurredAt.After(after) {
		t.Fatalf("occurred_at=%v not in [%v,%v]", occurredAt, before, after)
	}
}

func TestSecurityAuditRejectsInvalidEventBeforeInsert(t *testing.T) {
	db := postgrestest.NewMigrated(t, authpostgres.Migrations())
	repo, err := authpostgres.New(db)
	if err != nil {
		t.Fatal(err)
	}
	err = repo.RecordSecurityEvent(context.Background(), auth.SecurityEvent{
		Kind:    "invalid kind",
		Outcome: auth.SecurityOutcomeSucceeded,
	})
	if err == nil {
		t.Fatal("invalid event was accepted")
	}
	var count int
	if err := db.QueryRow(context.Background(), "SELECT count(*) FROM forge_security_audit_events").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("audit count=%d", count)
	}
}
