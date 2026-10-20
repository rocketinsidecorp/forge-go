package web_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/uuid"
	"github.com/rocketinsidecorp/forge-go/web"
)

type benchPost struct {
	ID        uuid.UUID `json:"id"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type benchDiscard struct{ header http.Header }

func (w benchDiscard) Header() http.Header          { return w.header }
func (benchDiscard) Write(data []byte) (int, error) { return len(data), nil }
func (benchDiscard) WriteHeader(int)                {}

// BenchmarkJSONRender is scenario "json_render" in docs/BENCHMARKS.md:
// web.JSON of one record (a scaffold's API show) and of a page of 50 records
// (its index) into a writer that discards the body.
func BenchmarkJSONRender(b *testing.B) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	post := benchPost{ID: uuid.MustNew(), Title: "Hello Forge", Body: "My first post, a few sentences long so that the body is not trivially small.", Version: 1, CreatedAt: now, UpdatedAt: now}
	list := struct {
		Items      []benchPost `json:"items"`
		NextCursor string      `json:"next_cursor"`
	}{Items: make([]benchPost, 50)}
	for i := range list.Items {
		list.Items[i] = post
	}
	for _, c := range []struct {
		name  string
		value any
	}{{"object", post}, {"list_50", list}} {
		b.Run(c.name, func(b *testing.B) {
			w := benchDiscard{header: http.Header{}}
			b.ReportAllocs()
			for b.Loop() {
				web.JSON(w, http.StatusOK, c.value)
			}
		})
	}
}
