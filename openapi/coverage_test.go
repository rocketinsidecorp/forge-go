package openapi_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/fault"
	"github.com/rocketinsidecorp/forge-go/openapi"
)

// --- types for the schema rules ---

type covNumbers struct {
	Int    int     `json:"int"`
	Int8   int8    `json:"int8"`
	Int16  int16   `json:"int16"`
	Int32  int32   `json:"int32"`
	Int64  int64   `json:"int64"`
	Uint   uint    `json:"uint"`
	Uint8  uint8   `json:"uint8"`
	Uint16 uint16  `json:"uint16"`
	Uint32 uint32  `json:"uint32"`
	Uint64 uint64  `json:"uint64"`
	F32    float32 `json:"f32"`
	F64    float64 `json:"f64"`
}

// covJSONMarshaler and covTextMarshaler encode themselves, so the document
// cannot say more than "any JSON" or "a string".
type covJSONMarshaler struct{ v int }

func (m covJSONMarshaler) MarshalJSON() ([]byte, error) { return []byte(strconv.Itoa(m.v)), nil }

type covTextMarshaler struct{ v int }

func (m covTextMarshaler) MarshalText() ([]byte, error) { return []byte(strconv.Itoa(m.v)), nil }

type covEncodings struct {
	J covJSONMarshaler `json:"j"`
	T covTextMarshaler `json:"t"`
	// A byte slice of a type with its own encoding is an array, not base64.
	Bytes []covTextByte `json:"bytes"`
}

type covTextByte byte

func (b covTextByte) MarshalText() ([]byte, error) { return []byte{'x'}, nil }

type covNullables struct {
	PtrRaw   *json.RawMessage `json:"ptr_raw"`
	PtrPtr   **int            `json:"ptr_ptr"`
	PtrNode  **node           `json:"ptr_node"`
	Quoted   *int64           `json:"quoted,string"`
	NonNull  *node            `json:"non_null" openapi:"nonnull"`
	Optional *string          `json:"optional" openapi:"nonnull"`
}

type covEmptyTyper struct{}

func (covEmptyTyper) OpenAPIType() openapi.Type { return openapi.Type{} }

type covSelfTyper struct{}

func (covSelfTyper) OpenAPIType() openapi.Type { return openapi.TypeOf[covSelfTyper]() }

type covInterfaceTyper struct {
	Any openapi.Typer `json:"any"`
}

// --- types for encoding/json's field rules ---

type covCycA struct {
	*covCycB
	X int `json:"x"`
}

type covCycB struct {
	*covCycA
	Y int `json:"y"`
}

type covInnerName struct {
	Name string `json:"name"`
	Deep string `json:"deep"`
}

// The shallowest field wins: Name is an int here, not the embedded string.
type covShadow struct {
	covInnerName
	Name int `json:"name"`
}

type covPlain struct{ Name string }

type covOther struct {
	Renamed bool `json:"Name"`
}

// A tagged field wins over an untagged one of the same name at the same
// depth, whichever comes first.
type covTaggedLast struct {
	covPlain
	covOther
}

type covTaggedFirst struct {
	covOther
	covPlain
}

type covLeft struct{ Dup string }
type covRight struct{ Dup int }

// Two fields of the same name at the same depth and tag status cancel out.
type covAmbiguous struct {
	covLeft
	covRight
	Kept bool `json:"kept"`
}

type covShared struct{ Shared string }
type covViaOne struct{ covShared }
type covViaTwo struct{ covShared }

// The same type embedded through two paths cancels out too.
type covDiamond struct {
	covViaOne
	covViaTwo
	Kept bool `json:"kept"`
}

type covHidden int

// covUnexportedEmbedded embeds an unexported non-struct: encoding/json skips it.
type covUnexportedEmbedded struct {
	covHidden
	Shown string `json:"shown"`
}

type covGeneric[T any] struct {
	Value T `json:"value"`
}

type covDouble__under struct {
	A int `json:"a"`
}

type covChan struct {
	C chan int `json:"c"`
}

type covNestedChan struct {
	Inline struct {
		C chan int `json:"c"`
	} `json:"inline"`
}

func routeFor(op openapi.Operation) openapi.Route {
	return openapi.Route{Method: http.MethodGet, Pattern: "/a", Handler: openapi.Describe(ok, op)}
}

