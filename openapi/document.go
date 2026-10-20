package openapi

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/rocketinsidecorp/forge-go/web"
)

// Version is the OpenAPI version of the documents Build produces.
const Version = "3.1.0"

const (
	jsonMediaType = "application/json"
	bearerScheme  = "bearerAuth"
)

var errorBodyType = reflect.TypeFor[web.ErrorBody]()

// Route is a registered route: its ServeMux method and path, and the handler
// as given to the router. Build documents it when the handler is Described.
type Route struct {
	Method  string
	Pattern string
	Handler http.Handler
}

// Document is a built OpenAPI document.
type Document struct {
	json         []byte
	tree         map[string]any
	undocumented []string
	// matcher maps a request to the documented route pattern that serves
	// it, with the router's own matching rules.
	matcher  *http.ServeMux
	patterns map[string]documentedRoute
}

type documentedRoute struct {
	method, path string
}

// JSON returns the document, indented, with object keys sorted, so the same
// routes and types always produce the same bytes and the file can be
// committed and diffed.
func (d *Document) JSON() []byte { return bytes.Clone(d.json) }

// Undocumented returns the "METHOD /path" patterns of the routes that carry
// no description, sorted.
func (d *Document) Undocumented() []string { return slices.Clone(d.undocumented) }

// Paths returns the documented OpenAPI paths, sorted.
func (d *Document) Paths() []string {
	paths, _ := d.tree["paths"].(map[string]any)
	result := make([]string, 0, len(paths))
	for path := range paths {
		result = append(result, path)
	}
	slices.Sort(result)
	return result
}

// Handler serves the document as application/json.
func (d *Document) Handler() http.Handler {
	body := d.JSON()
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", jsonMediaType)
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(body)
	})
}

// Build describes the routes whose handler is Described. Routes are
// documented in path and method order regardless of the order given. A
// description the document cannot express accurately (an unsupported type,
// a path parameter the pattern lacks, a duplicate operation ID) is an error
// rather than a guess.
func Build(info Info, routes []Route) (*Document, error) {
	if strings.TrimSpace(info.Title) == "" || strings.TrimSpace(info.Version) == "" {
		return nil, errors.New("openapi: the document needs a title and a version")
	}
	types := newSchemas()
	errorSchema, err := types.schema(errorBodyType)
	if err != nil {
		return nil, err
	}
	sorted := slices.Clone(routes)
	slices.SortStableFunc(sorted, func(a, b Route) int {
		return cmp.Or(strings.Compare(a.Pattern, b.Pattern), strings.Compare(a.Method, b.Method))
	})
	document := &Document{matcher: http.NewServeMux(), patterns: map[string]documentedRoute{}}
	paths := map[string]map[string]any{}
	operationIDs := map[string]string{}
	usesBearer := false
	for _, route := range sorted {
		pattern := route.Method + " " + route.Pattern
		described, ok := route.Handler.(Described)
		if !ok {
			document.undocumented = append(document.undocumented, pattern)
			continue
		}
		operation := described.OpenAPIOperation()
		path, wildcards, err := openAPIPath(route.Pattern)
		if err != nil {
			return nil, fmt.Errorf("openapi: %s: %w", pattern, err)
		}
		method := strings.ToLower(route.Method)
		if !slices.Contains([]string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}, method) {
			return nil, fmt.Errorf("%w %s: OpenAPI 3.1 has no %s operations", ErrNotDescribable, pattern, route.Method)
		}
		built, err := types.operation(method, path, wildcards, operation, errorSchema)
		if err != nil {
			return nil, fmt.Errorf("openapi: %s: %w", pattern, err)
		}
		id := built["operationId"].(string)
		if previous, taken := operationIDs[id]; taken {
			return nil, fmt.Errorf("openapi: %s and %s share the operation ID %q; set Operation.ID", previous, pattern, id)
		}
		operationIDs[id] = pattern
		usesBearer = usesBearer || operation.Permission != "" || operation.Authenticated
		if paths[path] == nil {
			paths[path] = map[string]any{}
		}
		paths[path][method] = built
		if err := document.match(pattern); err != nil {
			return nil, err
		}
		document.patterns[pattern] = documentedRoute{method: method, path: path}
	}
	components := map[string]any{"schemas": types.assignNames()}
	if usesBearer {
		components["securitySchemes"] = map[string]any{
			bearerScheme: map[string]any{"type": "http", "scheme": "bearer", "description": "A Forge session token: Authorization: Bearer <token>."},
		}
	}
	infoObject := map[string]any{"title": info.Title, "version": info.Version}
	if info.Description != "" {
		infoObject["description"] = info.Description
	}
	tree := map[string]any{
		"openapi":           Version,
		"jsonSchemaDialect": "https://json-schema.org/draft/2020-12/schema",
		"info":              infoObject,
		"paths":             paths,
		"components":        components,
	}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(tree); err != nil {
		return nil, fmt.Errorf("openapi: encode document: %w", err)
	}
	document.json = encoded.Bytes()
	// The validator works on the published bytes, not on the Go values
	// that produced them.
	decoder := json.NewDecoder(bytes.NewReader(document.json))
	decoder.UseNumber()
	if err := decoder.Decode(&document.tree); err != nil {
		return nil, fmt.Errorf("openapi: decode document: %w", err)
	}
	return document, nil
}

