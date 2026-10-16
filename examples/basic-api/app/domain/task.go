// Package domain holds the task entity and its rules. It depends on no other
// layer and knows nothing about HTTP or SQL.
package domain

import (
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/rocketinsidecorp/forge-go/uuid"
)

// MaxTitleLength is measured in characters (Unicode code points), matching
// PostgreSQL's char_length used by the tasks_title_length constraint.
const MaxTitleLength = 200

// Task is a unit of work that can be completed.
type Task struct {
	ID    uuid.UUID
	Title string
	Done  bool
	// Version is the optimistic-locking token. The store advances it on every
	// successful write; a write based on an older version is rejected.
	Version   int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewTask creates a pending task at version 1.
func NewTask(id uuid.UUID, title string, now time.Time) (Task, error) {
	normalized, err := NormalizeTitle(title)
	if err != nil {
		return Task{}, err
	}
	return Task{ID: id, Title: normalized, Version: 1, CreatedAt: now, UpdatedAt: now}, nil
}

// NormalizeTitle trims surrounding whitespace and validates the result: 1 to
// MaxTitleLength characters of valid UTF-8 with no control characters, so a
// title is always a single printable line.
func NormalizeTitle(title string) (string, error) {
	title = strings.TrimSpace(title)
	if title == "" || !utf8.ValidString(title) || utf8.RuneCountInString(title) > MaxTitleLength {
		return "", ErrInvalidTitle
	}
	if strings.ContainsFunc(title, unicode.IsControl) {
		return "", ErrInvalidTitle
	}
	return title, nil
}

// ParseID parses a task id. Task ids are UUIDv7.
func ParseID(value string) (uuid.UUID, error) {
	id, err := uuid.Parse(value)
	if err != nil {
		return uuid.UUID{}, ErrInvalidID
	}
	return id, nil
}

// ValidateVersion rejects versions no task can have. Versions start at 1.
func ValidateVersion(version int) error {
	if version < 1 {
		return ErrInvalidVersion
	}
	return nil
}

// Complete marks the task done on behalf of a caller who last saw
// expectedVersion.
//
// The rule: expectedVersion must equal the current version, otherwise the
// caller acted on stale data and gets ErrStaleVersion, whether or not the
// task is already done. With a matching version, completing a task that is
// already done is a no-op and reports changed == false, so the store is not
// written and the version does not advance.
func (t *Task) Complete(expectedVersion int, now time.Time) (changed bool, err error) {
	if err := ValidateVersion(expectedVersion); err != nil {
		return false, err
	}
	if expectedVersion != t.Version {
		return false, ErrStaleVersion
	}
	if t.Done {
		return false, nil
	}
	t.Done = true
	t.UpdatedAt = now
	return true, nil
}