func respond[T any]() openapi.Operation {
	return openapi.Operation{Responses: []openapi.Response{openapi.JSON[T](http.StatusOK)}}
}

func schemaOf[T any](t *testing.T) (map[string]any, map[string]any) {
	t.Helper()
	_, tree := build(t, routeFor(respond[T]()))
	body := at(t, tree, "paths", "/a", "get", "responses", "200", "content", "application/json", "schema").(map[string]any)
	return body, at(t, tree, "components", "schemas").(map[string]any)
}

// properties returns the property schemas of the component T references.
func properties[T any](t *testing.T) map[string]any {
	t.Helper()
	ref, components := schemaOf[T](t)
	name := strings.TrimPrefix(ref["$ref"].(string), "#/components/schemas/")
	return at(t, components, name, "properties").(map[string]any)
}

func TestIntegerAndFloatKindsHaveTheirRanges(t *testing.T) {
	t.Parallel()
	props := properties[covNumbers](t)
	tests := map[string]string{
		"int":    `{"format":"int64","type":"integer"}`,
		"int64":  `{"format":"int64","type":"integer"}`,
		"int32":  `{"format":"int32","type":"integer"}`,
		"int8":   `{"format":"int32","maximum":127,"minimum":-128,"type":"integer"}`,
		"int16":  `{"format":"int32","maximum":32767,"minimum":-32768,"type":"integer"}`,
		"uint":   `{"format":"int64","minimum":0,"type":"integer"}`,
		"uint32": `{"format":"int64","minimum":0,"type":"integer"}`,
		"uint64": `{"format":"int64","minimum":0,"type":"integer"}`,
		"uint8":  `{"format":"int32","maximum":255,"minimum":0,"type":"integer"}`,
		"uint16": `{"format":"int32","maximum":65535,"minimum":0,"type":"integer"}`,
		"f32":    `{"format":"float","type":"number"}`,
		"f64":    `{"format":"double","type":"number"}`,
	}
	for name, want := range tests {
		if got := compact(t, props[name]); got != want {
			t.Errorf("%s = %s, want %s", name, got, want)
		}
	}
}

func TestRangesRejectValuesTheTypeCannotHold(t *testing.T) {
	t.Parallel()
	document, _ := build(t, routeFor(respond[covNumbers]()))
	header := http.Header{"Content-Type": {"application/json"}}
	body := func(field, value string) []byte {
		doc := map[string]json.RawMessage{}
		for _, name := range []string{"int", "int8", "int16", "int32", "int64", "uint", "uint8", "uint16", "uint32", "uint64", "f32", "f64"} {
			doc[name] = json.RawMessage("1")
		}
		doc[field] = json.RawMessage(value)
		encoded, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	if err := document.ValidateResponse("GET", "/a", 200, header, body("int8", "1")); err != nil {
		t.Fatalf("a valid body was rejected: %v", err)
	}
	for _, tc := range []struct{ field, value, want string }{
		{"int8", "200", "is greater than 127"},
		{"int8", "-129", "is less than -128"},
		{"uint8", "-1", "is less than 0"},
		{"uint16", "65536", "is greater than 65535"},
		{"uint", "-5", "is less than 0"},
		{"int", "1.5", "is not of type integer"},
		{"int", "1e999", "is not of type integer"},
		{"f64", "1e999", "is not a number"},
		{"f64", `"1"`, "a string is not of type number"},
		{"f32", "true", "a boolean is not of type number"},
		{"int16", "[]", "an array is not of type integer"},
		{"int32", "null", "null is not of type integer"},
	} {
		err := document.ValidateResponse("GET", "/a", 200, header, body(tc.field, tc.value))
		if !errors.Is(err, openapi.ErrContract) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s=%s: err = %v, want %q", tc.field, tc.value, err, tc.want)
		}
	}
	// A whole number written with a fraction or exponent is still an integer.
	for _, value := range []string{"2.0", "2e0", "1e2"} {
		if err := document.ValidateResponse("GET", "/a", 200, header, body("int", value)); err != nil {
			t.Errorf("int=%s: %v", value, err)
		}
	}
}

