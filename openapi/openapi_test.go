package openapi_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/fault"
	"github.com/rocketinsidecorp/forge-go/openapi"
	"github.com/rocketinsidecorp/forge-go/pagination"
	"github.com/rocketinsidecorp/forge-go/uuid"
	"github.com/rocketinsidecorp/forge-go/web"
)

type node struct {
	Name     string  `json:"name" doc:"The node's name." example:"root"`
	Children []*node `json:"children,omitempty"`
	Parent   *node   `json:"parent"`
}

type audit struct {
	CreatedAt time.Time  `json:"created_at"`
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
}

type secret struct {
	Hidden string `json:"hidden"`
}

type kitchenSink struct {
	audit
	*secret
	ID        uuid.UUID         `json:"id"`
	OwnerID   *uuid.UUID        `json:"owner_id"`
	Count     int               `json:"count"`
	Small     int8              `json:"small"`
	Ratio     float64           `json:"ratio"`
	Enabled   bool              `json:"enabled"`
	Tags      []string          `json:"tags"`
	Labels    map[string]int    `json:"labels,omitempty"`
	Raw       json.RawMessage   `json:"raw"`
	Blob      []byte            `json:"blob"`
	Pair      [2]int            `json:"pair"`
	Big       int64             `json:"big,string"`
	Anything  any               `json:"anything"`
	Elapsed   time.Duration     `json:"elapsed"`
	Root      node              `json:"root"`
	Inline    struct{ A int }   `json:"inline"`
	Required  *string           `json:"required" openapi:"required,nonnull"`
	Skipped   string            `json:"-"`
	Formatted string            `json:"formatted" format:"email"`
	Patch     optional[string]  `json:"patch,omitzero"`
	Nested    map[string][]node `json:"nested"`
	private   int               // unexported: never encoded
}

// optional mimics a generated PATCH field: it decodes itself.
type optional[T any] struct {
	Set   bool
	Value *T
}

func (o *optional[T]) UnmarshalJSON(data []byte) error {
	o.Set = true
	return json.Unmarshal(data, &o.Value)
}

func (optional[T]) OpenAPIType() openapi.Type { return openapi.TypeOf[*T]() }

type sinkParams struct {
	ID     uuid.UUID `path:"id" doc:"The sink."`
	Limit  int       `query:"limit"`
	Cursor *string   `query:"cursor" openapi:"required"`
	Trace  string    `header:"X-Trace"`
}

func build(t *testing.T, routes ...openapi.Route) (*openapi.Document, map[string]any) {
	t.Helper()
	document, err := openapi.Build(openapi.Info{Title: "test", Version: "1.0.0"}, routes)
	if err != nil {
		t.Fatal(err)
	}
	var tree map[string]any
	if err := json.Unmarshal(document.JSON(), &tree); err != nil {
		t.Fatalf("document is not JSON: %v", err)
	}
	return document, tree
}

var _ = kitchenSink{private: 0}

func at(t *testing.T, value any, path ...string) any {
	t.Helper()
	for _, key := range path {
		object, ok := value.(map[string]any)
		if !ok {
			t.Fatalf("%v: not an object at %q", path, key)
		}
		value, ok = object[key]
		if !ok {
			t.Fatalf("%v: missing %q in %v", path, key, object)
		}
	}
	return value
}

