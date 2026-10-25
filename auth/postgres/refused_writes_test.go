package postgres_test

import (
	"context"
	"testing"
)

// refusingTables are the identity tables the operations write.
var refusingTables = []string{
	"forge_users", "forge_memberships", "forge_sessions", "forge_login_throttle",
	"forge_password_recovery", "forge_totp_enrollments", "forge_totp_factors",
	"forge_mfa_challenges", "forge_mfa_backup_codes", "forge_security_audit_events",
}

// TestIdentityOperationsFailClosedWhenOneWriteIsRefused makes the database
// refuse one kind of statement (INSERT, UPDATE or DELETE) on one table, as a
// constraint, a trigger or a revoked privilege would, and runs every
// operation. The statements before it have already run inside the
// operation's transaction, so this is where a missing rollback shows: an
// operation that fails must say so and leave every table as it was.
// (TestIdentityOperationsFailClosedWhenATableIsUnavailable breaks the first
// statement that needs a table; this breaks each statement in turn.)
func TestIdentityOperationsFailClosedWhenOneWriteIsRefused(t *testing.T) {
	for _, op := range failureOperations() {
		t.Run(op.name, func(t *testing.T) {
			t.Parallel()
			w := newFailureWorld(t)
			ctx := context.Background()
			if _, err := w.db.Exec(ctx, `
				CREATE FUNCTION forge_test_refuse() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN RAISE EXCEPTION 'write refused by the test' USING ERRCODE = 'XX000'; END $$`); err != nil {
				t.Fatal(err)
			}
			untouched := w.snapshot()
			if err := op.run(ctx, w); err != nil {
				t.Fatalf("the operation fails on a healthy database: %v", err)
			}
			if w.snapshot() == untouched {
				t.Skip("a read-only operation has no write to refuse")
			}
			w.reset()
			failed := 0
			dirty := false
			for _, table := range refusingTables {
				for _, event := range []string{"INSERT", "UPDATE", "DELETE"} {
					if dirty {
						w.reset()
						dirty = false
					}
					before := w.snapshot()
					// A statement-level trigger fires even when the statement
					// would have matched no row.
					if _, err := w.db.Exec(ctx, "CREATE TRIGGER forge_test_refuse BEFORE "+event+" ON "+table+
						" FOR EACH STATEMENT EXECUTE FUNCTION forge_test_refuse()"); err != nil {
						t.Fatal(err)
					}
					err := op.run(ctx, w)
					if _, dropErr := w.db.Exec(ctx, "DROP TRIGGER forge_test_refuse ON "+table); dropErr != nil {
						t.Fatal(dropErr)
					}
					if err == nil {
						dirty = true // the operation never runs such a statement
						continue
					}
					failed++
					if op.partial {
						dirty = true
						continue
					}
					if after := w.snapshot(); after != before {
						t.Errorf("%s on %s refused: the operation failed (%v) but changed the database", event, table, err)
						dirty = true
					}
				}
			}
			if failed == 0 {
				t.Error("no refused write made the operation fail: the harness is not exercising it")
			}
		})
	}
}