func TestSelfEncodingTypesAreDescribedAsAnyJSONOrAString(t *testing.T) {
	t.Parallel()
	props := properties[covEncodings](t)
	if got := compact(t, props["j"]); got != `{}` {
		t.Errorf("json.Marshaler = %s", got)
	}
	if got := compact(t, props["t"]); got != `{"type":"string"}` {
		t.Errorf("encoding.TextMarshaler = %s", got)
	}
	if got := compact(t, props["bytes"]); got != `{"items":{"type":"string"},"type":["array","null"]}` {
		t.Errorf("[]TextMarshaler-byte = %s", got)
	}
}

func TestNullabilityComposes(t *testing.T) {
	t.Parallel()
	props := properties[covNullables](t)
	tests := map[string]string{
		// "Any value" already includes null.
		"ptr_raw": `{}`,
		// A pointer to a pointer is nullable once.
		"ptr_ptr": `{"format":"int64","type":["integer","null"]}`,
		// A reference is made nullable by an alternative, once.
		"ptr_node": `{"anyOf":[{"$ref":"#/components/schemas/Node"},{"type":"null"}]}`,
		// ",string" writes a number as text; the null stays.
		"quoted": `{"type":["string","null"]}`,
		// openapi:"nonnull" removes the null alternative again.
		"non_null": `{"$ref":"#/components/schemas/Node"}`,
		"optional": `{"type":"string"}`,
	}
	for name, want := range tests {
		if got := compact(t, props[name]); got != want {
			t.Errorf("%s = %s, want %s", name, got, want)
		}
	}
}

func TestADocumentedFormatAppliesToTheReferencedSchema(t *testing.T) {
	t.Parallel()
	props := properties[covFormatted](t)
	got := compact(t, props["ref"])
	if want := `{"anyOf":[{"$ref":"#/components/schemas/Node","format":"custom"},{"type":"null"}]}`; got != want {
		t.Errorf("ref = %s, want %s", got, want)
	}
}

type covFormatted struct {
	Ref *node `json:"ref" format:"custom"`
}

func TestSchemasThatCannotBeDescribedAreErrors(t *testing.T) {
	t.Parallel()
	tests := map[string]openapi.Operation{
		"pointer to chan":      respond[*chan int](),
		"slice of chan":        respond[[]chan int](),
		"array of chan":        respond[[2]chan int](),
		"map of chan":          respond[map[string]chan int](),
		"named struct field":   respond[covChan](),
		"anonymous struct":     respond[covNestedChan](),
		"typer without a type": respond[covEmptyTyper](),
		"typer that loops":     respond[covSelfTyper](),
		"typer interface":      respond[covInterfaceTyper](),
		"request chan":         {Request: openapi.TypeOf[chan int](), Responses: []openapi.Response{openapi.Empty(http.StatusNoContent)}},
	}
	for name, op := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := openapi.Build(openapi.Info{Title: "t", Version: "1"}, []openapi.Route{routeFor(op)})
			if !errors.Is(err, openapi.ErrNotDescribable) {
				t.Fatalf("err = %v, want ErrNotDescribable", err)
			}
		})
	}
	// A failed type leaves no half-built component behind: the next types
	// still build.
	_, err := openapi.Build(openapi.Info{Title: "t", Version: "1"}, []openapi.Route{
		routeFor(respond[covChan]()),
	})
	if err == nil || !strings.Contains(err.Error(), "cannot encode chan") {
		t.Fatalf("err = %v", err)
	}
}