func compact(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

var ok = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

func sinkRoute() openapi.Route {
	return openapi.Route{Method: http.MethodPut, Pattern: "/v1/sinks/{id}", Handler: openapi.Describe(ok, openapi.Operation{
		Summary:    "Replace a sink",
		Tags:       []string{"sinks"},
		Permission: "sinks:write",
		Params:     openapi.TypeOf[sinkParams](),
		Request:    openapi.TypeOf[kitchenSink](),
		Responses:  []openapi.Response{{Status: http.StatusOK, Body: openapi.TypeOf[kitchenSink](), Headers: []string{"location"}}},
		Errors:     []int{http.StatusNotFound},
	})}
}

func TestSchemasFollowEncodingJSON(t *testing.T) {
	t.Parallel()
	_, tree := build(t, sinkRoute())
	sink := at(t, tree, "components", "schemas", "KitchenSink").(map[string]any)
	properties := at(t, sink, "properties").(map[string]any)
	want := map[string]string{
		"id":         `{"description":"UUIDv7 (RFC 9562)","format":"uuid","pattern":"^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$","type":"string"}`,
		"owner_id":   `{"description":"UUIDv7 (RFC 9562)","format":"uuid","pattern":"^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$","type":["string","null"]}`,
		"count":      `{"format":"int64","type":"integer"}`,
		"small":      `{"format":"int32","maximum":127,"minimum":-128,"type":"integer"}`,
		"ratio":      `{"format":"double","type":"number"}`,
		"enabled":    `{"type":"boolean"}`,
		"tags":       `{"items":{"type":"string"},"type":["array","null"]}`,
		"labels":     `{"additionalProperties":{"format":"int64","type":"integer"},"type":["object","null"]}`,
		"raw":        `{}`,
		"blob":       `{"contentEncoding":"base64","type":["string","null"]}`,
		"pair":       `{"items":{"format":"int64","type":"integer"},"maxItems":2,"minItems":2,"type":"array"}`,
		"big":        `{"type":"string"}`,
		"anything":   `{}`,
		"elapsed":    `{"format":"int64","type":"integer"}`,
		"root":       `{"$ref":"#/components/schemas/Node"}`,
		"inline":     `{"additionalProperties":false,"properties":{"A":{"format":"int64","type":"integer"}},"required":["A"],"type":"object"}`,
		"required":   `{"type":"string"}`,
		"formatted":  `{"format":"email","type":"string"}`,
		"patch":      `{"type":["string","null"]}`,
		"nested":     `{"additionalProperties":{"items":{"$ref":"#/components/schemas/Node"},"type":["array","null"]},"type":["object","null"]}`,
		"created_at": `{"format":"date-time","type":"string"}`,
		"deleted_at": `{"format":"date-time","type":["string","null"]}`,
		"hidden":     `{"type":"string"}`,
	}
	for name, schema := range want {
		if got := compact(t, properties[name]); got != schema {
			t.Errorf("%s = %s\nwant %s", name, got, schema)
		}
	}
	if len(properties) != len(want) {
		t.Errorf("properties = %v, want exactly %d", slices.Sorted(func(yield func(string) bool) {
			for key := range properties {
				if !yield(key) {
					return
				}
			}
		}), len(want))
	}
	// Absent: omitempty, omitzero and fields promoted through a nil-able
	// embedded pointer. openapi:"required" overrides a pointer.
	wantRequired := `["anything","big","blob","count","created_at","elapsed","enabled","formatted","id","inline","nested","owner_id","pair","ratio","raw","required","root","small","tags"]`
	if got := compact(t, sink["required"]); got != wantRequired {
		t.Errorf("required = %s\nwant %s", got, wantRequired)
	}
	if sink["additionalProperties"] != false {
		t.Errorf("additionalProperties = %v", sink["additionalProperties"])
	}
	// Recursion through $ref, nullable references, doc and example tags.
	wantNode := `{"additionalProperties":false,"properties":{"children":{"items":{"anyOf":[{"$ref":"#/components/schemas/Node"},{"type":"null"}]},"type":["array","null"]},"name":{"description":"The node's name.","examples":["root"],"type":"string"},"parent":{"anyOf":[{"$ref":"#/components/schemas/Node"},{"type":"null"}]}},"required":["name","parent"],"type":"object"}`
	if got := compact(t, at(t, tree, "components", "schemas", "Node")); got != wantNode {
		t.Errorf("Node = %s\nwant %s", got, wantNode)
	}
}

func TestOperationsDocumentParametersErrorsAndSecurity(t *testing.T) {
	t.Parallel()
	document, tree := build(t, sinkRoute(), openapi.Route{Method: http.MethodGet, Pattern: "/health", Handler: ok})
	operation := at(t, tree, "paths", "/v1/sinks/{id}", "put").(map[string]any)
	if operation["operationId"] != "putV1SinksById" || operation["summary"] != "Replace a sink" || operation["x-forge-permission"] != "sinks:write" {
		t.Fatalf("operation = %v", operation)
	}
	if got := compact(t, operation["security"]); got != `[{"bearerAuth":[]}]` {
		t.Fatalf("security = %s", got)
	}
	wantParameters := `[{"in":"header","name":"X-Trace","schema":{"type":"string"}},` +
		`{"in":"path","name":"id","description":"The sink.","required":true,"schema":{"format":"uuid","pattern":"^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$","type":"string"}},` +
		`{"in":"query","name":"cursor","required":true,"schema":{"type":"string"}},` +
		`{"in":"query","name":"limit","schema":{"format":"int64","type":"integer"}}]`
	var normalized any
	if err := json.Unmarshal([]byte(wantParameters), &normalized); err != nil {
		t.Fatal(err)
	}
	if got, want := compact(t, operation["parameters"]), compact(t, normalized); got != want {
		t.Fatalf("parameters = %s\nwant %s", got, want)
	}
	responses := at(t, operation, "responses").(map[string]any)
	for _, status := range []string{"200", "400", "401", "403", "404", "413", "415", "default"} {
		if _, ok := responses[status]; !ok {
			t.Errorf("response %s is missing: %v", status, responses)
		}
	}
	if len(responses) != 8 {
		t.Errorf("responses = %v", responses)
	}
	if got := compact(t, at(t, responses, "404", "content", "application/json", "schema")); got != `{"$ref":"#/components/schemas/ErrorBody"}` {
		t.Errorf("404 schema = %s", got)
	}
	if got := compact(t, at(t, responses, "200", "headers")); got != `{"Location":{"schema":{"type":"string"}}}` {
		t.Errorf("200 headers = %s", got)
	}
	if got := compact(t, at(t, tree, "components", "securitySchemes", "bearerAuth", "scheme")); got != `"bearer"` {
		t.Errorf("bearer scheme = %s", got)
	}
	if got := document.Undocumented(); !slices.Equal(got, []string{"GET /health"}) {
		t.Errorf("Undocumented = %v", got)
	}
	if got := document.Paths(); !slices.Equal(got, []string{"/v1/sinks/{id}"}) {
		t.Errorf("Paths = %v", got)
	}
	if tree["openapi"] != "3.1.0" || compact(t, tree["info"]) != `{"title":"test","version":"1.0.0"}` {
		t.Errorf("header = %v %v", tree["openapi"], tree["info"])
	}
}

func TestBuildIsDeterministic(t *testing.T) {
	t.Parallel()
	routes := []openapi.Route{
		sinkRoute(),
		{Method: http.MethodGet, Pattern: "/v1/nodes/{path...}", Handler: openapi.Describe(ok, openapi.Operation{Responses: []openapi.Response{openapi.JSON[node](http.StatusOK)}})},
		{Method: http.MethodDelete, Pattern: "/v1/nodes/{path...}", Handler: openapi.Describe(ok, openapi.Operation{Responses: []openapi.Response{openapi.Empty(http.StatusNoContent)}})},
		{Method: http.MethodGet, Pattern: "/{$}", Handler: openapi.Describe(ok, openapi.Operation{ID: "home", Responses: []openapi.Response{openapi.JSON[map[string]string](http.StatusOK)}})},
	}
	first, tree := build(t, routes...)
	for range 5 {
		reversed := slices.Clone(routes)
		slices.Reverse(reversed)
		again, _ := build(t, reversed...)
		if !bytes.Equal(first.JSON(), again.JSON()) {
			t.Fatal("the document depends on route order")
		}
	}
	if got := first.Paths(); !slices.Equal(got, []string{"/", "/v1/nodes/{path}", "/v1/sinks/{id}"}) {
		t.Fatalf("paths = %v", got)
	}
	if got := at(t, tree, "paths", "/v1/nodes/{path}", "delete", "operationId"); got != "deleteV1NodesByPath" {
		t.Fatalf("operationId = %v", got)
	}
	if _, hasContent := at(t, tree, "paths", "/v1/nodes/{path}", "delete", "responses", "204").(map[string]any)["content"]; hasContent {
		t.Fatal("204 has content")
	}
	if !bytes.HasSuffix(first.JSON(), []byte("}\n")) || !bytes.Contains(first.JSON(), []byte("\n  \"components\": {")) {
		t.Fatalf("document is not indented:\n%s", first.JSON())
	}
}

type first struct{ A int }

func TestDescriptionsThatCannotBeAccurateAreErrors(t *testing.T) {
	t.Parallel()
	describe := func(op openapi.Operation) http.Handler { return openapi.Describe(ok, op) }
	okResponse := []openapi.Response{openapi.Empty(http.StatusNoContent)}
	tests := map[string]struct {
		routes []openapi.Route
		want   string
	}{
		"unknown path parameter": {[]openapi.Route{{Method: "GET", Pattern: "/a", Handler: describe(openapi.Operation{Params: openapi.TypeOf[sinkParams](), Responses: okResponse})}}, "not a wildcard"},
		"channel":                {[]openapi.Route{{Method: "GET", Pattern: "/a", Handler: describe(openapi.Operation{Responses: []openapi.Response{openapi.JSON[chan int](http.StatusOK)}})}}, "cannot encode chan"},
		"no response":            {[]openapi.Route{{Method: "GET", Pattern: "/a", Handler: describe(openapi.Operation{})}}, "declares no response"},
		"response and error":     {[]openapi.Route{{Method: "GET", Pattern: "/a", Handler: describe(openapi.Operation{Responses: []openapi.Response{openapi.Empty(http.StatusNotFound)}, Errors: []int{http.StatusNotFound}})}}, "both a response and an error"},
		"success as error":       {[]openapi.Route{{Method: "GET", Pattern: "/a", Handler: describe(openapi.Operation{Responses: okResponse, Errors: []int{http.StatusOK}})}}, "not 4xx"},
		"body on 204":            {[]openapi.Route{{Method: "GET", Pattern: "/a", Handler: describe(openapi.Operation{Responses: []openapi.Response{openapi.JSON[first](http.StatusNoContent)}})}}, "has no body"},
		"bad tag option": {[]openapi.Route{{Method: "GET", Pattern: "/a", Handler: describe(openapi.Operation{Responses: []openapi.Response{openapi.JSON[struct {
			A string `openapi:"requird"`
		}](http.StatusOK)}})}}, "unknown openapi tag option"},
		"duplicate operation id": {[]openapi.Route{
			{Method: "GET", Pattern: "/a", Handler: describe(openapi.Operation{ID: "same", Responses: okResponse})},
			{Method: "GET", Pattern: "/b", Handler: describe(openapi.Operation{ID: "same", Responses: okResponse})},
		}, "share the operation ID"},
		"unsupported method": {[]openapi.Route{{Method: "QUERY", Pattern: "/a", Handler: describe(openapi.Operation{Responses: okResponse})}}, "no QUERY operations"},
		"map key":            {[]openapi.Route{{Method: "GET", Pattern: "/a", Handler: describe(openapi.Operation{Responses: []openapi.Response{openapi.JSON[map[first]int](http.StatusOK)}})}}, "map keys"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := openapi.Build(openapi.Info{Title: "t", Version: "1"}, tt.routes)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tt.want)
			}
		})
	}
	if _, err := openapi.Build(openapi.Info{}, nil); err == nil {
		t.Fatal("a document without a title was built")
	}
}

