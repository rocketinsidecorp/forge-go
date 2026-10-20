package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/rocketinsidecorp/forge-go/encryption"
	"github.com/rocketinsidecorp/forge-go/tenancy"
)

// EncryptedColumnOption configures EncryptedColumn.
type EncryptedColumnOption func(*encryptedColumn)

// WithTenant scopes every page read and compare-and-set update of an
// EncryptedColumn to tenant: each runs in its own transaction that first
// installs the tenant as forge.tenant_id (set_config(..., true), as InTenantTx
// does), so the table's row-level security policies show and accept that
// tenant's rows only. Rotate a tenant table once per tenant. db must then be a
// *DB or a pgx.Tx; inside a pgx.Tx each step is a savepoint and the setting
// stays for the rest of that transaction.
func WithTenant(tenant tenancy.Tenant) EncryptedColumnOption {
	return func(c *encryptedColumn) { c.tenant = &tenant }
}

// EncryptedColumn returns the encryption.Rewriter for one encrypted TEXT
// column, used by Cipher.Reencrypt to move a table to the active key. The
// primary key must be a uuid or text column (UUIDv7 in Forge applications);
// other types, such as bigint, are refused with an error naming the
// requirement. The spec's placeholder style is always PostgreSQL's.
//
// A table under row-level security (the generated tenant tables: ENABLE and
// FORCE ROW LEVEL SECURITY on forge.tenant_id, read by NOBYPASSRLS roles) is
// rotated one tenant at a time with WithTenant. Without a tenant scope every
// page first checks the catalog and fails with encryption.ErrConfiguration
// when the relation is missing or is not a table (a view hides the security
// of the table under it), or when its policies apply to the connected role,
// instead of seeing no row and reporting an empty, successful rotation.
func EncryptedColumn(db DBTX, spec encryption.SQLColumn, opts ...EncryptedColumnOption) (encryption.Rewriter, error) {
	if db == nil {
		return nil, errors.New("postgres: database is required")
	}
	spec.DollarPlaceholders = true
	spec, err := spec.Validate()
	if err != nil {
		return nil, err
	}
	column := encryptedColumn{db: db, spec: spec}
	for _, option := range opts {
		if option != nil {
			option(&column)
		}
	}
	if column.tenant != nil {
		if _, err := tenancy.New(column.tenant.ID); err != nil {
			return nil, err
		}
		switch db.(type) {
		case *DB, txBeginner:
		default:
			return nil, errors.New("postgres: a tenant-scoped EncryptedColumn needs a *postgres.DB or a pgx.Tx to open its transactions")
		}
	}
	return column, nil
}

// txBeginner is satisfied by pgx.Tx (a savepoint), *pgx.Conn and
// *pgxpool.Pool.
type txBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

type encryptedColumn struct {
	db   DBTX
	spec encryption.SQLColumn
	// tenant is the WithTenant scope; nil runs unscoped behind the
	// row-level security check.
	tenant *tenancy.Tenant
}

func (c encryptedColumn) Table() string  { return c.spec.Table }
func (c encryptedColumn) Column() string { return c.spec.Column }

func (c encryptedColumn) Page(ctx context.Context, after string, limit int) ([]encryption.Record, error) {
	var page []encryption.Record
	read := func(q DBTX) error {
		var err error
		page, err = c.readPage(ctx, q, after, limit)
		return err
	}
	if c.tenant != nil {
		return page, c.inTenantTx(ctx, read)
	}
	if err := c.refuseRowSecurity(ctx); err != nil {
		return nil, err
	}
	return page, read(c.db)
}