func TestEmbeddedFieldsFollowEncodingJSON(t *testing.T) {
	t.Parallel()
	// Each case is checked against what encoding/json really writes, so the
	// document cannot drift from the encoder.
	type tc struct {
		name  string
		props map[string]any
		value any
	}
	cases := []tc{
		{"cycle", properties[covCycA](t), covCycA{covCycB: &covCycB{Y: 2}, X: 1}},
		{"shallowest wins", properties[covShadow](t), covShadow{Name: 3}},
		{"tagged wins, untagged first", properties[covTaggedLast](t), covTaggedLast{covOther: covOther{Renamed: true}}},
		{"tagged wins, tagged first", properties[covTaggedFirst](t), covTaggedFirst{covOther: covOther{Renamed: true}}},
		{"ambiguous names are dropped", properties[covAmbiguous](t), covAmbiguous{Kept: true}},
		{"diamond is dropped", properties[covDiamond](t), covDiamond{Kept: true}},
		{"unexported embedded non-struct", properties[covUnexportedEmbedded](t), covUnexportedEmbedded{Shown: "s"}},
	}
	for _, c := range cases {
		encoded, err := json.Marshal(c.value)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		var written map[string]any
		if err := json.Unmarshal(encoded, &written); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		for key := range written {
			if _, ok := c.props[key]; !ok {
				t.Errorf("%s: encoding/json writes %q, the document lacks it (%v)", c.name, key, c.props)
			}
		}
		if len(written) != len(c.props) {
			t.Errorf("%s: encoding/json writes %v, the document has %v", c.name, written, c.props)
		}
	}
	if got := compact(t, properties[covShadow](t)["name"]); got != `{"format":"int64","type":"integer"}` {
		t.Errorf("the outer Name must win: %s", got)
	}
	if got := compact(t, properties[covTaggedLast](t)["Name"]); got != `{"type":"boolean"}` {
		t.Errorf("the tagged Name must win: %s", got)
	}
	if got := compact(t, properties[covTaggedFirst](t)["Name"]); got != `{"type":"boolean"}` {
		t.Errorf("the tagged Name must win: %s", got)
	}
}

func TestGenericAndOddlyNamedTypesGetCleanComponentNames(t *testing.T) {
	t.Parallel()
	_, components := schemaOf[covGeneric[[]covDouble__under]](t)
	for _, name := range []string{"CovGeneric_CovDoubleUnder", "CovDoubleUnder"} {
		if _, ok := components[name]; ok {
			return
		}
	}
	var names []string
	for name := range components {
		names = append(names, name)
	}
	t.Fatalf("components = %v", names)
}

// --- Build ---