// Types with the same name in different packages are qualified; here the
// collision is between a local type and web.ErrorBody's detail.
type ErrorDetail struct {
	Field string `json:"field"`
}

func TestCollidingComponentNamesAreQualified(t *testing.T) {
	t.Parallel()
	_, tree := build(t, openapi.Route{Method: "GET", Pattern: "/a", Handler: openapi.Describe(ok, openapi.Operation{Responses: []openapi.Response{openapi.JSON[ErrorDetail](http.StatusOK)}})})
	schemas := at(t, tree, "components", "schemas").(map[string]any)
	for _, name := range []string{"ErrorBody", "openapi_test.ErrorDetail", "web.ErrorDetail"} {
		if _, ok := schemas[name]; !ok {
			t.Errorf("component %s is missing: %v", name, schemas)
		}
	}
}

type createRequest struct {
	Name string `json:"name"`
}

func (c createRequest) Validate() error {
	if c.Name == "" {
		return fault.New("invalid_name", "name is required", fault.CategoryInvalid, http.StatusUnprocessableEntity)
	}
	return nil
}

type created struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

func TestTypedHandlersAreDescribedByTheirSignature(t *testing.T) {
	t.Parallel()
	id := uuid.MustNew()
	handler := openapi.Typed(http.StatusCreated, openapi.Operation{Summary: "Create", Errors: []int{http.StatusUnprocessableEntity}},
		func(_ *http.Request, request createRequest) (created, error) {
			return created{ID: id, Name: request.Name}, nil
		})
	described, ok := handler.(openapi.Described)
	if !ok {
		t.Fatal("Typed handler is not Described")
	}
	operation := described.OpenAPIOperation()
	if operation.Request != openapi.TypeOf[createRequest]() || len(operation.Responses) != 1 || operation.Responses[0].Status != http.StatusCreated || operation.Responses[0].Body != openapi.TypeOf[created]() {
		t.Fatalf("operation = %+v", operation)
	}
	document, _ := build(t, openapi.Route{Method: http.MethodPost, Pattern: "/things", Handler: handler})

	send := func(body, contentType string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/things", strings.NewReader(body))
		request.Header.Set("Content-Type", contentType)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if err := document.ValidateResponse(http.MethodPost, "/things", recorder.Code, recorder.Header(), recorder.Body.Bytes()); err != nil {
			t.Errorf("%s: %v", body, err)
		}
		return recorder
	}
	if recorder := send(`{"name":"a"}`, "application/json"); recorder.Code != http.StatusCreated || !strings.Contains(recorder.Body.String(), id.String()) {
		t.Fatalf("create: %d %s", recorder.Code, recorder.Body)
	}
	if recorder := send(`{"name":""}`, "application/json"); recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("Validate was not called: %d %s", recorder.Code, recorder.Body)
	}
	if recorder := send(`{"name":"a","extra":1}`, "application/json"); recorder.Code != http.StatusBadRequest {
		t.Fatalf("unknown member: %d", recorder.Code)
	}
	if recorder := send(`name=a`, "application/x-www-form-urlencoded"); recorder.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("form body: %d", recorder.Code)
	}

	noBody := openapi.Typed(http.StatusOK, openapi.Operation{}, func(*http.Request, openapi.NoBody) (created, error) { return created{ID: id}, nil })
	if operation := noBody.(openapi.Described).OpenAPIOperation(); !operation.Request.IsZero() {
		t.Fatalf("NoBody handler documents a request: %+v", operation)
	}
	recorder := httptest.NewRecorder()
	noBody.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("NoBody handler: %d %s", recorder.Code, recorder.Body)
	}
	for _, status := range []int{http.StatusNoContent, http.StatusNotFound} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("Typed accepted status %d", status)
				}
			}()
			openapi.Typed(status, openapi.Operation{}, func(*http.Request, openapi.NoBody) (created, error) { return created{}, nil })
		}()
	}
}

