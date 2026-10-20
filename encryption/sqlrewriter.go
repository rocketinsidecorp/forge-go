package encryption

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
)

var identifierPattern = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// SQLColumn describes one encrypted TEXT column and generates the statements a
// Rewriter needs. The primary key is read as text and a text parameter resumes
// after it: use a text or uuid key (UUIDv7 in Forge applications). Integer
// keys work through SQLRewriter on MySQL, MariaDB and SQLite, which convert,
// but not on PostgreSQL, where postgres.EncryptedColumn reports an error. Identifiers are validated (lowercase letters, digits, underscore):
// they come from the application's source, never from user input.
type SQLColumn struct {
	Table  string
	Column string
	// IDColumn is the primary key; "id" when empty.
	IDColumn string
	// IndexColumn receives the blind index when RotateOptions.BlindIndex is set.
	IndexColumn string
	// DollarPlaceholders selects $1, $2 (PostgreSQL) instead of ? (MySQL,
	// MariaDB, SQLite).
	DollarPlaceholders bool
}

// Validate checks the identifiers and fills defaults.
func (c SQLColumn) Validate() (SQLColumn, error) {
	if c.IDColumn == "" {
		c.IDColumn = "id"
	}
	for _, name := range []string{c.Table, c.Column, c.IDColumn} {
		if !identifierPattern.MatchString(name) {
			return SQLColumn{}, fmt.Errorf("encryption: invalid SQL identifier %q", name)
		}
	}
	if c.IndexColumn != "" && !identifierPattern.MatchString(c.IndexColumn) {
		return SQLColumn{}, fmt.Errorf("encryption: invalid SQL identifier %q", c.IndexColumn)
	}
	return c, nil
}

func (c SQLColumn) placeholder(n int) string {
	if c.DollarPlaceholders {
		return fmt.Sprintf("$%d", n)
	}
	return "?"
}

// PageSQL is the statement behind Rewriter.Page. With after set it takes one
// argument, the id after which to read; for the first page (after false) it
// takes none, which also suits uuid columns that cannot compare with "".
func (c SQLColumn) PageSQL(limit int, after bool) string {
	condition := fmt.Sprintf("%s IS NOT NULL", c.Column)
	if after {
		condition = fmt.Sprintf("%s > %s AND %s", c.IDColumn, c.placeholder(1), condition)
	}
	return fmt.Sprintf("SELECT %s, %s FROM %s WHERE %s ORDER BY %s LIMIT %d",
		c.IDColumn, c.Column, c.Table, condition, c.IDColumn, limit) // #nosec G201 -- identifiers are validated, limit is an int
}

// ReplaceSQL is the statement behind Rewriter.Replace, a compare-and-set on
// the old ciphertext. Its arguments are, in order: the new ciphertext, the new
// blind index (only when withIndex), the id and the old ciphertext.
func (c SQLColumn) ReplaceSQL(withIndex bool) (string, error) {
	set, n := fmt.Sprintf("%s = %s", c.Column, c.placeholder(1)), 1
	if withIndex {
		if c.IndexColumn == "" {
			return "", errors.New("encryption: a blind index was requested but no index column is configured")
		}
		n++
		set += fmt.Sprintf(", %s = %s", c.IndexColumn, c.placeholder(n))
	}
	return fmt.Sprintf("UPDATE %s SET %s WHERE %s = %s AND %s = %s",
		c.Table, set, c.IDColumn, c.placeholder(n+1), c.Column, c.placeholder(n+2)), nil // #nosec G201 -- identifiers are validated
}

// Queryer is the part of *sql.DB and *sql.Tx a SQLRewriter needs.
type Queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// SQLRewriter is a Rewriter over database/sql, for MySQL, MariaDB and SQLite
// (and PostgreSQL through a database/sql driver). The postgres package
// provides the native pgx equivalent.
type SQLRewriter struct {
	db   Queryer
	spec SQLColumn
}

// NewSQLRewriter validates spec and returns the Rewriter.
func NewSQLRewriter(db Queryer, spec SQLColumn) (*SQLRewriter, error) {
	if db == nil {
		return nil, errors.New("encryption: database is required")
	}
	spec, err := spec.Validate()
	if err != nil {
		return nil, err
	}
	return &SQLRewriter{db: db, spec: spec}, nil
}

func (r *SQLRewriter) Table() string  { return r.spec.Table }
func (r *SQLRewriter) Column() string { return r.spec.Column }

// Page implements Rewriter.
func (r *SQLRewriter) Page(ctx context.Context, after string, limit int) ([]Record, error) {
	var args []any
	if after != "" {
		args = append(args, after)
	}
	rows, err := r.db.QueryContext(ctx, r.spec.PageSQL(limit, after != ""), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var page []Record
	for rows.Next() {
		var record Record
		if err := rows.Scan(&record.ID, &record.Sealed); err != nil {
			return nil, err
		}
		page = append(page, record)
	}
	return page, rows.Err()
}

// Replace implements Rewriter with an optimistic compare-and-set on the old
// ciphertext.
func (r *SQLRewriter) Replace(ctx context.Context, u Update) (bool, error) {
	query, err := r.spec.ReplaceSQL(u.Index != "")
	if err != nil {
		return false, err
	}
	args := []any{u.NewSealed}
	if u.Index != "" {
		args = append(args, u.Index)
	}
	result, err := r.db.ExecContext(ctx, query, append(args, u.ID, u.OldSealed)...)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n > 0, err
}
