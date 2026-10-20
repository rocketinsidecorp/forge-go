package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/rocketinsidecorp/forge-go/encryption"
)

// EncryptedColumn returns the encryption.Rewriter for one encrypted TEXT
// column, used by Cipher.Reencrypt to move a table to the active key. The
// primary key must be a uuid or text column (UUIDv7 in Forge applications);
// other types, such as bigint, are refused with an error naming the
// requirement. The spec's
// placeholder style is always PostgreSQL's.
func EncryptedColumn(db DBTX, spec encryption.SQLColumn) (encryption.Rewriter, error) {
	if db == nil {
		return nil, errors.New("postgres: database is required")
	}
	spec.DollarPlaceholders = true
	spec, err := spec.Validate()
	if err != nil {
		return nil, err
	}
	return encryptedColumn{db: db, spec: spec}, nil
}

type encryptedColumn struct {
	db   DBTX
	spec encryption.SQLColumn
}

func (c encryptedColumn) Table() string  { return c.spec.Table }
func (c encryptedColumn) Column() string { return c.spec.Column }

func (c encryptedColumn) Page(ctx context.Context, after string, limit int) ([]encryption.Record, error) {
	var args []any
	if after != "" {
		args = append(args, after)
	}
	rows, err := c.db.Query(ctx, c.spec.PageSQL(limit, after != ""), args...)
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
	tag, err := c.db.Exec(ctx, query, append(args, u.ID, u.OldSealed)...)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}