func TestDescribeKeepsBehaviorAndCopiesTheOperation(t *testing.T) {
	t.Parallel()
	tags := []string{"a"}
	handler := openapi.Describe(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }), openapi.Operation{Tags: tags})
	tags[0] = "changed"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusTeapot {
		t.Fatalf("status = %d", recorder.Code)
	}
	operation := handler.(openapi.Described).OpenAPIOperation()
	operation.Tags[0] = "mutated"
	if got := handler.(openapi.Described).OpenAPIOperation().Tags; !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("tags = %v", got)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("Describe accepted a nil handler")
		}
	}()
	openapi.Describe(nil, openapi.Operation{})
}

func TestValidateResponseRejectsMismatches(t *testing.T) {
	t.Parallel()
	document, _ := build(t, sinkRoute(),
		openapi.Route{Method: http.MethodDelete, Pattern: "/v1/sinks/{id}", Handler: openapi.Describe(ok, openapi.Operation{Responses: []openapi.Response{openapi.Empty(http.StatusNoContent)}})})
	id := uuid.MustNew().String()
	valid := `{"id":"` + id + `","owner_id":null,"count":1,"small":2,"ratio":0.5,"enabled":true,"tags":null,"raw":[1],"blob":"aGk=","pair":[1,2],"big":"9","anything":{},"elapsed":5,` +
		`"root":{"name":"r","parent":null,"children":[{"name":"c","parent":null}]},"inline":{"A":1},"required":"x","formatted":"a@b.c","nested":{"k":[]},"created_at":"2026-10-20T10:00:00Z"}`
	jsonHeader := http.Header{"Content-Type": {"application/json; charset=utf-8"}}
	path := "/v1/sinks/" + id
	if err := document.ValidateResponse(http.MethodPut, path, http.StatusOK, jsonHeader, []byte(valid)); err != nil {
		t.Fatalf("valid body rejected: %v", err)
	}
	if err := document.ValidateResponse(http.MethodPut, path, http.StatusNotFound, jsonHeader, []byte(`{"error":{"code":"not_found","message":"m"}}`)); err != nil {
		t.Fatalf("valid error rejected: %v", err)
	}
	if err := document.ValidateResponse(http.MethodDelete, path, http.StatusNoContent, http.Header{}, nil); err != nil {
		t.Fatalf("empty 204 rejected: %v", err)
	}
	replace := func(old, replacement string) []byte { return []byte(strings.Replace(valid, old, replacement, 1)) }
	tests := map[string]struct {
		method string
		path   string
		status int
		header http.Header
		body   []byte
		want   string
	}{
		"wrong type":        {http.MethodPut, path, 200, jsonHeader, replace(`"count":1`, `"count":"1"`), "/count"},
		"fraction":          {http.MethodPut, path, 200, jsonHeader, replace(`"count":1`, `"count":1.5`), "integer"},
		"missing required":  {http.MethodPut, path, 200, jsonHeader, replace(`"count":1,`, ``), `"count" is missing`},
		"unknown member":    {http.MethodPut, path, 200, jsonHeader, replace(`"count":1`, `"count":1,"extra":2`), `"extra" is not in the schema`},
		"null not allowed":  {http.MethodPut, path, 200, jsonHeader, replace(`"required":"x"`, `"required":null`), "/required"},
		"nested ref":        {http.MethodPut, path, 200, jsonHeader, replace(`{"name":"c","parent":null}`, `{"name":3,"parent":null}`), "/root/children/0"},
		"uuid version":      {http.MethodPut, path, 200, jsonHeader, replace(id, "00000000-0000-4000-8000-000000000000"), "/id"},
		"date-time":         {http.MethodPut, path, 200, jsonHeader, replace("2026-10-20T10:00:00Z", "yesterday"), "date-time"},
		"range":             {http.MethodPut, path, 200, jsonHeader, replace(`"small":2`, `"small":200`), "greater than"},
		"array length":      {http.MethodPut, path, 200, jsonHeader, replace(`[1,2]`, `[1]`), "fewer than"},
		"base64":            {http.MethodPut, path, 200, jsonHeader, replace(`aGk=`, `!!`), "base64"},
		"not json":          {http.MethodPut, path, 200, jsonHeader, []byte(`{`), "not JSON"},
		"trailing data":     {http.MethodPut, path, 200, jsonHeader, []byte(valid + "{}"), "after the JSON value"},
		"content type":      {http.MethodPut, path, 200, http.Header{"Content-Type": {"text/html"}}, []byte(valid), "Content-Type"},
		"body on 204":       {http.MethodDelete, path, 204, http.Header{}, []byte(`{}`), "without a body"},
		"error shape":       {http.MethodPut, path, 500, jsonHeader, []byte(`{"message":"boom"}`), "error"},
		"undeclared status": {http.MethodDelete, path, 500, jsonHeader, []byte(`{"error":{"code":"c","message":"m"}}`), ""},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := document.ValidateResponse(tt.method, tt.path, tt.status, tt.header, tt.body)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("default error response rejected: %v", err)
				}
				return
			}
			if !errors.Is(err, openapi.ErrContract) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want ErrContract mentioning %q", err, tt.want)
			}
		})
	}
}

