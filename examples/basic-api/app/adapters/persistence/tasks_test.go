package persistence_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/examples/basic-api/app/adapters/persistence"
	"github.com/rocketinsidecorp/forge-go/examples/basic-api/app/application"
	"github.com/rocketinsidecorp/forge-go/examples/basic-api/app/domain"
	"github.com/rocketinsidecorp/forge-go/examples/basic-api/db"
	"github.com/rocketinsidecorp/forge-go/fault"
	"github.com/rocketinsidecorp/forge-go/postgres"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

func newRepository(t *testing.T) (*persistence.TaskRepository, *postgres.DB) {
	t.Helper()
	database := postgrestest.NewMigrated(t, db.Migrations())
	return persistence.NewTaskRepository(database), database
}

// newTask builds a valid task. PostgreSQL stores microseconds, so the clock
// is truncated for exact round-trip comparisons.
func newTask(t *testing.T, title string) domain.Task {
	t.Helper()
	task, err := domain.NewTask(uuid.MustNew(), title, time.Now().UTC().Truncate(time.Microsecond))
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func mustCreate(t *testing.T, repo *persistence.TaskRepository, title string) domain.Task {
	t.Helper()
	created, err := repo.Create(context.Background(), newTask(t, title))
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func TestCreateAndGet(t *testing.T) {
	t.Parallel()
	repo, _ := newRepository(t)
	ctx := context.Background()
	task := newTask(t, "Café ☕")

	created, err := repo.Create(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	if created != task {
		t.Fatalf("created = %+v, want %+v", created, task)
	}
	got, err := repo.Get(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != task {
		t.Fatalf("got = %+v, want %+v", got, task)
	}
	if got.CreatedAt.Location() != time.UTC {
		t.Fatalf("timestamps must be UTC, got %v", got.CreatedAt.Location())
	}
}

func TestGetMissing(t *testing.T) {
	t.Parallel()
	repo, _ := newRepository(t)
	if _, err := repo.Get(context.Background(), uuid.MustNew()); !errors.Is(err, domain.ErrTaskNotFound) {
		t.Fatalf("err = %v, want ErrTaskNotFound", err)
	}
}

func TestCreateTranslatesDatabaseErrors(t *testing.T) {
	t.Parallel()
	repo, _ := newRepository(t)
	ctx := context.Background()
	existing := mustCreate(t, repo, "first")

	// A title that bypassed the domain still meets the CHECK constraint.
	tooLong := newTask(t, "x")
	tooLong.Title = strings.Repeat("x", domain.MaxTitleLength+1)

	tests := []struct {
		name       string
		task       domain.Task
		code       string
		constraint string
	}{
		{name: "duplicate id", task: existing, code: "already_exists", constraint: "tasks_pkey"},
		{name: "title check", task: tooLong, code: "constraint_violation", constraint: "tasks_title_length"},
	}
	for _, test := range tests {
		_, err := repo.Create(ctx, test.task)
		public := fault.From(err)
		if public == nil || public.Code != test.code || public.Metadata["constraint"] != test.constraint {
			t.Errorf("%s: err = %v (%+v), want code %q constraint %q", test.name, err, public, test.code, test.constraint)
		}
	}
}

func TestListWalksEveryRowOnceNewestFirst(t *testing.T) {
	t.Parallel()
	repo, _ := newRepository(t)
	ctx := context.Background()
	const total, limit = 23, 5
	created := make([]domain.Task, 0, total)
	for range total {
		created = append(created, mustCreate(t, repo, "task"))
	}

	seen := make(map[uuid.UUID]bool, total)
	var order []uuid.UUID
	var after uuid.UUID
	for pages := 0; ; pages++ {
		if pages > total {
			t.Fatal("pagination does not terminate")
		}
		rows, err := repo.List(ctx, after, limit)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 0 {
			break
		}
		for _, task := range rows {
			if seen[task.ID] {
				t.Fatalf("task %v returned twice", task.ID)
			}
			seen[task.ID] = true
			order = append(order, task.ID)
		}
		after = rows[len(rows)-1].ID
	}

	if len(order) != total {
		t.Fatalf("walked %d rows, want %d", len(order), total)
	}
	for i, id := range order {
		if want := created[total-1-i].ID; id != want {
			t.Fatalf("position %d: got %v, want %v (newest first)", i, id, want)
		}
	}
}

func TestListEmpty(t *testing.T) {
	t.Parallel()
	repo, _ := newRepository(t)
	rows, err := repo.List(context.Background(), uuid.UUID{}, 10)
	if err != nil || len(rows) != 0 {
		t.Fatalf("rows = %v, err = %v", rows, err)
	}
}

func TestUpdate(t *testing.T) {
	t.Parallel()
	repo, _ := newRepository(t)
	ctx := context.Background()
	task := mustCreate(t, repo, "task")

	changed := task
	if _, err := changed.Complete(task.Version, task.UpdatedAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	updated, err := repo.Update(ctx, changed)
	if err != nil {
		t.Fatal(err)
	}
	want := changed
	want.Version = 2
	if updated != want {
		t.Fatalf("updated = %+v, want %+v", updated, want)
	}

	// changed still carries version 1, which is now stale.
	if _, err := repo.Update(ctx, changed); !errors.Is(err, domain.ErrStaleVersion) {
		t.Fatalf("stale update: err = %v, want ErrStaleVersion", err)
	}
	missing := newTask(t, "missing")
	if _, err := repo.Update(ctx, missing); !errors.Is(err, domain.ErrTaskNotFound) {
		t.Fatalf("missing update: err = %v, want ErrTaskNotFound", err)
	}
	if got, _ := repo.Get(ctx, task.ID); got != want {
		t.Fatalf("stored = %+v, want %+v", got, want)
	}
}

func TestTransactor(t *testing.T) {
	t.Parallel()
	repo, database := newRepository(t)
	transactor := persistence.NewTransactor(database)
	ctx := context.Background()

	committed := newTask(t, "committed")
	if err := transactor.InTx(ctx, func(tasks application.TaskRepository) error {
		_, err := tasks.Create(ctx, committed)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Get(ctx, committed.ID); err != nil {
		t.Fatalf("committed task: %v", err)
	}

	rolledBack := newTask(t, "rolled back")
	abort := errors.New("abort")
	err := transactor.InTx(ctx, func(tasks application.TaskRepository) error {
		if _, err := tasks.Create(ctx, rolledBack); err != nil {
			return err
		}
		// Visible inside the transaction...
		if _, err := tasks.Get(ctx, rolledBack.ID); err != nil {
			return err
		}
		return abort
	})
	if !errors.Is(err, abort) {
		t.Fatalf("err = %v, want %v", err, abort)
	}
	// ...and gone after the rollback.
	if _, err := repo.Get(ctx, rolledBack.ID); !errors.Is(err, domain.ErrTaskNotFound) {
		t.Fatalf("rolled back task: err = %v, want ErrTaskNotFound", err)
	}
}
