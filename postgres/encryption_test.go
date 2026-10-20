package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/encryption"
	"github.com/rocketinsidecorp/forge-go/postgres"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"
	"github.com/rocketinsidecorp/forge-go/tenancy"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

func ringCipher(t *testing.T, active byte, previous ...byte) *encryption.Cipher {
	t.Helper()
	key := func(fill byte) encryption.Key {
		m := make([]byte, encryption.KeySize)
		for i := range m {
			m[i] = fill
		}
		return encryption.Key{ID: "k" + string(rune('0'+fill)), Material: m}
	}
	var old []encryption.Key
	for _, fill := range previous {
		old = append(old, key(fill))
	}
	ring, err := encryption.NewKeyRing(key(active), old...)
	if err != nil {
		t.Fatal(err)
	}
	c, err := encryption.NewCipher(ring)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestEncryptedColumnRotation stores attributes through encryption.Value in a
// uuid-keyed table, finds a row by blind index, and rotates the column to a new
// key with the pgx rewriter.
func TestEncryptedColumnRotation(t *testing.T) {
	t.Parallel()
	db := postgrestest.New(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `CREATE TABLE people (id uuid PRIMARY KEY, ssn text, ssn_idx text)`); err != nil {
		t.Fatal(err)
	}
	before := ringCipher(t, 1)
	ids := make([]uuid.UUID, 7)
	for i := range ids {
		ids[i] = uuid.MustNew()
		ssn := encryption.NewValue[string](before, "people", "ssn").ForRecord(ids[i].String())
		ssn.Set("ssn-" + string(rune('a'+i)))
		index, err := before.BlindIndex("people", "ssn", []byte("ssn-"+string(rune('a'+i))))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(ctx, `INSERT INTO people (id, ssn, ssn_idx) VALUES ($1, $2, $3)`, ids[i], ssn, index); err != nil {
			t.Fatal(err)
		}
	}
	var raw string
	if err := db.QueryRow(ctx, `SELECT ssn FROM people WHERE id = $1`, ids[0]).Scan(&raw); err != nil || strings.Contains(raw, "ssn-a") || !strings.HasPrefix(raw, "fg1.k1.") {
		t.Fatalf("stored value %q, %v", raw, err)
	}
	// A ciphertext moved to another row is rejected on read.
	if _, err := db.Exec(ctx, `UPDATE people SET ssn = (SELECT ssn FROM people WHERE id = $1) WHERE id = $2`, ids[0], ids[1]); err != nil {
		t.Fatal(err)
	}
	swapped := encryption.NewValue[string](before, "people", "ssn")
	if err := db.QueryRow(ctx, `SELECT ssn FROM people WHERE id = $1`, ids[1]).Scan(swapped); err != nil {
		t.Fatal(err)
	}
	if _, _, err := swapped.ForRecord(ids[1].String()).Get(); err == nil {
		t.Fatal("a ciphertext swapped between rows was accepted")
	}
	// Restore row 1 so the rotation below has only valid rows.
	restored := encryption.NewValue[string](before, "people", "ssn").ForRecord(ids[1].String())
	restored.Set("ssn-b")
	if _, err := db.Exec(ctx, `UPDATE people SET ssn = $1 WHERE id = $2`, restored, ids[1]); err != nil {
		t.Fatal(err)
	}

	after := ringCipher(t, 2, 1)
	rewriter, err := postgres.EncryptedColumn(db, encryption.SQLColumn{Table: "people", Column: "ssn", IndexColumn: "ssn_idx"})
	if err != nil {
		t.Fatal(err)
	}
	report, err := after.Reencrypt(ctx, rewriter, encryption.RotateOptions{BatchSize: 3, BlindIndex: true})
	if err != nil || report.Rotated != 7 || report.Reindexed != 7 || report.Conflicts != 0 {
		t.Fatalf("report %+v, %v", report, err)
	}
	retired := ringCipher(t, 2) // the old key is no longer needed
	for i, id := range ids {
		var found string
		index, _ := retired.BlindIndex("people", "ssn", []byte("ssn-"+string(rune('a'+i))))
		if err := db.QueryRow(ctx, `SELECT id::text FROM people WHERE ssn_idx = $1`, index).Scan(&found); err != nil || found != id.String() {
			t.Fatalf("blind index lookup %d: %q %v", i, found, err)
		}
		v := encryption.NewValue[string](retired, "people", "ssn")
		if err := db.QueryRow(ctx, `SELECT ssn FROM people WHERE id = $1`, id).Scan(v); err != nil {
			t.Fatal(err)
		}
		if got, _, err := v.ForRecord(id.String()).Get(); err != nil || got != "ssn-"+string(rune('a'+i)) {
			t.Fatalf("row %d after rotation: %q %v", i, got, err)
		}
	}
}

// TestEncryptedColumnRotatesTenantTablesPerTenantAndRefusesUnscopedRuns
// reproduces issue #33: the generated tenant tables use ENABLE and FORCE ROW
// LEVEL SECURITY on forge.tenant_id and the generated roles are NOBYPASSRLS,
// so an unscoped rotation saw no row and reported success. It must be refused
// for the runtime role and for the (non-superuser) owner, a view must be
// refused, a WithTenant run must rotate that tenant only, and the count of
// rows not on the active key, as a role that sees every row, proves
// completion.
func TestEncryptedColumnRotatesTenantTablesPerTenantAndRefusesUnscopedRuns(t *testing.T) {
	t.Parallel()
	admin, cfg := postgrestest.NewWithConfig(t) // a superuser: bypasses RLS
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.MustNew().String(), "-", "")[:16]
	owner, ownerDB := loginRole(t, admin, cfg, "forge_enc_owner_"+suffix)
	runtime, runtimeDB := loginRole(t, admin, cfg, "forge_enc_runtime_"+suffix)
	if _, err := admin.Exec(ctx, `
		CREATE TABLE people (id uuid PRIMARY KEY, tenant_id uuid NOT NULL, ssn text, ssn_idx text);
		ALTER TABLE people ENABLE ROW LEVEL SECURITY;
		ALTER TABLE people FORCE ROW LEVEL SECURITY;
		CREATE POLICY people_tenant_isolation ON people
			USING (tenant_id = NULLIF(current_setting('forge.tenant_id', true), '')::uuid)
			WITH CHECK (tenant_id = NULLIF(current_setting('forge.tenant_id', true), '')::uuid);
		ALTER TABLE people OWNER TO `+owner+`;
		GRANT SELECT, INSERT, UPDATE ON people TO `+runtime+`;
		CREATE VIEW people_view AS SELECT * FROM people;
		ALTER VIEW people_view OWNER TO `+owner+`;
		GRANT SELECT, UPDATE ON people_view TO `+runtime); err != nil {
		t.Fatal(err)
	}

	const perTenant = 5
	old := ringCipher(t, 1)
	first, _ := tenancy.New(uuid.MustNew())
	second, _ := tenancy.New(uuid.MustNew())
	for _, tenant := range []tenancy.Tenant{first, second} {
		for n := range perTenant {
			id := uuid.MustNew()
			ssn := encryption.NewValue[string](old, "people", "ssn").ForRecord(id.String())
			ssn.Set("ssn-" + string(rune('a'+n)))
			if _, err := admin.Exec(ctx, `INSERT INTO people (id, tenant_id, ssn) VALUES ($1, $2, $3)`, id, tenant.ID, ssn); err != nil {
				t.Fatal(err)
			}
		}
	}
	rotating := ringCipher(t, 2, 1)
	// The proof of completion: rows not sealed with the active key, beside
	// count(*) so that a role that sees no row cannot pass for done.
	counts := func(db *postgres.DB, tenant *tenancy.Tenant) (stale, total int) {
		t.Helper()
		run := func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE ssn IS NOT NULL AND substr(ssn, 1, length($1)) <> $1), count(*) FROM people`,
				"fg1.k2.").Scan(&stale, &total)
		}
		var err error
		if tenant != nil {
			tenantCtx, _ := tenancy.WithContext(ctx, *tenant)
			err = db.InTenantTx(tenantCtx, run)
		} else {
			err = db.InTx(ctx, run)
		}
		if err != nil {
			t.Fatal(err)
		}
		return stale, total
	}
	values := func(tenant tenancy.Tenant) map[string]string {
		t.Helper()
		rows, err := admin.Query(ctx, `SELECT id::text, ssn FROM people WHERE tenant_id = $1`, tenant.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		found := map[string]string{}
		for rows.Next() {
			var id, ssn string
			if err := rows.Scan(&id, &ssn); err != nil {
				t.Fatal(err)
			}
			found[id] = ssn
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return found
	}
	spec := encryption.SQLColumn{Table: "people", Column: "ssn", IndexColumn: "ssn_idx"}
	if stale, total := counts(admin, nil); stale != 2*perTenant || total != 2*perTenant {
		t.Fatalf("seeded: %d stale of %d", stale, total)
	}
	// What the old rewriter scanned: the runtime role sees no row at all.
	if stale, total := counts(runtimeDB, nil); stale != 0 || total != 0 {
		t.Fatalf("unscoped runtime role sees %d stale of %d", stale, total)
	}

	// 1. Unscoped runs are refused before any write, for the runtime role and
	//    for the owner (FORCE binds it too), and so is a view.
	for name, db := range map[string]*postgres.DB{"runtime": runtimeDB, "owner": ownerDB} {
		for _, table := range []string{"people", "people_view"} {
			rewriter, err := postgres.EncryptedColumn(db, encryption.SQLColumn{Table: table, Column: "ssn"})
			if err != nil {
				t.Fatal(err)
			}
			report, err := rotating.Reencrypt(ctx, rewriter, encryption.RotateOptions{})
			if !errors.Is(err, encryption.ErrConfiguration) || report != (encryption.Report{}) {
				t.Fatalf("%s, %s: unscoped rotation was not refused: %+v %v", name, table, report, err)
			}
			want := "row-level security"
			if table == "people_view" {
				want = "not a table"
			}
			if !strings.Contains(err.Error(), want) || (table == "people" && !strings.Contains(err.Error(), "WithTenant")) {
				t.Fatalf("%s, %s: %v", name, table, err)
			}
		}
	}
	if stale, _ := counts(admin, nil); stale != 2*perTenant {
		t.Fatalf("a refused run wrote: %d stale", stale)
	}
	missing, _ := postgres.EncryptedColumn(runtimeDB, encryption.SQLColumn{Table: "nobody", Column: "ssn"})
	if _, err := rotating.Reencrypt(ctx, missing, encryption.RotateOptions{}); !errors.Is(err, encryption.ErrConfiguration) {
		t.Fatalf("missing table: %v", err)
	}

	// 2. Scoped to the first tenant: its rows rotate (pages of 2, with blind
	//    indexes), the second tenant's bytes are untouched.
	untouched := values(second)
	scoped, err := postgres.EncryptedColumn(runtimeDB, spec, postgres.WithTenant(first))
	if err != nil {
		t.Fatal(err)
	}
	report, err := rotating.Reencrypt(ctx, scoped, encryption.RotateOptions{BatchSize: 2, BlindIndex: true})
	if err != nil || report.Scanned != perTenant || report.Rotated != perTenant || report.Reindexed != perTenant || report.Conflicts != 0 {
		t.Fatalf("first tenant: %+v %v", report, err)
	}
	if got := values(second); len(got) != perTenant || !maps.Equal(got, untouched) {
		t.Fatalf("the second tenant's rows changed: %v -> %v", untouched, got)
	}

	// 3. The counts prove progress per tenant and completion overall;
	//    Scanned never does.
	if stale, total := counts(runtimeDB, &first); stale != 0 || total != perTenant {
		t.Fatalf("first tenant: %d stale of %d", stale, total)
	}
	if stale, _ := counts(runtimeDB, &second); stale != perTenant {
		t.Fatalf("second tenant: %d stale", stale)
	}
	if stale, _ := counts(admin, nil); stale != perTenant {
		t.Fatalf("overall: %d stale", stale)
	}
	// Inside a caller's transaction the steps are savepoints, and the
	// caller's own tenant scope is back after the rotation.
	err = runtimeDB.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT set_config('forge.tenant_id', $1, true)", first.ID.String()); err != nil {
			return err
		}
		inTx, err := postgres.EncryptedColumn(tx, spec, postgres.WithTenant(second))
		if err != nil {
			return err
		}
		if report, err = rotating.Reencrypt(ctx, inTx, encryption.RotateOptions{}); err != nil {
			return err
		}
		var scope string
		if err := tx.QueryRow(ctx, "SELECT current_setting('forge.tenant_id', true)").Scan(&scope); err != nil {
			return err
		}
		if scope != first.ID.String() {
			return fmt.Errorf("caller scope after rotation = %q, want %s", scope, first.ID)
		}
		return nil
	})
	if err != nil || report.Scanned != perTenant || report.Rotated != perTenant {
		t.Fatalf("second tenant: %+v %v", report, err)
	}
	if stale, total := counts(admin, nil); stale != 0 || total != 2*perTenant {
		t.Fatalf("overall after both tenants: %d stale of %d", stale, total)
	}

	// Only now may the old key go: the new key alone reads every row.
	retired := ringCipher(t, 2)
	for _, tenant := range []tenancy.Tenant{first, second} {
		for id, sealed := range values(tenant) {
			v := encryption.NewValue[string](retired, "people", "ssn")
			if err := v.Scan(sealed); err != nil {
				t.Fatal(err)
			}
			if plain, _, err := v.ForRecord(id).Get(); err != nil || !strings.HasPrefix(plain, "ssn-") {
				t.Fatalf("row %s: %q %v", id, plain, err)
			}
		}
	}

	// A tenant that is not a UUIDv7 is refused.
	if _, err := postgres.EncryptedColumn(runtimeDB, spec, postgres.WithTenant(tenancy.Tenant{})); !errors.Is(err, tenancy.ErrInvalid) {
		t.Fatalf("invalid tenant: %v", err)
	}
}

// loginRole creates a NOSUPERUSER NOBYPASSRLS login role and returns its
// quoted name and a pool connected as it to cfg's database. The role is
// dropped when the test ends.
func loginRole(t *testing.T, admin *postgres.DB, cfg config.Database, name string) (string, *postgres.DB) {
	t.Helper()
	ctx := context.Background()
	quoted := pgx.Identifier{name}.Sanitize()
	password := strings.ReplaceAll(uuid.MustNew().String(), "-", "")
	if _, err := admin.Exec(ctx, "CREATE ROLE "+quoted+" LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD '"+password+"'"); err != nil {
		t.Fatal(err)
	}
	// Runs after the role's pool closes and before the database is dropped.
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP OWNED BY "+quoted)
		_, _ = admin.Exec(context.Background(), "DROP ROLE "+quoted)
	})
	roleURL, err := url.Parse(cfg.URL.Reveal())
	if err != nil {
		t.Fatal("invalid test database URL") // not wrapped: contains a password
	}
	roleURL.User = url.UserPassword(name, password)
	roleCfg := cfg
	roleCfg.URL = config.NewSecret(roleURL.String())
	db, err := postgres.Open(ctx, roleCfg)
	if err != nil {
		t.Fatalf("connect as %s: %v", name, err)
	}
	t.Cleanup(db.Close)
	return quoted, db
}