// match registers pattern on the matcher; ServeMux panics on conflicting
// patterns, which only routes given to Build directly (not taken from one
// router) can contain.
func (d *Document) match(pattern string) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("openapi: route %s: %v", pattern, recovered)
		}
	}()
	d.matcher.Handle(pattern, http.NotFoundHandler())
	return nil
}

// openAPIPath converts a ServeMux path to an OpenAPI path and lists its
// wildcards: {name...} becomes {name} and {$} is dropped.
func openAPIPath(pattern string) (string, []string, error) {
	var wildcards []string
	segments := strings.Split(pattern, "/")
	for i, segment := range segments {
		if !strings.Contains(segment, "{") {
			continue
		}
		if !strings.HasPrefix(segment, "{") || !strings.HasSuffix(segment, "}") {
			return "", nil, fmt.Errorf("%w: wildcard segment %q", ErrNotDescribable, segment)
		}
		name := strings.TrimSuffix(segment[1:len(segment)-1], "...")
		if name == "$" {
			segments[i] = ""
			continue
		}
		wildcards = append(wildcards, name)
		segments[i] = "{" + name + "}"
	}
	return strings.Join(segments, "/"), wildcards, nil
}

func (s *schemas) operation(method, path string, wildcards []string, op Operation, errorSchema map[string]any) (map[string]any, error) {
	result := map[string]any{"operationId": cmp.Or(op.ID, operationID(method, path))}
	if op.Summary != "" {
		result["summary"] = op.Summary
	}
	if op.Description != "" {
		result["description"] = op.Description
	}
	if len(op.Tags) > 0 {
		result["tags"] = op.Tags
	}
	if op.Deprecated {
		result["deprecated"] = true
	}
	parameters, err := s.parameters(wildcards, op.Params)
	if err != nil {
		return nil, err
	}
	if len(parameters) > 0 {
		result["parameters"] = parameters
	}
	// Errors the framework answers on its own for this kind of operation;
	// a declared response with the same status takes precedence.
	var implied []int
	if !op.Request.IsZero() {
		body, err := s.schema(op.Request.t)
		if err != nil {
			return nil, err
		}
		result["requestBody"] = map[string]any{"required": true, "content": map[string]any{jsonMediaType: map[string]any{"schema": body}}}
		implied = append(implied, http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnsupportedMediaType)
	}
	if op.Permission != "" || op.Authenticated {
		result["security"] = []any{map[string]any{bearerScheme: []string{}}}
		implied = append(implied, http.StatusUnauthorized)
	}
	if op.Permission != "" {
		result["x-forge-permission"] = op.Permission
		implied = append(implied, http.StatusForbidden)
	}
	if len(op.Responses) == 0 {
		return nil, fmt.Errorf("%w: the operation declares no response", ErrNotDescribable)
	}
	responses := map[string]any{}
	for _, response := range op.Responses {
		if response.Status < 100 || response.Status > 599 {
			return nil, fmt.Errorf("%w: response status %d is not an HTTP status", ErrNotDescribable, response.Status)
		}
		key := strconv.Itoa(response.Status)
		if _, duplicate := responses[key]; duplicate {
			return nil, fmt.Errorf("%w: response %d is declared twice", ErrNotDescribable, response.Status)
		}
		object := map[string]any{"description": cmp.Or(response.Description, http.StatusText(response.Status), key)}
		if !response.Body.IsZero() {
			if response.Status == http.StatusNoContent || response.Status == http.StatusNotModified {
				return nil, fmt.Errorf("%w: a %d response has no body", ErrNotDescribable, response.Status)
			}
			body, err := s.schema(response.Body.t)
			if err != nil {
				return nil, err
			}
			object["content"] = map[string]any{jsonMediaType: map[string]any{"schema": body}}
		}
		if len(response.Headers) > 0 {
			headers := map[string]any{}
			for _, header := range response.Headers {
				headers[http.CanonicalHeaderKey(header)] = map[string]any{"schema": map[string]any{"type": "string"}}
			}
			object["headers"] = headers
		}
		responses[key] = object
	}
	for _, status := range op.Errors {
		if status < 400 || status > 599 {
			return nil, fmt.Errorf("%w: error status %d is not 4xx or 5xx", ErrNotDescribable, status)
		}
		key := strconv.Itoa(status)
		if _, declared := responses[key]; declared {
			return nil, fmt.Errorf("%w: status %d is both a response and an error", ErrNotDescribable, status)
		}
		responses[key] = errorResponse(http.StatusText(status), errorSchema)
	}
	for _, status := range implied {
		key := strconv.Itoa(status)
		if _, declared := responses[key]; !declared {
			responses[key] = errorResponse(http.StatusText(status), errorSchema)
		}
	}
	responses["default"] = errorResponse("Error", errorSchema)
	result["responses"] = responses
	return result, nil
}

