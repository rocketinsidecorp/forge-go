package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	forge "github.com/rocketinsidecorp/forge-go"
	"github.com/rocketinsidecorp/forge-go/examples/basic-api/app/adapters/httpapi"
	"github.com/rocketinsidecorp/forge-go/examples/basic-api/app/adapters/persistence"
	"github.com/rocketinsidecorp/forge-go/examples/basic-api/app/application"
	"github.com/rocketinsidecorp/forge-go/examples/basic-api/db"
	"github.com/rocketinsidecorp/forge-go/pagination"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"
	"github.com/rocketinsidecorp/forge-go/uuid"
	"github.com/rocketinsidecorp/forge-go/web"
)

type task struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Done      bool      `json:"done"`
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// newHandler serves the task API from a forge.App, so requests pass through
// the framework's middleware (request IDs, body limit, error format), backed
// by a fresh migrated database.
func newHandler(t *testing.T) http.Handler {
	t.Helper()
	database := postgrestest.NewMigrated(t, db.Migrations())
	service, err := application.NewService(persistence.NewTaskRepository(database), persistence.NewTransactor(database), time.Now, uuid.New)
	if err != nil {
		t.Fatal(err)
	}
	app, err := forge.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := httpapi.Register(app, service); err != nil {
		t.Fatal(err)
	}
	return app.Handler()
}

func send(t *testing.T, handler http.Handler, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func decode[T any](t *testing.T, recorder *httptest.ResponseRecorder) T {
	t.Helper()
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type %q, body %s", got, recorder.Body)
	}
	var value T
	if err := json.Unmarshal(recorder.Body.Bytes(), &value); err != nil {
		t.Fatalf("decode %s: %v", recorder.Body, err)
	}
	return value
}

func expectStatus(t *testing.T, recorder *httptest.ResponseRecorder, status int) {
	t.Helper()
	if recorder.Code != status {
		t.Fatalf("status %d, want %d; body %s", recorder.Code, status, recorder.Body)
	}
}

func createTask(t *testing.T, handler http.Handler, title string) task {
	t.Helper()
	body, err := json.Marshal(map[string]string{"title": title})
	if err != nil {
		t.Fatal(err)
	}
	recorder := send(t, handler, http.MethodPost, "/v1/tasks", string(body))
	expectStatus(t, recorder, http.StatusCreated)
	return decode[task](t, recorder)
}

func TestTaskLifecycle(t *testing.T) {
	t.Parallel()
	handler := newHandler(t)

	recorder := send(t, handler, http.MethodPost, "/v1/tasks", `{"title":"  Review the pull request  "}`)
	expectStatus(t, recorder, http.StatusCreated)
	created := decode[task](t, recorder)
	if _, err := uuid.Parse(created.ID); err != nil {
		t.Fatalf("id %q is not a UUIDv7: %v", created.ID, err)
	}
	if created.Title != "Review the pull request" || created.Done || created.Version != 1 ||
		created.CreatedAt.IsZero() || !created.UpdatedAt.Equal(created.CreatedAt) {
		t.Fatalf("created = %+v", created)
	}
	location := recorder.Header().Get("Location")
	if location != "/v1/tasks/"+created.ID {
		t.Fatalf("Location = %q", location)
	}

	recorder = send(t, handler, http.MethodGet, location, "")
	expectStatus(t, recorder, http.StatusOK)
	if got := decode[task](t, recorder); got != created {
		t.Fatalf("got = %+v, want %+v", got, created)
	}

	completePath := location + "/complete"
	recorder = send(t, handler, http.MethodPost, completePath, `{"version":1}`)
	expectStatus(t, recorder, http.StatusOK)
	completed := decode[task](t, recorder)
	if !completed.Done || completed.Version != 2 || !completed.CreatedAt.Equal(created.CreatedAt) || !completed.UpdatedAt.After(created.UpdatedAt) {
		t.Fatalf("completed = %+v", completed)
	}

	// Completing again with the current version is a no-op.
	recorder = send(t, handler, http.MethodPost, completePath, `{"version":2}`)
	expectStatus(t, recorder, http.StatusOK)
	if again := decode[task](t, recorder); again != completed {
		t.Fatalf("again = %+v, want %+v", again, completed)
	}

	// Replaying the first request is rejected as stale.
	recorder = send(t, handler, http.MethodPost, completePath, `{"version":1}`)
	expectStatus(t, recorder, http.StatusConflict)
	if body := decode[web.ErrorBody](t, recorder); body.Error.Code != "stale_version" {
		t.Fatalf("error = %+v", body.Error)
	}

	recorder = send(t, handler, http.MethodGet, location, "")
	expectStatus(t, recorder, http.StatusOK)
	if got := decode[task](t, recorder); got != completed {
		t.Fatalf("stored = %+v, want %+v", got, completed)
	}
}

