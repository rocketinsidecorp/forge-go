package encryption

import (
	"context"
	"errors"
	"fmt"
)

const (
	defaultBatchSize = 100
	maxBatchSize     = 10000
)

// ErrConfiguration reports a rotation that cannot see what it must rotate,
// such as an unscoped PostgreSQL rewriter on a table whose row-level security
// hides rows from the connected role. Running it would scan nothing and
// report success; it is refused instead.
var ErrConfiguration = errors.New("encryption: invalid rotation configuration")

// Record is one stored ciphertext and the id of its row.
type Record struct {
	ID     string
	Sealed string
}

// Update replaces a stored ciphertext. OldSealed lets the store apply it only
// if the row still holds that value (optimistic locking), so a concurrent
// application write is never overwritten with stale data. Index is the new
// blind index when the rotation maintains one, else empty.
type Update struct {
	ID        string
	OldSealed string
	NewSealed string
	Index     string
}

// Rewriter is the storage side of a rotation for one encrypted column.
type Rewriter interface {
	Table() string
	Column() string
	// Page returns up to limit rows with ID greater than after, ordered by ID
	// ascending in the database's own order. Rows with a NULL value are omitted.
	Page(ctx context.Context, after string, limit int) ([]Record, error)
	// Replace applies u and reports whether a row was changed. False means the
	// row no longer holds u.OldSealed.
	Replace(ctx context.Context, u Update) (bool, error)
}

// RotateOptions tunes Reencrypt.
type RotateOptions struct {
	// BatchSize is the page size; 100 when zero, at most 10000.
	BatchSize int
	// After resumes a previous run: rows with ID <= After are skipped. Pass
	// the LastID of the interrupted Report.
	After string
	// BlindIndex also writes the blind index (under the active key) for every
	// row visited, repairing missing or stale indexes.
	BlindIndex bool
}

// Report summarizes a rotation. LastID is the checkpoint for RotateOptions.After.
type Report struct {
	Scanned   int
	Rotated   int
	Skipped   int // already on the active key
	Conflicts int // changed concurrently; rerun to pick them up
	Reindexed int // blind indexes written (RotateOptions.BlindIndex)
	LastID    string
}

// Reencrypt moves every row of rw to the active key, in bounded pages, and is
// safe to interrupt and rerun: rows already on the active key are skipped and
// the Report's LastID resumes where a cancelled run stopped. A row that cannot
// be decrypted stops the run with an error naming its id, never its value.
//
// It moves only the rows rw shows. A Report never proves that a column is
// complete (Scanned == 0 and Rotated == 0 are exactly what a role that sees no
// row reports): before removing an old key, count the rows not sealed with the
// active key as a role that sees every row (docs/ENCRYPTION.md).
func (c *Cipher) Reencrypt(ctx context.Context, rw Rewriter, opts RotateOptions) (Report, error) {
	if rw == nil {
		return Report{}, errors.New("encryption: rewriter is required")
	}
	size := opts.BatchSize
	switch {
	case size == 0:
		size = defaultBatchSize
	case size < 0 || size > maxBatchSize:
		return Report{}, fmt.Errorf("encryption: batch size must be between 1 and %d", maxBatchSize)
	}
	report := Report{LastID: opts.After}
	for {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		page, err := rw.Page(ctx, report.LastID, size)
		if err != nil {
			return report, fmt.Errorf("read page after %q: %w", report.LastID, err)
		}
		for _, record := range page {
			if err := ctx.Err(); err != nil {
				return report, err
			}
			// The database defines the order (integers sort numerically, text
			// by collation), so the ids are never compared here; a row that
			// repeats the checkpoint would loop forever and is refused.
			if record.ID == report.LastID {
				return report, fmt.Errorf("encryption: rewriter did not advance past row %q", record.ID)
			}
			if err := c.reencryptRecord(ctx, rw, record, opts, &report); err != nil {
				return report, err
			}
			report.Scanned++
			report.LastID = record.ID
		}
		if len(page) < size {
			return report, nil
		}
	}
}

func (c *Cipher) reencryptRecord(ctx context.Context, rw Rewriter, record Record, opts RotateOptions, report *Report) error {
	stale, err := c.NeedsRotation(record.Sealed)
	if err != nil {
		return fmt.Errorf("row %q: %w", record.ID, err)
	}
	if !stale && !opts.BlindIndex {
		report.Skipped++
		return nil
	}
	aad := AAD{Table: rw.Table(), Column: rw.Column(), RecordID: record.ID}
	plaintext, err := c.Decrypt(record.Sealed, aad)
	if err != nil {
		return fmt.Errorf("row %q: %w", record.ID, err)
	}
	defer clear(plaintext)
	update := Update{ID: record.ID, OldSealed: record.Sealed, NewSealed: record.Sealed}
	if stale {
		if update.NewSealed, err = c.Encrypt(plaintext, aad); err != nil {
			return fmt.Errorf("row %q: %w", record.ID, err)
		}
	}
	if opts.BlindIndex {
		if update.Index, err = c.BlindIndex(rw.Table(), rw.Column(), plaintext); err != nil {
			return fmt.Errorf("row %q: %w", record.ID, err)
		}
	}
	changed, err := rw.Replace(ctx, update)
	if err != nil {
		return fmt.Errorf("row %q: %w", record.ID, err)
	}
	if !changed {
		report.Conflicts++
		return nil
	}
	if stale {
		report.Rotated++
	}
	if opts.BlindIndex {
		report.Reindexed++
	} else if !stale {
		report.Skipped++
	}
	return nil
}