func TestDocumentHandlerServesJSON(t *testing.T) {
	t.Parallel()
	document, _ := build(t, sinkRoute())
	recorder := httptest.NewRecorder()
	document.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/openapi.json", nil))
	if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Type") != "application/json" || !bytes.Equal(recorder.Body.Bytes(), document.JSON()) {
		t.Fatalf("status=%d headers=%v", recorder.Code, recorder.Header())
	}
}

type named struct {
	A int `json:"a"`
}

func (named) OpenAPIName() string { return "Chosen" }

func TestNamerChoosesTheComponentNameAndHTMLIsNotEscaped(t *testing.T) {
	t.Parallel()
	document, tree := build(t, openapi.Route{Method: "GET", Pattern: "/a", Handler: openapi.Describe(ok, openapi.Operation{
		Summary:   "<a> & b",
		Responses: []openapi.Response{openapi.JSON[named](http.StatusOK)},
	})})
	if _, ok := at(t, tree, "components", "schemas").(map[string]any)["Chosen"]; !ok {
		t.Fatal("OpenAPIName was not used")
	}
	if !bytes.Contains(document.JSON(), []byte(`"<a> & b"`)) {
		t.Fatalf("summary was escaped:\n%s", document.JSON())
	}
}

func TestCheckResponsesReportsOnlyDocumentedMismatches(t *testing.T) {
	t.Parallel()
	var body string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Kept", "yes")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(body))
	})
	document, _ := build(t, openapi.Route{Method: http.MethodPost, Pattern: "/things", Handler: openapi.Describe(handler, openapi.Operation{
		Responses: []openapi.Response{openapi.JSON[createRequest](http.StatusCreated)},
	})})
	var reports []string
	checked := document.CheckResponses(handler, func(r *http.Request, err error) { reports = append(reports, r.URL.Path+": "+err.Error()) })
	serve := func(path, payload string) *httptest.ResponseRecorder {
		body = payload
		recorder := httptest.NewRecorder()
		checked.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, nil))
		return recorder
	}
	if recorder := serve("/things", `{"name":"a"}`); recorder.Code != http.StatusCreated || recorder.Body.String() != `{"name":"a"}` || recorder.Header().Get("X-Kept") != "yes" {
		t.Fatalf("response was changed: %d %v %s", recorder.Code, recorder.Header(), recorder.Body)
	}
	serve("/things", `{"name":1}`)
	serve("/elsewhere", `not even JSON`)
	if len(reports) != 1 || !strings.Contains(reports[0], "/things: ") || !strings.Contains(reports[0], "/name") {
		t.Fatalf("reports = %v", reports)
	}
	err := document.ValidateResponse(http.MethodGet, "/elsewhere", 200, http.Header{}, nil)
	if !errors.Is(err, openapi.ErrUndocumented) || errors.Is(err, openapi.ErrContract) {
		t.Fatalf("undocumented route: err = %v", err)
	}
}

