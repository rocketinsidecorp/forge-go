package migrate

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"
)

// Table records applied migrations. Its columns use types every supported
// database understands; applied_at holds Unix microseconds written by the
// migrator, so no driver-specific time handling is involved.
const Table = "forge_schema_migrations"

const (
	createTable = `CREATE TABLE IF NOT EXISTS ` + Table + ` (
	version    BIGINT       PRIMARY KEY,
	name       VARCHAR(255) NOT NULL,
	checksum   CHAR(64)     NOT NULL,
	applied_at BIGINT       NOT NULL
)`
	lockPollMinDelay = 50 * time.Millisecond
	lockPollMaxDelay = time.Second
)

// Dialect adapts the engine to one database.
type Dialect interface {
	// Placeholder returns the bind parameter for the n-th argument, from 1.
	Placeholder(n int) string
	// PrepareSession configures the dedicated migration session, for
	// example lifting statement timeouts: migrations may legitimately run
	// long (index builds, backfills).
	PrepareSession(ctx context.Context, conn *sql.Conn) error
	// TryLock tries to take the cross-process migration lock without
	// waiting. It must not block inside the database: a blocked lock
	// statement keeps a snapshot open, and PostgreSQL's CREATE INDEX
	// CONCURRENTLY in the lock holder waits for it, deadlocking both.
	TryLock(ctx context.Context, conn *sql.Conn) (bool, error)
	// Unlock releases the lock taken by TryLock.
	Unlock(ctx context.Context, conn *sql.Conn) error
	// TableExists reports whether table exists in the current database.
	TableExists(ctx context.Context, conn *sql.Conn, table string) (bool, error)
}

// ScriptErrorExplainer is implemented by dialects that can make a failed
// migration script's error clearer, for example when the server is too old
// for a function the script calls. q is the migration session, outside any
// transaction; the result should wrap err.
type ScriptErrorExplainer interface {
	ExplainScriptError(ctx context.Context, q Queryer, err error) error
}

// Queryer runs a single-row query on a migration session or transaction.
type Queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// SessionIdentifier is implemented by dialects whose migrations can switch
// the session to another database or schema (MySQL's USE, PostgreSQL's
// search_path). The engine compares the identity before and after every
// script, so a migration cannot silently redirect the bookkeeping, which
// uses unqualified names, into another database.
type SessionIdentifier interface {
	SessionIdentity(ctx context.Context, q Queryer) (string, error)
}

type applied struct {
	name      string
	checksum  string
	appliedAt time.Time
}

// Migrator applies and reverts migrations. Every change holds the dialect's
// cross-process lock, so replicas starting at the same time migrate exactly
// once.
type Migrator struct {
	db         *sql.DB
	dialect    Dialect
	migrations []Migration
	logger     *slog.Logger
}

// New returns a migrator that owns db; Close closes it. db must be dedicated
// to migrations and must accept several statements in one Exec (MySQL needs
// multiStatements). New makes sure sessions are never reused, so the session
// settings a migration changes cannot leak anywhere. It refuses a migration
// that reuses a framework migration's version under another name
// (*ReservedVersionError).
func New(db *sql.DB, dialect Dialect, migrations []Migration, logger *slog.Logger) (*Migrator, error) {
	if db == nil || dialect == nil || logger == nil {
		return nil, errors.New("migrate: database, dialect and logger are required")
	}
	if err := checkReservedVersions(migrations); err != nil {
		return nil, err
	}
	db.SetMaxIdleConns(0)
	return &Migrator{db: db, dialect: dialect, migrations: slices.Clone(migrations), logger: logger}, nil
}

// Close closes the migrator's database.
func (m *Migrator) Close() error { return m.db.Close() }

