// Package httpapi is the HTTP adapter for the task use cases. Handlers only
// decode the request, call the application service and encode the result;
// every error goes through web.Error so all responses share one format.
package httpapi

import (
	"net/http"
	"time"

	"github.com/rocketinsidecorp/forge-go/examples/basic-api/app/application"
	"github.com/rocketinsidecorp/forge-go/examples/basic-api/app/domain"
	"github.com/rocketinsidecorp/forge-go/pagination"
	"github.com/rocketinsidecorp/forge-go/uuid"
	"github.com/rocketinsidecorp/forge-go/web"
)

const tasksPath = "/v1/tasks"

// Router is where the handlers are registered; *forge.App satisfies it.
type Router interface {
	HandleFunc(pattern string, handler http.HandlerFunc) error
}

// Register mounts the task endpoints.
func Register(router Router, tasks *application.Service) error {
	h := handlers{tasks: tasks}
	routes := []struct {
		pattern string
		handler http.HandlerFunc
	}{
		{"POST " + tasksPath, h.create},
		{"GET " + tasksPath, h.list},
		{"GET " + tasksPath + "/{id}", h.get},
		{"POST " + tasksPath + "/{id}/complete", h.complete},
	}
	for _, route := range routes {
		if err := router.HandleFunc(route.pattern, route.handler); err != nil {
			return err
		}
	}
	return nil
}

type handlers struct {
	tasks *application.Service
}

type createRequest struct {
	Title string `json:"title"`
}

type completeRequest struct {
	Version int `json:"version"`
}

type taskResponse struct {
	ID        uuid.UUID `json:"id"`
	Title     string    `json:"title"`
	Done      bool      `json:"done"`
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (h handlers) create(w http.ResponseWriter, r *http.Request) {
	var body createRequest
	if err := web.DecodeJSON(r, &body); err != nil {
		web.Error(w, r, err)
		return
	}
	task, err := h.tasks.Create(r.Context(), body.Title)
	if err != nil {
		web.Error(w, r, err)
		return
	}
	w.Header().Set("Location", tasksPath+"/"+task.ID.String())
	web.JSON(w, http.StatusCreated, newTaskResponse(task))
}

func (h handlers) list(w http.ResponseWriter, r *http.Request) {
	request, err := pagination.FromQuery(r.URL.Query())
	if err != nil {
		web.Error(w, r, err)
		return
	}
	page, err := h.tasks.List(r.Context(), request)
	if err != nil {
		web.Error(w, r, err)
		return
	}
	items := make([]taskResponse, len(page.Items))
	for i, task := range page.Items {
		items[i] = newTaskResponse(task)
	}
	web.JSON(w, http.StatusOK, pagination.Page[taskResponse]{Items: items, NextCursor: page.NextCursor})
}

func (h handlers) get(w http.ResponseWriter, r *http.Request) {
	id, err := domain.ParseID(r.PathValue("id"))
	if err != nil {
		web.Error(w, r, err)
		return
	}
	task, err := h.tasks.Get(r.Context(), id)
	if err != nil {
		web.Error(w, r, err)
		return
	}
	web.JSON(w, http.StatusOK, newTaskResponse(task))
}

func (h handlers) complete(w http.ResponseWriter, r *http.Request) {
	id, err := domain.ParseID(r.PathValue("id"))
	if err != nil {
		web.Error(w, r, err)
		return
	}
	var body completeRequest
	if err := web.DecodeJSON(r, &body); err != nil {
		web.Error(w, r, err)
		return
	}
	task, err := h.tasks.Complete(r.Context(), id, body.Version)
	if err != nil {
		web.Error(w, r, err)
		return
	}
	web.JSON(w, http.StatusOK, newTaskResponse(task))
}

func newTaskResponse(task domain.Task) taskResponse {
	return taskResponse{
		ID:        task.ID,
		Title:     task.Title,
		Done:      task.Done,
		Version:   task.Version,
		CreatedAt: task.CreatedAt,
		UpdatedAt: task.UpdatedAt,
	}
}