type pointerTyper struct{ Value int }

func (*pointerTyper) OpenAPIType() openapi.Type { return openapi.TypeOf[string]() }

func TestTypersThroughPointersAndPointerReceivers(t *testing.T) {
	t.Parallel()
	type wrapper struct {
		Maybe   *optional[int] `json:"maybe"`
		ByPtr   pointerTyper   `json:"by_ptr"`
		PtrPtr  *pointerTyper  `json:"ptr_ptr"`
		Missing optional[bool] `json:"missing,omitzero"`
	}
	_, tree := build(t, openapi.Route{Method: "GET", Pattern: "/a", Handler: openapi.Describe(ok, openapi.Operation{Responses: []openapi.Response{openapi.JSON[wrapper](http.StatusOK)}})})
	properties := at(t, tree, "components", "schemas", "Wrapper", "properties").(map[string]any)
	want := map[string]string{
		"maybe":   `{"format":"int64","type":["integer","null"]}`,
		"by_ptr":  `{"type":"string"}`,
		"ptr_ptr": `{"type":["string","null"]}`,
		"missing": `{"type":["boolean","null"]}`,
	}
	for name, schema := range want {
		if got := compact(t, properties[name]); got != schema {
			t.Errorf("%s = %s, want %s", name, got, schema)
		}
	}
}