// Up applies all pending migrations in version order and returns them. It
// refuses to run when an applied migration was modified or is unknown to this
// build, which usually means an older release is deploying over a newer schema.
// An applied framework migration whose set the build no longer composes (the
// feature was turned off) is not unknown: it stays applied and untouched.
func (m *Migrator) Up(ctx context.Context) ([]Migration, error) {
	var done []Migration
	err := m.withLock(ctx, func(conn *sql.Conn, identity string) error {
		state, err := m.loadApplied(ctx, conn)
		if err != nil {
			return err
		}
		if err := m.verify(state); err != nil {
			return err
		}
		insert := "INSERT INTO " + Table + " (version, name, checksum, applied_at) VALUES (" +
			m.dialect.Placeholder(1) + ", " + m.dialect.Placeholder(2) + ", " + m.dialect.Placeholder(3) + ", " + m.dialect.Placeholder(4) + ")"
		for _, migration := range m.migrations {
			if _, ok := state[migration.Version]; ok {
				continue
			}
			started := time.Now()
			record := func(ctx context.Context, db execer) error {
				_, err := db.ExecContext(ctx, insert, migration.Version, migration.Name, migration.Checksum, time.Now().UnixMicro())
				return err
			}
			if err := m.run(ctx, conn, identity, migration.Up, record); err != nil {
				return fmt.Errorf("apply migration %d_%s: %w", migration.Version, migration.Name, m.explain(ctx, conn, err))
			}
			m.logger.Info("applied migration", "version", migration.Version, "name", migration.Name, "duration", time.Since(started))
			done = append(done, migration)
		}
		return nil
	})
	return done, err
}

// Down is the application rollback: it reverts the steps most recently
// applied application migrations, last applied first. Framework migrations
// (FrameworkMigrations) are never reverted, even when they were applied
// after the application's; Down skips over them. When steps exceeds the
// applied application migrations, it reverts those there are. When only
// framework migrations remain applied it reverts nothing and returns a
// *FrameworkMigrationOnlyError; when nothing is applied it returns nothing.
// Every target is checked to be reversible before any is reverted.
func (m *Migrator) Down(ctx context.Context, steps int) ([]Migration, error) {
	return m.down(ctx, steps, scopeApplication)
}

// DownFramework reverts the steps most recently applied framework migrations
// that this migrator contains, last applied first, and never an application
// migration. It is the deliberate way to drop a framework table, for example
// after turning a feature off; no command exposes it. Framework migrations
// shipped without a down script (the job queue, rate limit, cache and outbox
// schemas) are irreversible and make it fail before anything is reverted.
func (m *Migrator) DownFramework(ctx context.Context, steps int) ([]Migration, error) {
	return m.down(ctx, steps, scopeFramework)
}

// revertScope selects which applied migrations a revert may touch.
type revertScope int

const (
	// scopeApplication is every version that is not a framework migration.
	scopeApplication revertScope = iota
	// scopeFramework is the framework migrations the migrator contains.
	scopeFramework
)

func (m *Migrator) down(ctx context.Context, steps int, scope revertScope) ([]Migration, error) {
	if steps < 1 {
		return nil, errors.New("steps must be at least 1")
	}
	var reverted []Migration
	err := m.withLock(ctx, func(conn *sql.Conn, identity string) error {
		state, err := m.loadApplied(ctx, conn)
		if err != nil {
			return err
		}
		if err := m.verify(state); err != nil {
			return err
		}
		// Most recently applied first, which differs from highest version
		// when migrations were applied out of order.
		known := make(map[int64]Migration, len(m.migrations))
		for _, migration := range m.migrations {
			known[migration.Version] = migration
		}
		versions := make([]int64, 0, len(state))
		for version := range state {
			versions = append(versions, version)
		}
		slices.SortFunc(versions, func(a, b int64) int {
			if byTime := state[b].appliedAt.Compare(state[a].appliedAt); byTime != 0 {
				return byTime
			}
			return cmp.Compare(b, a)
		})
		var targets []Migration
		var newestFramework *FrameworkMigration
		for _, version := range versions {
			framework, isFramework := frameworkMigration(version)
			if isFramework && newestFramework == nil {
				newestFramework = &framework
			}
			migration, contained := known[version]
			switch {
			case len(targets) == steps:
			case scope == scopeApplication && !isFramework:
				targets = append(targets, migration) // verify guarantees presence
			case scope == scopeFramework && isFramework && contained:
				targets = append(targets, migration)
			}
		}
		if len(targets) == 0 && scope == scopeApplication && newestFramework != nil {
			return &FrameworkMigrationOnlyError{Newest: *newestFramework}
		}
		for _, migration := range targets {
			if migration.Down.SQL == "" {
				return fmt.Errorf("migration %d_%s is irreversible: it has no down script", migration.Version, migration.Name)
			}
		}
		remove := "DELETE FROM " + Table + " WHERE version = " + m.dialect.Placeholder(1)
		for _, migration := range targets {
			record := func(ctx context.Context, db execer) error {
				_, err := db.ExecContext(ctx, remove, migration.Version)
				return err
			}
			if err := m.run(ctx, conn, identity, migration.Down, record); err != nil {
				return fmt.Errorf("revert migration %d_%s: %w", migration.Version, migration.Name, m.explain(ctx, conn, err))
			}
			m.logger.Info("reverted migration", "version", migration.Version, "name", migration.Name)
			reverted = append(reverted, migration)
		}
		return nil
	})
	return reverted, err
}