func errorResponse(description string, schema map[string]any) map[string]any {
	return map[string]any{"description": description, "content": map[string]any{jsonMediaType: map[string]any{"schema": schema}}}
}

// parameters documents the path wildcards and the Params type's fields.
func (s *schemas) parameters(wildcards []string, params Type) ([]any, error) {
	type parameter struct {
		in, name string
		object   map[string]any
	}
	var list []parameter
	declaredPath := map[string]bool{}
	if !params.IsZero() {
		t := params.t
		if t.Kind() != reflect.Struct {
			return nil, fmt.Errorf("%w: Params must be a struct, not %s", ErrNotDescribable, t)
		}
		var walk func(reflect.Type) error
		walk = func(t reflect.Type) error {
			for i := range t.NumField() {
				sf := t.Field(i)
				if sf.Anonymous && sf.Type.Kind() == reflect.Struct {
					if err := walk(sf.Type); err != nil {
						return err
					}
					continue
				}
				in, name := "", ""
				for _, location := range []string{"path", "query", "header"} {
					if value, ok := sf.Tag.Lookup(location); ok {
						if in != "" {
							return fmt.Errorf("%w: field %s has more than one of the path, query and header tags", ErrNotDescribable, sf.Name)
						}
						in, name = location, value
					}
				}
				if in == "" {
					continue
				}
				if name == "" {
					return fmt.Errorf("%w: field %s has an empty %s tag", ErrNotDescribable, sf.Name, in)
				}
				schema, err := s.schema(sf.Type)
				if err != nil {
					return err
				}
				options, err := openapiOptions(sf.Tag)
				if err != nil {
					return err
				}
				// A parameter is text and is never null; an absent one is
				// expressed by required.
				description, documented := sf.Tag.Lookup("doc")
				schema = annotate(nonNull(schema), sf.Tag, options)
				object := map[string]any{"name": name, "in": in, "schema": schema}
				if documented {
					// The doc tag describes the parameter, not its type.
					object["description"] = description
					delete(schema, "description")
				}
				switch {
				case in == "path":
					if !slices.Contains(wildcards, name) {
						return fmt.Errorf("%w: path parameter %q is not a wildcard of the route", ErrNotDescribable, name)
					}
					declaredPath[name] = true
					object["required"] = true
				case options.required:
					object["required"] = true
				}
				list = append(list, parameter{in, name, object})
			}
			return nil
		}
		if err := walk(t); err != nil {
			return nil, err
		}
	}
	for _, wildcard := range wildcards {
		if !declaredPath[wildcard] {
			list = append(list, parameter{"path", wildcard, map[string]any{"name": wildcard, "in": "path", "required": true, "schema": map[string]any{"type": "string"}}})
		}
	}
	slices.SortFunc(list, func(a, b parameter) int {
		return cmp.Or(strings.Compare(a.in, b.in), strings.Compare(a.name, b.name))
	})
	result := make([]any, 0, len(list))
	for i, current := range list {
		if i > 0 && current.in == list[i-1].in && current.name == list[i-1].name {
			return nil, fmt.Errorf("%w: %s parameter %q is declared twice", ErrNotDescribable, current.in, current.name)
		}
		result = append(result, current.object)
	}
	return result, nil
}

// operationID derives "getV1PostsById" from GET /v1/posts/{id}.
func operationID(method, path string) string {
	var builder strings.Builder
	builder.WriteString(method)
	for segment := range strings.SplitSeq(path, "/") {
		if strings.HasPrefix(segment, "{") {
			builder.WriteString("By")
			segment = strings.Trim(segment, "{}")
		}
		for word := range strings.FieldsFuncSeq(segment, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
			builder.WriteString(upperFirst(word))
		}
	}
	return builder.String()
}