type snake_case struct {
	A int `json:"a"`
}

func TestSnakeCaseTypeNamesBecomeCamelCase(t *testing.T) {
	t.Parallel()
	_, tree := build(t, openapi.Route{Method: "GET", Pattern: "/a", Handler: openapi.Describe(ok, openapi.Operation{Responses: []openapi.Response{openapi.JSON[snake_case](http.StatusOK)}})})
	if _, ok := at(t, tree, "components", "schemas").(map[string]any)["SnakeCase"]; !ok {
		t.Fatalf("components = %v", at(t, tree, "components", "schemas"))
	}
}

type collidingName struct {
	X string `json:"x"`
}

func (collidingName) OpenAPIName() string { return "Chosen" }

func TestComponentNameCollisions(t *testing.T) {
	t.Parallel()
	respond := func(response openapi.Response) http.Handler {
		return openapi.Describe(ok, openapi.Operation{Responses: []openapi.Response{response}})
	}

	// Type arguments from different packages are qualified: v1 and v2
	// packages both returning pages of their own Item.
	routes := []openapi.Route{
		{Method: "GET", Pattern: "/web", Handler: respond(openapi.JSON[pagination.Page[web.ErrorDetail]](http.StatusOK))},
		{Method: "GET", Pattern: "/local", Handler: respond(openapi.JSON[pagination.Page[ErrorDetail]](http.StatusOK))},
	}
	first, tree := build(t, routes...)
	schemas := at(t, tree, "components", "schemas").(map[string]any)
	for name, field := range map[string]string{"pagination.Page_web.ErrorDetail": "code", "pagination.Page_openapi_test.ErrorDetail": "field"} {
		items := at(t, schemas, name, "properties", "items", "items", "$ref").(string)
		target := strings.TrimPrefix(items, "#/components/schemas/")
		if _, ok := at(t, schemas, target, "properties").(map[string]any)[field]; !ok {
			t.Errorf("%s refers to %s, which lacks %q", name, target, field)
		}
	}
	for range 20 {
		again, _ := build(t, routes...)
		if !bytes.Equal(first.JSON(), again.JSON()) {
			t.Fatal("colliding names produce a different document on each build")
		}
	}

	// Function-local types of one package cannot be told apart: an error
	// naming both, never one schema silently standing for both.
	type response struct {
		X string `json:"x"`
	}
	other := func() openapi.Response {
		type response struct {
			Y int `json:"y"`
		}
		return openapi.JSON[response](http.StatusOK)
	}
	_, err := openapi.Build(openapi.Info{Title: "t", Version: "1"}, []openapi.Route{
		{Method: "GET", Pattern: "/a", Handler: respond(openapi.JSON[response](http.StatusOK))},
		{Method: "GET", Pattern: "/b", Handler: respond(other())},
	})
	if !errors.Is(err, openapi.ErrNotDescribable) || !strings.Contains(err.Error(), "openapi_test.response and openapi_test.response") || !strings.Contains(err.Error(), "openapi.Namer") {
		t.Fatalf("local types: err = %v", err)
	}

	// An OpenAPIName equal to another type's name.
	_, err = openapi.Build(openapi.Info{Title: "t", Version: "1"}, []openapi.Route{
		{Method: "GET", Pattern: "/a", Handler: respond(openapi.JSON[named](http.StatusOK))},
		{Method: "GET", Pattern: "/b", Handler: respond(openapi.JSON[collidingName](http.StatusOK))},
	})
	if !errors.Is(err, openapi.ErrNotDescribable) || !strings.Contains(err.Error(), `"Chosen"`) {
		t.Fatalf("Namer collision: err = %v", err)
	}
}