// Status reports every known and applied migration in version order. It is
// read-only: it takes no lock, so it answers while a long migration runs, and
// it treats a database that was never migrated as having everything pending.
func (m *Migrator) Status(ctx context.Context) ([]MigrationStatus, error) {
	conn, err := m.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("open migration session: %w", err)
	}
	defer func() { _ = conn.Close() }()
	tracked, err := m.dialect.TableExists(ctx, conn, Table)
	if err != nil {
		return nil, fmt.Errorf("read applied migrations: %w", err)
	}
	state := map[int64]applied{}
	if tracked {
		if state, err = m.loadApplied(ctx, conn); err != nil {
			return nil, err
		}
	}

	var statuses []MigrationStatus
	known := make(map[int64]bool, len(m.migrations))
	for _, migration := range m.migrations {
		known[migration.Version] = true
		status := MigrationStatus{Version: migration.Version, Name: migration.Name, State: StatePending}
		if row, ok := state[migration.Version]; ok {
			status.State, status.AppliedAt = StateApplied, row.appliedAt
			if row.checksum != migration.Checksum {
				status.State = StateModified
			}
		}
		statuses = append(statuses, status)
	}
	for version, row := range state {
		if !known[version] {
			rowState := StateUnknown
			if framework, ok := frameworkMigration(version); ok && framework.Name == row.name {
				rowState = StateDisabled
			}
			statuses = append(statuses, MigrationStatus{Version: version, Name: row.name, State: rowState, AppliedAt: row.appliedAt})
		}
	}
	slices.SortFunc(statuses, func(a, b MigrationStatus) int { return cmp.Compare(a.Version, b.Version) })
	return statuses, nil
}

func (m *Migrator) verify(state map[int64]applied) error {
	known := make(map[int64]Migration, len(m.migrations))
	for _, migration := range m.migrations {
		known[migration.Version] = migration
	}
	versions := make([]int64, 0, len(state))
	for version := range state {
		versions = append(versions, version)
	}
	slices.Sort(versions)
	for _, version := range versions {
		row := state[version]
		// Ownership is decided by version: a row recorded under a framework
		// version with another name was not written by that framework
		// migration, so it is neither tolerated nor protected.
		framework, reserved := frameworkMigration(version)
		if reserved && row.name != framework.Name {
			return &ReservedVersionError{Version: version, Name: row.name, Reserved: framework}
		}
		migration, ok := known[version]
		if !ok {
			if reserved {
				// Its feature was turned off: the table and the row stay,
				// and turning it on again finds it applied and verifies
				// its checksum.
				continue
			}
			return fmt.Errorf("database has migration %d_%s, which this build does not contain", version, row.name)
		}
		if row.checksum != migration.Checksum {
			return fmt.Errorf("migration %d_%s was modified after it was applied; add a new migration instead", version, migration.Name)
		}
	}
	return nil
}

