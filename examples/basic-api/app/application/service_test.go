package application_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/examples/basic-api/app/application"
	"github.com/rocketinsidecorp/forge-go/examples/basic-api/app/domain"
	"github.com/rocketinsidecorp/forge-go/pagination"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

var start = time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

// fixture is a service over an in-memory store with a clock that advances one
// second per reading, so every timestamp in a test is distinct and known.
type fixture struct {
	service *application.Service
	repo    *memoryRepository
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	repo := newMemoryRepository()
	ticks := 0
	clock := func() time.Time {
		ticks++
		return start.Add(time.Duration(ticks) * time.Second)
	}
	service, err := application.NewService(repo, repo, clock, uuid.New)
	if err != nil {
		t.Fatal(err)
	}
	return fixture{service: service, repo: repo}
}

func (f fixture) create(t *testing.T, title string) domain.Task {
	t.Helper()
	task, err := f.service.Create(context.Background(), title)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func TestNewServiceRequiresDependencies(t *testing.T) {
	t.Parallel()
	repo := newMemoryRepository()
	if _, err := application.NewService(nil, repo, time.Now, uuid.New); err == nil {
		t.Error("nil repository accepted")
	}
	if _, err := application.NewService(repo, nil, time.Now, uuid.New); err == nil {
		t.Error("nil transactor accepted")
	}
	if _, err := application.NewService(repo, repo, nil, uuid.New); err == nil {
		t.Error("nil clock accepted")
	}
	if _, err := application.NewService(repo, repo, time.Now, nil); err == nil {
		t.Error("nil id source accepted")
	}
}

func TestCreate(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	task := f.create(t, "  Write the ADR  ")
	if task.Title != "Write the ADR" || task.Done || task.Version != 1 || !task.CreatedAt.Equal(start.Add(time.Second)) {
		t.Fatalf("task = %+v", task)
	}
	if task.ID.Version() != 7 {
		t.Fatalf("id %v is not a UUIDv7", task.ID)
	}
	stored, err := f.service.Get(context.Background(), task.ID)
	if err != nil || stored != task {
		t.Fatalf("stored = %+v, %v", stored, err)
	}
}

func TestCreateRejectsInvalidTitleWithoutWriting(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	if _, err := f.service.Create(context.Background(), "   "); !errors.Is(err, domain.ErrInvalidTitle) {
		t.Fatalf("err = %v, want ErrInvalidTitle", err)
	}
	if len(f.repo.tasks) != 0 {
		t.Fatalf("invalid task was stored: %+v", f.repo.tasks)
	}
}

func TestCreatePropagatesIDFailure(t *testing.T) {
	t.Parallel()
	repo := newMemoryRepository()
	entropy := errors.New("entropy exhausted")
	service, err := application.NewService(repo, repo, time.Now, func() (uuid.UUID, error) { return uuid.UUID{}, entropy })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Create(context.Background(), "x"); !errors.Is(err, entropy) {
		t.Fatalf("err = %v, want %v", err, entropy)
	}
}

func TestGetMissing(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	if _, err := f.service.Get(context.Background(), uuid.MustNew()); !errors.Is(err, domain.ErrTaskNotFound) {
		t.Fatalf("err = %v, want ErrTaskNotFound", err)
	}
}

func TestListWalksEveryTaskOnceNewestFirst(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	var created []domain.Task
	for range 7 {
		created = append(created, f.create(t, "task"))
	}

	var seen []domain.Task
	request := pagination.Request{Limit: 3}
	for pages := 1; ; pages++ {
		page, err := f.service.List(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		seen = append(seen, page.Items...)
		if page.NextCursor == "" {
			if pages != 3 {
				t.Fatalf("walked %d pages, want 3", pages)
			}
			break
		}
		if request.After, err = pagination.DecodeCursor(page.NextCursor); err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != len(created) {
		t.Fatalf("saw %d tasks, want %d", len(seen), len(created))
	}
	for i, task := range seen {
		if want := created[len(created)-1-i]; task.ID != want.ID {
			t.Fatalf("position %d: got %v, want %v", i, task.ID, want.ID)
		}
	}
}

func TestListLimits(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for range pagination.MaxLimit + 1 {
		f.create(t, "task")
	}
	tests := []struct {
		limit, want int
	}{
		{limit: 0, want: pagination.DefaultLimit},
		{limit: pagination.MaxLimit + 50, want: pagination.MaxLimit},
		{limit: 1, want: 1},
	}
	for _, test := range tests {
		page, err := f.service.List(context.Background(), pagination.Request{Limit: test.limit})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != test.want || page.NextCursor == "" {
			t.Errorf("limit %d: got %d items, cursor %q; want %d items and a cursor", test.limit, len(page.Items), page.NextCursor, test.want)
		}
	}
}

func TestListEmpty(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	page, err := f.service.List(context.Background(), pagination.Request{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if page.Items == nil || len(page.Items) != 0 || page.NextCursor != "" {
		t.Fatalf("page = %+v, want empty non-nil items and no cursor", page)
	}
}

func TestComplete(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	task := f.create(t, "task")

	completed, err := f.service.Complete(context.Background(), task.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !completed.Done || completed.Version != 2 || !completed.UpdatedAt.After(task.UpdatedAt) || !completed.CreatedAt.Equal(task.CreatedAt) {
		t.Fatalf("completed = %+v", completed)
	}

	// Same version as the current one: a no-op that does not advance it.
	again, err := f.service.Complete(context.Background(), task.ID, 2)
	if err != nil || again != completed {
		t.Fatalf("again = %+v, %v; want %+v", again, err, completed)
	}

	// Retrying with the version the first call used is stale.
	if _, err := f.service.Complete(context.Background(), task.ID, 1); !errors.Is(err, domain.ErrStaleVersion) {
		t.Fatalf("err = %v, want ErrStaleVersion", err)
	}
}

func TestCompleteErrors(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	task := f.create(t, "task")
	tests := []struct {
		name    string
		id      uuid.UUID
		version int
		want    error
	}{
		{name: "missing", id: uuid.MustNew(), version: 1, want: domain.ErrTaskNotFound},
		{name: "stale", id: task.ID, version: 7, want: domain.ErrStaleVersion},
		{name: "invalid version", id: uuid.MustNew(), version: 0, want: domain.ErrInvalidVersion},
	}
	for _, test := range tests {
		if _, err := f.service.Complete(context.Background(), test.id, test.version); !errors.Is(err, test.want) {
			t.Errorf("%s: err = %v, want %v", test.name, err, test.want)
		}
	}
	if stored, _ := f.service.Get(context.Background(), task.ID); stored != task {
		t.Fatalf("failed completions changed the task: %+v", stored)
	}
}

func TestCompleteLosesRaceToConcurrentWriter(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	task := f.create(t, "task")
	// Another request writes the task after this one has read it.
	f.repo.afterGet = f.repo.bump

	if _, err := f.service.Complete(context.Background(), task.ID, 1); !errors.Is(err, domain.ErrStaleVersion) {
		t.Fatalf("err = %v, want ErrStaleVersion", err)
	}
	f.repo.afterGet = nil
	stored, err := f.service.Get(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Done || stored.Version != 2 {
		t.Fatalf("stored = %+v, want the concurrent writer's version 2, not done", stored)
	}
}
