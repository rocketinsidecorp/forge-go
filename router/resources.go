package router

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Action is one conventional resource action.
type Action string

const (
	ActionIndex   Action = "index"
	ActionNew     Action = "new"
	ActionCreate  Action = "create"
	ActionShow    Action = "show"
	ActionEdit    Action = "edit"
	ActionUpdate  Action = "update"
	ActionDestroy Action = "destroy"
)

// ReadOnly reports whether the action only reads (index, new, show, edit),
// for example to choose between a read and a write permission.
func (a Action) ReadOnly() bool {
	switch a {
	case ActionIndex, ActionNew, ActionShow, ActionEdit:
		return true
	}
	return false
}

// A resource handler implements any subset of these interfaces; Resources
// registers a route for each one it implements.
type (
	ResourceIndex interface {
		Index(http.ResponseWriter, *http.Request)
	}
	ResourceNew interface {
		New(http.ResponseWriter, *http.Request)
	}
	ResourceCreate interface {
		Create(http.ResponseWriter, *http.Request)
	}
	ResourceShow interface {
		Show(http.ResponseWriter, *http.Request)
	}
	ResourceEdit interface {
		Edit(http.ResponseWriter, *http.Request)
	}
	ResourceUpdate interface {
		Update(http.ResponseWriter, *http.Request)
	}
	ResourceDestroy interface {
		Destroy(http.ResponseWriter, *http.Request)
	}
)

// ResourceRoute is one conventional route of a resource.
type ResourceRoute struct {
	Action  Action
	Method  string
	Pattern string // "/posts/{id}/edit"
}

// ResourceMiddleware wraps the handler of one action, for example to require
// a permission chosen by Action.ReadOnly. An error stops the registration.
type ResourceMiddleware func(action Action, next http.Handler) (http.Handler, error)

// ResourceRoutes returns the conventional routes for a resource at path
// ("/posts"), in this order:
//
//	GET    /posts            index
//	GET    /posts/new        new
//	POST   /posts            create
//	GET    /posts/{id}       show
//	GET    /posts/{id}/edit  edit
//	PATCH  /posts/{id}       update
//	PUT    /posts/{id}       update
//	DELETE /posts/{id}       destroy
//
// HTML forms reach PATCH, PUT and DELETE through web.MethodOverride. Path
// must be absolute, without a trailing slash, and may contain wildcards
// other than {id} ("/projects/{project}/tasks").
func ResourceRoutes(path string) ([]ResourceRoute, error) {
	if path == "/" || strings.HasSuffix(path, "/") || strings.Contains(path, "{id}") || strings.Contains(path, "{id...}") {
		return nil, fmt.Errorf("resource path %q must not be /, end with / or contain {id}", path)
	}
	if _, _, err := parsePattern(http.MethodGet + " " + path); err != nil {
		return nil, err
	}
	member := path + "/{id}"
	return []ResourceRoute{
		{ActionIndex, http.MethodGet, path},
		{ActionNew, http.MethodGet, path + "/new"},
		{ActionCreate, http.MethodPost, path},
		{ActionShow, http.MethodGet, member},
		{ActionEdit, http.MethodGet, member + "/edit"},
		{ActionUpdate, http.MethodPatch, member},
		{ActionUpdate, http.MethodPut, member},
		{ActionDestroy, http.MethodDelete, member},
	}, nil
}

// Resources registers the conventional routes (see ResourceRoutes) for the
// actions handler implements. It returns an error when handler implements
// none of them. Middleware wraps each action, the first outermost. All
// handlers are built before any route is registered; a registration error
// (such as a conflicting route) can still leave earlier routes of the
// resource registered, so treat it as fatal.
func (r *Router) Resources(path string, handler any, middleware ...ResourceMiddleware) error {
	if handler == nil {
		return errors.New("resource handler is required")
	}
	routes, err := ResourceRoutes(path)
	if err != nil {
		return err
	}
	type registration struct {
		pattern string
		handler http.Handler
	}
	var registrations []registration
	for _, route := range routes {
		action := actionHandler(handler, route.Action)
		if action == nil {
			continue
		}
		var wrapped http.Handler = action
		for i := len(middleware) - 1; i >= 0; i-- {
			if middleware[i] == nil {
				return errors.New("resource middleware is required")
			}
			if wrapped, err = middleware[i](route.Action, wrapped); err != nil {
				return fmt.Errorf("resource %s %s: %w", route.Action, path, err)
			}
			if wrapped == nil {
				return fmt.Errorf("resource %s %s: middleware returned a nil handler", route.Action, path)
			}
		}
		registrations = append(registrations, registration{route.Method + " " + route.Pattern, wrapped})
	}
	if len(registrations) == 0 {
		return fmt.Errorf("resource handler %T implements no resource action", handler)
	}
	for _, current := range registrations {
		if err := r.Handle(current.pattern, current.handler); err != nil {
			return err
		}
	}
	return nil
}

func actionHandler(handler any, action Action) http.HandlerFunc {
	switch action {
	case ActionIndex:
		if h, ok := handler.(ResourceIndex); ok {
			return h.Index
		}
	case ActionNew:
		if h, ok := handler.(ResourceNew); ok {
			return h.New
		}
	case ActionCreate:
		if h, ok := handler.(ResourceCreate); ok {
			return h.Create
		}
	case ActionShow:
		if h, ok := handler.(ResourceShow); ok {
			return h.Show
		}
	case ActionEdit:
		if h, ok := handler.(ResourceEdit); ok {
			return h.Edit
		}
	case ActionUpdate:
		if h, ok := handler.(ResourceUpdate); ok {
			return h.Update
		}
	case ActionDestroy:
		if h, ok := handler.(ResourceDestroy); ok {
			return h.Destroy
		}
	}
	return nil
}