// withLock runs fn on a dedicated session holding the migration lock. The
// session is closed afterwards (New disabled idle connections), which also
// releases the lock if Unlock could not run.
func (m *Migrator) withLock(ctx context.Context, fn func(conn *sql.Conn, identity string) error) (err error) {
	conn, err := m.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("open migration session: %w", err)
	}
	defer func() { err = errors.Join(err, conn.Close()) }()

	if err := m.dialect.PrepareSession(ctx, conn); err != nil {
		return fmt.Errorf("configure migration session: %w", err)
	}
	identity, err := m.sessionIdentity(ctx, conn)
	if err != nil {
		return fmt.Errorf("identify migration session: %w", err)
	}
	if err := m.acquireLock(ctx, conn); err != nil {
		return err
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lockPollMaxDelay*5)
		defer cancel()
		if unlockErr := m.dialect.Unlock(unlockCtx, conn); unlockErr != nil {
			err = errors.Join(err, fmt.Errorf("release migration lock: %w", unlockErr))
		}
	}()
	if _, err := conn.ExecContext(ctx, createTable); err != nil {
		return fmt.Errorf("create %s: %w", Table, err)
	}
	return fn(conn, identity)
}

// acquireLock polls TryLock with exponential backoff until it succeeds or
// ctx ends.
func (m *Migrator) acquireLock(ctx context.Context, conn *sql.Conn) error {
	delay := lockPollMinDelay
	for waited := false; ; waited = true {
		acquired, err := m.dialect.TryLock(ctx, conn)
		if err != nil {
			return fmt.Errorf("acquire migration lock: %w", err)
		}
		if acquired {
			return nil
		}
		if !waited {
			m.logger.Info("waiting for another process to finish migrating")
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("acquire migration lock: %w", ctx.Err())
		case <-timer.C:
		}
		delay = min(delay*2, lockPollMaxDelay)
	}
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// run executes a script and records it. Transactional scripts and their
// bookkeeping commit atomically where the database supports transactional
// DDL (PostgreSQL, SQLite); MySQL commits DDL implicitly. Scripts are sent
// without parameters, so a file may hold many statements.
func (m *Migrator) run(ctx context.Context, conn *sql.Conn, identity string, script Script, record func(context.Context, execer) error) error {
	if script.NoTx {
		if _, err := conn.ExecContext(ctx, script.SQL); err != nil {
			return err
		}
		if err := m.checkSession(ctx, conn, identity); err != nil {
			return err
		}
		return record(ctx, conn)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	if _, err := tx.ExecContext(ctx, script.SQL); err != nil {
		return err
	}
	if err := m.checkSession(ctx, tx, identity); err != nil {
		return err
	}
	if err := record(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

// explain lets the dialect clarify a failed script's error.
func (m *Migrator) explain(ctx context.Context, q Queryer, err error) error {
	if explainer, ok := m.dialect.(ScriptErrorExplainer); ok {
		return explainer.ExplainScriptError(ctx, q, err)
	}
	return err
}

func (m *Migrator) sessionIdentity(ctx context.Context, q Queryer) (string, error) {
	identifier, ok := m.dialect.(SessionIdentifier)
	if !ok {
		return "", nil
	}
	return identifier.SessionIdentity(ctx, q)
}

// checkSession fails when a script switched the session to another database
// or schema; a transactional script is then rolled back.
func (m *Migrator) checkSession(ctx context.Context, q Queryer, want string) error {
	identity, err := m.sessionIdentity(ctx, q)
	if err != nil {
		return fmt.Errorf("identify migration session: %w", err)
	}
	if identity != want {
		return fmt.Errorf("the migration switched the session from %q to %q; qualify object names instead of changing the database or search_path", want, identity)
	}
	return nil
}

func (m *Migrator) loadApplied(ctx context.Context, conn *sql.Conn) (map[int64]applied, error) {
	rows, err := conn.QueryContext(ctx, "SELECT version, name, checksum, applied_at FROM "+Table)
	if err != nil {
		return nil, fmt.Errorf("read applied migrations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	state := make(map[int64]applied)
	for rows.Next() {
		var version, appliedAt int64
		var row applied
		if err := rows.Scan(&version, &row.name, &row.checksum, &appliedAt); err != nil {
			// Most likely a table written by a Forge pre-release, whose
			// applied_at was a timestamp rather than Unix microseconds.
			return nil, fmt.Errorf("read applied migrations: %s does not have the expected layout "+
				"(version BIGINT, name, checksum, applied_at BIGINT Unix microseconds): %w", Table, err)
		}
		row.appliedAt = time.UnixMicro(appliedAt).UTC()
		state[version] = row
	}
	return state, rows.Err()
}
