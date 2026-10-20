package openapi_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/openapi"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

// FuzzValidateResponse feeds arbitrary requests and responses to the
// contract validator: it never panics, every failure is ErrContract or
// ErrUndocumented, and the verdict is deterministic.
func FuzzValidateResponse(f *testing.F) {
	document, err := openapi.Build(openapi.Info{Title: "fuzz", Version: "1.0.0"}, []openapi.Route{
		sinkRoute(),
		{Method: http.MethodDelete, Pattern: "/v1/sinks/{id}", Handler: openapi.Describe(ok, openapi.Operation{Responses: []openapi.Response{openapi.Empty(http.StatusNoContent)}})},
		{Method: http.MethodGet, Pattern: "/v1/nodes", Handler: openapi.Describe(ok, openapi.Operation{Responses: []openapi.Response{{Status: http.StatusOK, Body: openapi.TypeOf[[]node]()}}})},
	})
	if err != nil {
		f.Fatal(err)
	}
	id := uuid.MustNew().String()
	valid := `{"id":"` + id + `","owner_id":null,"count":1,"small":2,"ratio":0.5,"enabled":true,"tags":null,"raw":[1],"blob":"aGk=","pair":[1,2],"big":"9","anything":{},"elapsed":5,` +
		`"root":{"name":"r","parent":null,"children":[{"name":"c","parent":null}]},"inline":{"A":1},"required":"x","formatted":"a@b.c","nested":{"k":[]},"created_at":"2026-10-20T10:00:00Z"}`
	const jsonType = "application/json; charset=utf-8"
	for _, seed := range []struct {
		method, path string
		status       int
		contentType  string
		body         string
	}{
		{http.MethodPut, "/v1/sinks/" + id, 200, jsonType, valid},
		{http.MethodPut, "/v1/sinks/" + id, 200, jsonType, strings.Replace(valid, `"count":1`, `"count":1e400`, 1)},
		{http.MethodPut, "/v1/sinks/" + id, 200, jsonType, strings.Replace(valid, `"small":2`, `"small":-9223372036854775809`, 1)},
		{http.MethodPut, "/v1/sinks/" + id, 404, jsonType, `{"error":{"code":"not_found","message":"m"}}`},
		{http.MethodPut, "/v1/sinks/x", 500, "application/json", `{"error":{"code":"c","message":"m","details":[{}]}}`},
		{http.MethodDelete, "/v1/sinks/" + id, 204, "", ""},
		{http.MethodHead, "/v1/nodes", 200, "", "ignored"},
		{http.MethodGet, "/v1/nodes", 200, jsonType, `[{"name":"a","parent":{"name":"b","parent":null,"children":[]}}]`},
		{http.MethodGet, "/v1/nodes", 200, jsonType, strings.Repeat("[", 300) + strings.Repeat("]", 300)},
		{http.MethodGet, "/v1/nodes", 200, "application/json; charset=\"", `[]`},
		{"CONNECT", "example.com:443", 200, jsonType, `{}`},
		{http.MethodGet, "/v1/../v1/nodes", 200, jsonType, `[]`},
		{http.MethodGet, "", -1, "", ""},
	} {
		f.Add(seed.method, seed.path, seed.status, seed.contentType, []byte(seed.body))
	}
	f.Fuzz(func(t *testing.T, method, path string, status int, contentType string, body []byte) {
		header := http.Header{}
		if contentType != "" {
			header.Set("Content-Type", contentType)
		}
		err := document.ValidateResponse(method, path, status, header, body)
		if err != nil && !errors.Is(err, openapi.ErrContract) && !errors.Is(err, openapi.ErrUndocumented) {
			t.Fatalf("ValidateResponse returned an unclassified error: %v", err)
		}
		again := document.ValidateResponse(method, path, status, header, body)
		if (err == nil) != (again == nil) || (err != nil && err.Error() != again.Error()) {
			t.Fatalf("verdict is not deterministic: %v, then %v", err, again)
		}
	})
}
