package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/examples/basic-api/app/domain"
	"github.com/rocketinsidecorp/forge-go/fault"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

var now = time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

func TestNormalizeTitle(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		want  string
		ok    bool
	}{
		{name: "plain", input: "Write docs", want: "Write docs", ok: true},
		{name: "trimmed", input: "  \t Write docs \n", want: "Write docs", ok: true},
		{name: "single character", input: "x", want: "x", ok: true},
		{name: "exactly max ASCII", input: strings.Repeat("a", 200), want: strings.Repeat("a", 200), ok: true},
		{name: "max counted in characters not bytes", input: strings.Repeat("é", 200), want: strings.Repeat("é", 200), ok: true},
		{name: "emoji counted as characters", input: strings.Repeat("🚀", 200), want: strings.Repeat("🚀", 200), ok: true},
		{name: "max after trimming", input: "  " + strings.Repeat("a", 200) + "  ", want: strings.Repeat("a", 200), ok: true},
		{name: "empty", input: ""},
		{name: "whitespace only", input: " \t\n "},
		{name: "unicode whitespace only", input: "  "},
		{name: "too long", input: strings.Repeat("a", 201)},
		{name: "too long multibyte", input: strings.Repeat("é", 201)},
		{name: "inner newline", input: "two\nlines"},
		{name: "NUL byte", input: "nul\x00byte"},
		{name: "invalid UTF-8", input: "bad \xff byte"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := domain.NormalizeTitle(test.input)
			if !test.ok {
				if !errors.Is(err, domain.ErrInvalidTitle) {
					t.Fatalf("err = %v, want ErrInvalidTitle", err)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("got %q, %v; want %q", got, err, test.want)
			}
		})
	}
}

func TestNewTask(t *testing.T) {
	t.Parallel()
	id := uuid.MustNew()
	task, err := domain.NewTask(id, "  Ship it ", now)
	if err != nil {
		t.Fatal(err)
	}
	want := domain.Task{ID: id, Title: "Ship it", Version: 1, CreatedAt: now, UpdatedAt: now}
	if task != want {
		t.Fatalf("task = %+v, want %+v", task, want)
	}
	if _, err := domain.NewTask(id, " ", now); !errors.Is(err, domain.ErrInvalidTitle) {
		t.Fatalf("err = %v, want ErrInvalidTitle", err)
	}
}

func TestParseID(t *testing.T) {
	t.Parallel()
	id := uuid.MustNew()
	if got, err := domain.ParseID(id.String()); err != nil || got != id {
		t.Fatalf("got %v, %v", got, err)
	}
	for _, input := range []string{"", "not-a-uuid", "6ba7b810-9dad-11d1-80b4-00c04fd430c8" /* v1 */, "550e8400-e29b-41d4-a716-446655440000" /* v4 */} {
		if _, err := domain.ParseID(input); !errors.Is(err, domain.ErrInvalidID) {
			t.Errorf("ParseID(%q) err = %v, want ErrInvalidID", input, err)
		}
	}
}

func TestComplete(t *testing.T) {
	t.Parallel()
	later := now.Add(time.Minute)
	pending := domain.Task{ID: uuid.MustNew(), Title: "t", Version: 3, CreatedAt: now, UpdatedAt: now}
	done := pending
	done.Done = true

	tests := []struct {
		name        string
		task        domain.Task
		version     int
		wantChanged bool
		wantErr     error
		wantTask    domain.Task
	}{
		{name: "pending with current version", task: pending, version: 3, wantChanged: true, wantTask: domain.Task{ID: pending.ID, Title: "t", Done: true, Version: 3, CreatedAt: now, UpdatedAt: later}},
		{name: "already done with current version is a no-op", task: done, version: 3, wantTask: done},
		{name: "stale version", task: pending, version: 2, wantErr: domain.ErrStaleVersion, wantTask: pending},
		{name: "future version is stale too", task: pending, version: 4, wantErr: domain.ErrStaleVersion, wantTask: pending},
		{name: "already done with stale version", task: done, version: 2, wantErr: domain.ErrStaleVersion, wantTask: done},
		{name: "zero version", task: pending, version: 0, wantErr: domain.ErrInvalidVersion, wantTask: pending},
		{name: "negative version", task: pending, version: -1, wantErr: domain.ErrInvalidVersion, wantTask: pending},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			task := test.task
			changed, err := task.Complete(test.version, later)
			if !errors.Is(err, test.wantErr) || changed != test.wantChanged {
				t.Fatalf("changed=%v err=%v; want changed=%v err=%v", changed, err, test.wantChanged, test.wantErr)
			}
			if task != test.wantTask {
				t.Fatalf("task = %+v, want %+v", task, test.wantTask)
			}
		})
	}
}

func TestErrorsAreStableFaults(t *testing.T) {
	t.Parallel()
	tests := []struct {
		err    error
		code   string
		status int
	}{
		{domain.ErrInvalidID, "invalid_id", 400},
		{domain.ErrInvalidTitle, "invalid_title", 400},
		{domain.ErrInvalidVersion, "invalid_version", 400},
		{domain.ErrTaskNotFound, "task_not_found", 404},
		{domain.ErrStaleVersion, "stale_version", 409},
	}
	for _, test := range tests {
		public := fault.From(test.err)
		if public == nil || public.Code != test.code || public.Status() != test.status {
			t.Errorf("%v: got %+v, want code %q status %d", test.err, public, test.code, test.status)
		}
	}
}