func TestListPagination(t *testing.T) {
	t.Parallel()
	handler := newHandler(t)
	const total = 5
	var created []task
	for i := range total {
		created = append(created, createTask(t, handler, "task "+strconv.Itoa(i)))
	}

	var seen []task
	target := "/v1/tasks?limit=2"
	for pages := 1; ; pages++ {
		recorder := send(t, handler, http.MethodGet, target, "")
		expectStatus(t, recorder, http.StatusOK)
		page := decode[pagination.Page[task]](t, recorder)
		seen = append(seen, page.Items...)
		if page.NextCursor == "" {
			if pages != 3 {
				t.Fatalf("walked %d pages, want 3", pages)
			}
			break
		}
		target = "/v1/tasks?limit=2&cursor=" + page.NextCursor
	}
	if len(seen) != total {
		t.Fatalf("saw %d tasks, want %d", len(seen), total)
	}
	for i, got := range seen {
		if want := created[total-1-i]; got != want {
			t.Fatalf("position %d: got %+v, want %+v", i, got, want)
		}
	}

	// Without parameters the default limit covers everything.
	recorder := send(t, handler, http.MethodGet, "/v1/tasks", "")
	expectStatus(t, recorder, http.StatusOK)
	if page := decode[pagination.Page[task]](t, recorder); len(page.Items) != total || page.NextCursor != "" {
		t.Fatalf("default page: %d items, cursor %q", len(page.Items), page.NextCursor)
	}
}

func TestListEmptyReturnsEmptyArray(t *testing.T) {
	t.Parallel()
	recorder := send(t, newHandler(t), http.MethodGet, "/v1/tasks", "")
	expectStatus(t, recorder, http.StatusOK)
	if body := strings.TrimSpace(recorder.Body.String()); body != `{"items":[]}` {
		t.Fatalf("body = %s", body)
	}
}

func TestErrors(t *testing.T) {
	t.Parallel()
	handler := newHandler(t)
	existing := createTask(t, handler, "existing")
	missing := uuid.MustNew().String()
	const uuidV4 = "550e8400-e29b-41d4-a716-446655440000"

	tests := []struct {
		name        string
		method      string
		target      string
		contentType string
		body        string
		status      int
		code        string
	}{
		{name: "blank title", method: http.MethodPost, target: "/v1/tasks", body: `{"title":"   "}`, status: 400, code: "invalid_title"},
		{name: "missing title", method: http.MethodPost, target: "/v1/tasks", body: `{}`, status: 400, code: "invalid_title"},
		{name: "title too long", method: http.MethodPost, target: "/v1/tasks", body: `{"title":"` + strings.Repeat("a", 201) + `"}`, status: 400, code: "invalid_title"},
		{name: "unknown field", method: http.MethodPost, target: "/v1/tasks", body: `{"title":"x","done":true}`, status: 400, code: "invalid_json"},
		{name: "malformed JSON", method: http.MethodPost, target: "/v1/tasks", body: `{"title":`, status: 400, code: "invalid_json"},
		{name: "wrong content type", method: http.MethodPost, target: "/v1/tasks", contentType: "text/plain", body: `{"title":"x"}`, status: 415, code: "unsupported_media_type"},
		{name: "invalid id", method: http.MethodGet, target: "/v1/tasks/not-a-uuid", status: 400, code: "invalid_id"},
		{name: "non-v7 id", method: http.MethodGet, target: "/v1/tasks/" + uuidV4, status: 400, code: "invalid_id"},
		{name: "missing task", method: http.MethodGet, target: "/v1/tasks/" + missing, status: 404, code: "task_not_found"},
		{name: "bad cursor", method: http.MethodGet, target: "/v1/tasks?cursor=not-a-cursor", status: 400, code: "invalid_cursor"},
		{name: "bad limit", method: http.MethodGet, target: "/v1/tasks?limit=0", status: 400, code: "invalid_limit"},
		{name: "limit too large", method: http.MethodGet, target: "/v1/tasks?limit=1000", status: 400, code: "invalid_limit"},
		{name: "complete missing task", method: http.MethodPost, target: "/v1/tasks/" + missing + "/complete", body: `{"version":1}`, status: 404, code: "task_not_found"},
		{name: "complete invalid id", method: http.MethodPost, target: "/v1/tasks/nope/complete", body: `{"version":1}`, status: 400, code: "invalid_id"},
		{name: "complete without version", method: http.MethodPost, target: "/v1/tasks/" + existing.ID + "/complete", body: `{}`, status: 400, code: "invalid_version"},
		{name: "complete with string version", method: http.MethodPost, target: "/v1/tasks/" + existing.ID + "/complete", body: `{"version":"1"}`, status: 400, code: "invalid_json"},
		{name: "complete with stale version", method: http.MethodPost, target: "/v1/tasks/" + existing.ID + "/complete", body: `{"version":2}`, status: 409, code: "stale_version"},
		{name: "complete without body", method: http.MethodPost, target: "/v1/tasks/" + existing.ID + "/complete", contentType: "application/json", status: 400, code: "invalid_json"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			request := httptest.NewRequest(test.method, test.target, strings.NewReader(test.body))
			switch {
			case test.contentType != "":
				request.Header.Set("Content-Type", test.contentType)
			case test.body != "":
				request.Header.Set("Content-Type", "application/json")
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			expectStatus(t, recorder, test.status)
			body := decode[web.ErrorBody](t, recorder)
			if body.Error.Code != test.code || body.Error.Message == "" {
				t.Fatalf("error = %+v, want code %q", body.Error, test.code)
			}
			if body.Error.RequestID == "" || body.Error.RequestID != recorder.Header().Get("X-Request-ID") {
				t.Fatalf("request_id %q does not match X-Request-ID %q", body.Error.RequestID, recorder.Header().Get("X-Request-ID"))
			}
		})
	}
}