type pointerValidated struct {
	Name string `json:"name"`
}

func (p *pointerValidated) Validate() error {
	if p.Name == "" {
		return fault.New("invalid_name", "name is required", fault.CategoryInvalid, http.StatusUnprocessableEntity)
	}
	return nil
}

func TestTypedValidatesPointerRequestsAndDocuments422(t *testing.T) {
	t.Parallel()
	echo := func(_ *http.Request, request *pointerValidated) (created, error) {
		if request == nil {
			return created{}, fault.New("missing", "body is null", fault.CategoryInvalid, http.StatusBadRequest)
		}
		return created{ID: uuid.MustNew(), Name: request.Name}, nil
	}
	for name, handler := range map[string]http.Handler{
		"pointer request": openapi.Typed(http.StatusCreated, openapi.Operation{}, echo),
		"value request with pointer receiver": openapi.Typed(http.StatusCreated, openapi.Operation{}, func(r *http.Request, request pointerValidated) (created, error) {
			return echo(r, &request)
		}),
	} {
		if errorsDeclared := handler.(openapi.Described).OpenAPIOperation().Errors; !slices.Contains(errorsDeclared, http.StatusUnprocessableEntity) {
			t.Errorf("%s: 422 is not documented: %v", name, errorsDeclared)
		}
		for body, want := range map[string]int{`{"name":""}`: http.StatusUnprocessableEntity, `{"name":"a"}`: http.StatusCreated} {
			request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != want {
				t.Errorf("%s %s: status %d, want %d (%s)", name, body, recorder.Code, want, recorder.Body)
			}
		}
	}
	if errorsDeclared := openapi.Typed(http.StatusOK, openapi.Operation{}, func(*http.Request, created) (created, error) { return created{}, nil }).(openapi.Described).OpenAPIOperation().Errors; len(errorsDeclared) != 0 {
		t.Errorf("a request without Validate documents %v", errorsDeclared)
	}
}

type pagedParams struct {
	*pagination.Query
	ID string `path:"id"`
}

func TestParamsWalkEmbeddedPointers(t *testing.T) {
	t.Parallel()
	_, tree := build(t, openapi.Route{Method: "GET", Pattern: "/a/{id}", Handler: openapi.Describe(ok, openapi.Operation{
		Params: openapi.TypeOf[pagedParams](), Responses: []openapi.Response{openapi.Empty(http.StatusNoContent)},
	})})
	var names []string
	for _, parameter := range at(t, tree, "paths", "/a/{id}", "get", "parameters").([]any) {
		names = append(names, parameter.(map[string]any)["name"].(string))
	}
	if !slices.Equal(names, []string{"id", "cursor", "limit"}) {
		t.Fatalf("parameters = %v", names)
	}
}
