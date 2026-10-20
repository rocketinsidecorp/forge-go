// Package openapi describes Forge JSON routes as an OpenAPI 3.1 document
// (ADR 0043). It is opt-in and declarative: a route keeps its ordinary
// http.Handler and is wrapped with Describe, which records the operation's
// request, response and error types next to the route:
//
//	app.Handle("POST /v1/posts", openapi.Describe(create, openapi.Operation{
//	    Summary:    "Create a post",
//	    Tags:       []string{"posts"},
//	    Permission: "posts:write",
//	    Request:    openapi.TypeOf[postCreateRequest](),
//	    Responses:  []openapi.Response{openapi.JSON[postResponse](http.StatusCreated)},
//	    Errors:     []int{http.StatusUnprocessableEntity},
//	}))
//
// Typed goes further and derives the description from the handler's own
// signature, so the two cannot drift. Build reflects the Go types once (never
// per request) into JSON Schema 2020-12 following encoding/json's rules, and
// Document.ValidateResponse lets tests check real responses against it.
package openapi

import (
	"errors"
	"net/http"
	"reflect"
	"slices"

	"github.com/rocketinsidecorp/forge-go/web"
)

// Info is the document's info object.
type Info struct {
	// Title names the API; generated applications use their name.
	Title string
	// Version is the API's version, not the OpenAPI version.
	Version     string
	Description string
}

// Type names a Go type whose JSON encoding the document describes. The zero
// Type means "none".
type Type struct{ t reflect.Type }

// TypeOf returns the Type of T.
func TypeOf[T any]() Type { return Type{reflect.TypeFor[T]()} }

// IsZero reports whether no type is named.
func (t Type) IsZero() bool { return t.t == nil }

// String returns the Go type's name, or "" for the zero Type.
func (t Type) String() string {
	if t.t == nil {
		return ""
	}
	return t.t.String()
}

// Typer is implemented by types whose JSON encoding differs from their Go
// structure, typically because they implement json.Marshaler or
// json.Unmarshaler. OpenAPIType returns a type that encodes the same way, for
// example a pointer to the wrapped value for an optional-field wrapper. It is
// called on the zero value.
type Typer interface {
	OpenAPIType() Type
}

// Namer is implemented by struct types that choose their component name in
// #/components/schemas instead of their Go name, such as a generic or
// unexported type. It is called on the zero value and needs no import of
// this package.
type Namer interface {
	OpenAPIName() string
}

// Response is one documented response of an operation.
type Response struct {
	Status int
	// Description defaults to the status text, such as "Created".
	Description string
	// Body is the JSON body's type; the zero Type means no body.
	Body Type
	// Headers names response headers the operation sets, such as Location.
	Headers []string
}

// JSON returns a response with a JSON body of type T.
func JSON[T any](status int) Response { return Response{Status: status, Body: TypeOf[T]()} }

// Empty returns a response without a body, such as 204 No Content.
func Empty(status int) Response { return Response{Status: status} }

// Operation describes one route. Path parameters are taken from the route
// pattern; Params adds types and documentation to them and declares query and
// header parameters.
type Operation struct {
	// ID is the operationId; it defaults to the method and path, such as
	// "getV1PostsById". IDs must be unique.
	ID          string
	Summary     string
	Description string
	Tags        []string
	Deprecated  bool
	// Permission is the permission the route requires (x-forge-permission).
	// It implies a bearer token and the 401 and 403 errors.
	Permission string
	// Authenticated marks routes that need a bearer token but no specific
	// permission; it adds the 401 error.
	Authenticated bool
	// Params is a struct whose fields carry path:"name", query:"name" or
	// header:"Name" tags, plus optional doc, example, format and
	// openapi:"required" tags. Path parameters are always required.
	Params Type
	// Request is the JSON request body, decoded with web.DecodeJSON. It adds
	// the 400, 413 and 415 errors that decoding answers.
	Request Type
	// Responses are the responses with their own body type: successes,
	// redirects, and errors that are not web.ErrorBody (a readiness check
	// answering 503 with its report).
	Responses []Response
	// Errors are the error statuses the route answers with web.Error. Every
	// error response, and the "default" response, uses web.ErrorBody.
	Errors []int
}

func (o Operation) clone() Operation {
	o.Tags = slices.Clone(o.Tags)
	o.Errors = slices.Clone(o.Errors)
	o.Responses = slices.Clone(o.Responses)
	for i := range o.Responses {
		o.Responses[i].Headers = slices.Clone(o.Responses[i].Headers)
	}
	return o
}

// Described is a handler that carries its operation's description. Build
// documents the routes whose registered handler implements it.
type Described interface {
	http.Handler
	OpenAPIOperation() Operation
}

type described struct {
	http.Handler
	operation Operation
}

func (d described) OpenAPIOperation() Operation { return d.operation.clone() }

// Describe returns handler unchanged in behavior, carrying op. Wrap the
// handler that is registered on the route, after any per-route guard, so the
// router sees the description. It panics on a nil handler.
func Describe(handler http.Handler, op Operation) http.Handler {
	if handler == nil {
		panic("openapi: nil handler")
	}
	return described{Handler: handler, operation: op.clone()}
}

// NoBody is the request type of a Typed handler that reads no body.
type NoBody struct{}

// Typed returns a JSON handler whose description is derived from its
// signature: unless Req is NoBody, the body is decoded strictly into Req with
// web.DecodeJSON (and Req's Validate method, if it has one, is called); fn's
// result is written as JSON with status, and its error through web.Error.
// op.Request and the success response come from Req and Res, so the document
// cannot drift from the handler. It panics when status is not a 2xx status
// with a body or fn is nil.
func Typed[Req, Res any](status int, op Operation, fn func(r *http.Request, request Req) (Res, error)) http.Handler {
	if fn == nil {
		panic("openapi: nil Typed function")
	}
	if status < 200 || status > 299 || status == http.StatusNoContent || status == http.StatusResetContent {
		panic("openapi: Typed needs a 2xx status with a body")
	}
	decode := reflect.TypeFor[Req]() != reflect.TypeFor[NoBody]()
	op = op.clone()
	if decode {
		op.Request = TypeOf[Req]()
	}
	op.Responses = slices.DeleteFunc(op.Responses, func(r Response) bool { return r.Status == status })
	op.Responses = slices.Insert(op.Responses, 0, JSON[Res](status))
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request Req
		if decode {
			if err := web.DecodeJSON(r, &request); err != nil {
				web.Error(w, r, err)
				return
			}
			if validator, ok := any(&request).(interface{ Validate() error }); ok {
				if err := validator.Validate(); err != nil {
					web.Error(w, r, err)
					return
				}
			}
		}
		result, err := fn(r, request)
		if err != nil {
			web.Error(w, r, err)
			return
		}
		web.JSON(w, status, result)
	})
	return described{Handler: handler, operation: op}
}

// ErrNotDescribable reports a route or type the document cannot describe.
var ErrNotDescribable = errors.New("openapi: cannot describe")