func (c encryptedColumn) readPage(ctx context.Context, q DBTX, after string, limit int) ([]encryption.Record, error) {
	var args []any
	if after != "" {
		args = append(args, after)
	}
	rows, err := q.Query(ctx, c.spec.PageSQL(limit, after != ""), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var page []encryption.Record
	for rows.Next() {
		var record encryption.Record
		if err := rows.Scan(&record.ID, &record.Sealed); err != nil {
			return nil, fmt.Errorf("postgres: the primary key and the encrypted column must be uuid or text columns: %w", err)
		}
		page = append(page, record)
	}
	return page, rows.Err()
}

func (c encryptedColumn) Replace(ctx context.Context, u encryption.Update) (bool, error) {
	query, err := c.spec.ReplaceSQL(u.Index != "")
	if err != nil {
		return false, err
	}
	args := []any{u.NewSealed}
	if u.Index != "" {
		args = append(args, u.Index)
	}
	args = append(args, u.ID, u.OldSealed)
	var changed bool
	update := func(q DBTX) error {
		tag, err := q.Exec(ctx, query, args...)
		if err != nil {
			return err
		}
		changed = tag.RowsAffected() > 0
		return nil
	}
	if c.tenant != nil {
		err = c.inTenantTx(ctx, update)
	} else {
		err = update(c.db)
	}
	return changed, err
}

// inTenantTx runs fn in a transaction scoped to c.tenant.
func (c encryptedColumn) inTenantTx(ctx context.Context, fn func(DBTX) error) error {
	scoped := func(tx pgx.Tx) error {
		if err := setTransactionTenant(ctx, tx, *c.tenant); err != nil {
			return err
		}
		return fn(tx)
	}
	switch db := c.db.(type) {
	case *DB:
		return db.InTx(ctx, scoped)
	case txBeginner:
		return pgx.BeginFunc(ctx, db, scoped)
	default:
		// EncryptedColumn refuses this combination.
		return errors.New("postgres: a tenant-scoped EncryptedColumn needs a *postgres.DB or a pgx.Tx")
	}
}

// rowSecurity is what the catalog says about one table and the role running
// a rotation.
type rowSecurity struct {
	enabled   bool // pg_class.relrowsecurity: ENABLE ROW LEVEL SECURITY
	forced    bool // pg_class.relforcerowsecurity: FORCE also binds the owner
	owner     bool // current_user has the privileges of the table's owner
	superuser bool // pg_roles.rolsuper of current_user
	bypass    bool // pg_roles.rolbypassrls of current_user
}

// applies reports whether the policies filter the rows this role sees:
// superusers and BYPASSRLS roles never see them applied, owners only when
// forced.
func (s rowSecurity) applies() bool {
	return s.enabled && !s.superuser && !s.bypass && (s.forced || !s.owner)
}

const rowSecuritySQL = `SELECT c.relkind IN ('r', 'p'), c.relrowsecurity, c.relforcerowsecurity,
       pg_has_role(current_user, c.relowner, 'USAGE'), r.rolsuper, r.rolbypassrls
FROM pg_class c JOIN pg_roles r ON r.rolname = current_user
WHERE c.oid = to_regclass($1)`

// refuseRowSecurity fails closed when row-level security would hide rows
// from an unscoped rotation, or when the relation is not a table.
func (c encryptedColumn) refuseRowSecurity(ctx context.Context) error {
	table := c.spec.Table
	var plainTable bool
	var security rowSecurity
	err := c.db.QueryRow(ctx, rowSecuritySQL, table).Scan(&plainTable,
		&security.enabled, &security.forced, &security.owner, &security.superuser, &security.bypass)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: table %s does not exist on the search path", encryption.ErrConfiguration, table)
	}
	if err != nil {
		return fmt.Errorf("postgres: check row-level security of %s: %w", table, err)
	}
	// A view, or another relation kind, carries no row-level security flags
	// of its own while the tables under it may still hide rows: only a table
	// proves what a rotation sees.
	if !plainTable {
		return fmt.Errorf("%w: %s is not a table: rotate the table that stores the column", encryption.ErrConfiguration, table)
	}
	if security.applies() {
		return fmt.Errorf("%w: table %s has row-level security for this database role, which sees no row "+
			"without a tenant scope: rotate it once per tenant with "+
			"postgres.EncryptedColumn(db, spec, postgres.WithTenant(tenant)), or as a role that bypasses "+
			"row-level security (see docs/ENCRYPTION.md)", encryption.ErrConfiguration, table)
	}
	return nil
}