func TestBuildRecordsDescriptionDeprecationAndInfo(t *testing.T) {
	t.Parallel()
	document, err := openapi.Build(openapi.Info{Title: "T", Version: "1", Description: "About it."}, []openapi.Route{
		routeFor(openapi.Operation{
			Description: "Long text.", Deprecated: true, Authenticated: true,
			Responses: []openapi.Response{openapi.Empty(http.StatusNoContent)},
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	var tree map[string]any
	if err := json.Unmarshal(document.JSON(), &tree); err != nil {
		t.Fatal(err)
	}
	if got := at(t, tree, "info", "description"); got != "About it." {
		t.Errorf("info.description = %v", got)
	}
	operation := at(t, tree, "paths", "/a", "get").(map[string]any)
	if operation["description"] != "Long text." || operation["deprecated"] != true {
		t.Errorf("operation = %v", operation)
	}
	if got := compact(t, operation["security"]); got != `[{"bearerAuth":[]}]` {
		t.Errorf("security = %s", got)
	}
	if _, ok := operation["x-forge-permission"]; ok {
		t.Error("an Authenticated operation has no permission")
	}
	if got := compact(t, at(t, operation, "responses", "401")); !strings.Contains(got, "Unauthorized") {
		t.Errorf("401 is not implied: %s", got)
	}
}

type covBothTags struct {
	ID string `path:"id" query:"id"`
}

type covEmptyTag struct {
	Limit int `query:""`
}

type covBadParamType struct {
	Chan chan int `query:"chan"`
}

type covBadParamOption struct {
	Limit int `query:"limit" openapi:"requird"`
}

type covTwice struct {
	A string `query:"q"`
	B string `query:"q"`
}

type covUntagged struct {
	Internal string
	Limit    int `query:"limit"`
}

func TestBuildRejectsDescriptionsItCannotExpress(t *testing.T) {
	t.Parallel()
	none := []openapi.Response{openapi.Empty(http.StatusNoContent)}
	route := func(pattern string, op openapi.Operation) openapi.Route {
		return openapi.Route{Method: http.MethodGet, Pattern: pattern, Handler: openapi.Describe(ok, op)}
	}
	tests := map[string]struct {
		routes []openapi.Route
		want   string
	}{
		"wildcard inside a segment": {[]openapi.Route{route("/files/{name}.txt", openapi.Operation{Responses: none})}, `wildcard segment "{name}.txt"`},
		"conflicting patterns":      {[]openapi.Route{route("/a/{x}", openapi.Operation{Responses: none}), route("/{y}/b", openapi.Operation{Responses: none})}, "openapi: route GET /"},
		"request type":              {[]openapi.Route{route("/a", openapi.Operation{Request: openapi.TypeOf[chan int](), Responses: none})}, "cannot encode chan"},
		"status below 100":          {[]openapi.Route{route("/a", openapi.Operation{Responses: []openapi.Response{{Status: 99}}})}, "response status 99 is not an HTTP status"},
		"status above 599":          {[]openapi.Route{route("/a", openapi.Operation{Responses: []openapi.Response{{Status: 600}}})}, "response status 600"},
		"status declared twice":     {[]openapi.Route{route("/a", openapi.Operation{Responses: []openapi.Response{openapi.Empty(200), openapi.Empty(200)}})}, "response 200 is declared twice"},
		"304 with a body":           {[]openapi.Route{route("/a", openapi.Operation{Responses: []openapi.Response{openapi.JSON[node](http.StatusNotModified)}})}, "a 304 response has no body"},
		"params is not a struct":    {[]openapi.Route{route("/a", openapi.Operation{Params: openapi.TypeOf[int](), Responses: none})}, "Params must be a struct, not int"},
		"two parameter locations":   {[]openapi.Route{route("/a/{id}", openapi.Operation{Params: openapi.TypeOf[covBothTags](), Responses: none})}, "more than one of the path, query and header tags"},
		"empty parameter name":      {[]openapi.Route{route("/a", openapi.Operation{Params: openapi.TypeOf[covEmptyTag](), Responses: none})}, "empty query tag"},
		"parameter type":            {[]openapi.Route{route("/a", openapi.Operation{Params: openapi.TypeOf[covBadParamType](), Responses: none})}, "cannot encode chan"},
		"parameter option":          {[]openapi.Route{route("/a", openapi.Operation{Params: openapi.TypeOf[covBadParamOption](), Responses: none})}, "unknown openapi tag option"},
		"parameter declared twice":  {[]openapi.Route{route("/a", openapi.Operation{Params: openapi.TypeOf[covTwice](), Responses: none})}, `query parameter "q" is declared twice`},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			document, err := openapi.Build(openapi.Info{Title: "t", Version: "1"}, tt.routes)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to mention %q (document %v)", err, tt.want, document)
			}
			if document != nil {
				t.Error("a failed Build must not return a document")
			}
		})
	}
}

func TestParametersKeepUndeclaredFieldsOutAndSortDeterministically(t *testing.T) {
	t.Parallel()
	_, tree := build(t, openapi.Route{Method: http.MethodGet, Pattern: "/a/{id}/{rest...}", Handler: openapi.Describe(ok, openapi.Operation{
		Params:    openapi.TypeOf[covUntagged](),
		Responses: []openapi.Response{openapi.Empty(http.StatusNoContent)},
	})})
	if got := at(t, tree, "paths"); len(got.(map[string]any)) != 1 {
		t.Fatalf("paths = %v", got)
	}
	parameters := at(t, tree, "paths", "/a/{id}/{rest}", "get", "parameters").([]any)
	var names []string
	for _, parameter := range parameters {
		object := parameter.(map[string]any)
		names = append(names, object["in"].(string)+":"+object["name"].(string))
	}
	// Path parameters come from the pattern; the untagged field is not one.
	if want := "path:id path:rest query:limit"; strings.Join(names, " ") != want {
		t.Fatalf("parameters = %v, want %s", names, want)
	}
}

func TestTypeStringNamesTheGoType(t *testing.T) {
	t.Parallel()
	if got := openapi.TypeOf[covNumbers]().String(); got != "openapi_test.covNumbers" {
		t.Errorf("String = %q", got)
	}
	if got := (openapi.Type{}).String(); got != "" {
		t.Errorf("the zero Type is %q", got)
	}
}

// --- Typed ---

type covPlainRequest struct {
	Name string `json:"name"`
}

func TestTypedHandlerWithoutValidateAndWithFailingFunction(t *testing.T) {
	t.Parallel()
	handler := openapi.Typed(http.StatusOK, openapi.Operation{}, func(_ *http.Request, request covPlainRequest) (created, error) {
		if request.Name == "boom" {
			return created{}, fault.New("exploded", "no", fault.CategoryNotFound, http.StatusNotFound)
		}
		return created{Name: request.Name}, nil
	})
	send := func(body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}
	if recorder := send(`{"name":"ok"}`); recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"name":"ok"`) {
		t.Fatalf("plain request: %d %s", recorder.Code, recorder.Body)
	}
	if recorder := send(`{"name":"boom"}`); recorder.Code != http.StatusNotFound || !strings.Contains(recorder.Body.String(), "exploded") {
		t.Fatalf("a failing function is answered through web.Error: %d %s", recorder.Code, recorder.Body)
	}
}

func TestTypedLeavesANullPointerRequestToTheFunction(t *testing.T) {
	t.Parallel()
	called := false
	handler := openapi.Typed(http.StatusOK, openapi.Operation{}, func(_ *http.Request, request *pointerValidated) (created, error) {
		called = request == nil
		return created{}, nil
	})
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("null"))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !called {
		t.Fatalf("a null body must reach fn as a nil pointer without Validate: %d %s called=%v", recorder.Code, recorder.Body, called)
	}
}

func TestTypedRejectsANilFunction(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("Typed accepted a nil function")
		}
	}()
	openapi.Typed[openapi.NoBody, created](http.StatusOK, openapi.Operation{}, nil)
}

func TestTypedReplacesADeclaredResponseOfTheSuccessStatus(t *testing.T) {
	t.Parallel()
	handler := openapi.Typed(http.StatusCreated, openapi.Operation{
		Responses: []openapi.Response{openapi.Empty(http.StatusCreated), openapi.Empty(http.StatusAccepted)},
	}, func(*http.Request, openapi.NoBody) (created, error) { return created{}, nil })
	responses := handler.(openapi.Described).OpenAPIOperation().Responses
	if len(responses) != 2 || responses[0].Status != http.StatusCreated || responses[0].Body != openapi.TypeOf[created]() || responses[1].Status != http.StatusAccepted {
		t.Fatalf("responses = %+v", responses)
	}
}

// --- ValidateResponse and CheckResponses ---

func TestCheckResponsesDefaultsTheStatusAndBuffersWrites(t *testing.T) {
	t.Parallel()
	var body string
	handlers := map[string]http.HandlerFunc{
		// Never writes: net/http would answer 200 with no body.
		"/silent": func(http.ResponseWriter, *http.Request) {},
		// Writes without WriteHeader: an implicit 200.
		"/implicit": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		},
	}
	mux := http.NewServeMux()
	var routes []openapi.Route
	for path, handler := range handlers {
		described := openapi.Describe(handler, openapi.Operation{Responses: []openapi.Response{openapi.JSON[createRequest](http.StatusOK)}})
		mux.Handle("GET "+path, handler)
		routes = append(routes, openapi.Route{Method: http.MethodGet, Pattern: path, Handler: described})
	}
	document, _ := build(t, routes...)
	var reports []string
	checked := document.CheckResponses(mux, func(r *http.Request, err error) { reports = append(reports, r.URL.Path+": "+err.Error()) })

	body = `{"name":"a"}`
	recorder := httptest.NewRecorder()
	checked.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/implicit", nil))
	if recorder.Code != http.StatusOK || recorder.Body.String() != body || len(reports) != 0 {
		t.Fatalf("implicit 200: %d %q reports %v", recorder.Code, recorder.Body, reports)
	}
	recorder = httptest.NewRecorder()
	checked.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/silent", nil))
	if recorder.Code != http.StatusOK || recorder.Body.Len() != 0 {
		t.Fatalf("silent handler: %d %q", recorder.Code, recorder.Body)
	}
	if len(reports) != 1 || !strings.HasPrefix(reports[0], "/silent: ") || !strings.Contains(reports[0], "Content-Type") {
		t.Fatalf("a 200 without the declared JSON body is a contract failure: %v", reports)
	}
}

type covLimited struct {
	Pair  [2]int         `json:"pair"`
	Tally map[string]int `json:"tally"`
	ID    string         `json:"id" format:"uuid"`
	Blob  []byte         `json:"blob"`
	Fixed string         `json:"fixed" format:"date-time"`
}

type covOptional struct {
	A int `json:"a,omitempty"`
}

func TestValidateResponseChecksTheSchemaSubset(t *testing.T) {
	t.Parallel()
	document, _ := build(t,
		routeFor(respond[covLimited]()),
		openapi.Route{Method: http.MethodGet, Pattern: "/optional", Handler: openapi.Describe(ok, respond[covOptional]())},
		openapi.Route{Method: http.MethodGet, Pattern: "/node", Handler: openapi.Describe(ok, respond[node]())},
		openapi.Route{Method: http.MethodGet, Pattern: "/gone", Handler: openapi.Describe(ok, openapi.Operation{Responses: []openapi.Response{openapi.Empty(http.StatusNoContent)}})},
	)
	json := http.Header{"Content-Type": {"application/json; charset=utf-8"}}
	nest := func(depth int) string {
		return strings.Repeat(`{"name":"n","parent":`, depth) + `null` + strings.Repeat(`}`, depth)
	}
	tests := []struct {
		name, method, path string
		status             int
		header             http.Header
		body               string
		want               string // "" means valid
	}{
		{"valid", "GET", "/a", 200, json, `{"pair":[1,2],"tally":{"a":1},"id":"0190a8f0-0000-7000-8000-000000000000","blob":"aGk=","fixed":"2026-01-02T03:04:05Z"}`, ""},
		{"too many items", "GET", "/a", 200, json, `{"pair":[1,2,3],"tally":null,"id":"0190a8f0-0000-7000-8000-000000000000","blob":null,"fixed":"2026-01-02T03:04:05Z"}`, "has 3 items, more than 2"},
		{"too few items", "GET", "/a", 200, json, `{"pair":[1],"tally":null,"id":"0190a8f0-0000-7000-8000-000000000000","blob":null,"fixed":"2026-01-02T03:04:05Z"}`, "has 1 items, fewer than 2"},
		{"map value of the wrong type", "GET", "/a", 200, json, `{"pair":[1,2],"tally":{"a":"x"},"id":"0190a8f0-0000-7000-8000-000000000000","blob":null,"fixed":"2026-01-02T03:04:05Z"}`, "body/tally/a"},
		{"not a uuid", "GET", "/a", 200, json, `{"pair":[1,2],"tally":null,"id":"nope","blob":null,"fixed":"2026-01-02T03:04:05Z"}`, `"nope" is not a UUID`},
		{"not a date", "GET", "/a", 200, json, `{"pair":[1,2],"tally":null,"id":"0190a8f0-0000-7000-8000-000000000000","blob":null,"fixed":"yesterday"}`, "is not an RFC 3339 date-time"},
		{"not base64", "GET", "/a", 200, json, `{"pair":[1,2],"tally":null,"id":"0190a8f0-0000-7000-8000-000000000000","blob":"@@@","fixed":"2026-01-02T03:04:05Z"}`, "is not base64"},
		{"escaped member name", "GET", "/optional", 200, json, `{"a/b~c":1}`, `member "a/b~c" is not in the schema`},
		{"trailing data", "GET", "/a", 200, json, `{} {}`, "data after the JSON value"},
		{"not json", "GET", "/a", 200, json, `{`, "body is not JSON"},
		{"wrong content type", "GET", "/a", 200, http.Header{"Content-Type": {"text/html"}}, `{}`, `has Content-Type "text/html"`},
		{"no content type", "GET", "/a", 200, http.Header{}, `{}`, "has Content-Type"},
		{"a body on a no-content route", "GET", "/gone", 204, http.Header{}, `x`, "declared without a body but has one"},
		{"no body where none is declared", "GET", "/gone", 204, http.Header{}, "  \n", ""},
		{"HEAD skips the body", "HEAD", "/a", 200, http.Header{}, `garbage`, ""},
		{"unknown route", "GET", "/missing", 200, json, `{}`, "no documented operation"},
		{"recursion within the limit", "GET", "/node", 200, json, nest(50), ""},
		{"recursion beyond the limit", "GET", "/node", 200, json, nest(300), "value nests too deeply"},
	}
	for _, tt := range tests {
		err := document.ValidateResponse(tt.method, tt.path, tt.status, tt.header, []byte(tt.body))
		switch {
		case tt.want == "" && err != nil:
			t.Errorf("%s: %v", tt.name, err)
		case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
			t.Errorf("%s: err = %v, want it to mention %q", tt.name, err, tt.want)
		}
	}
}

var _ = covUnexportedEmbedded{covHidden: 0}
