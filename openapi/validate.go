package openapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

var (
	// ErrContract reports a response that does not match the document.
	ErrContract = errors.New("openapi: response does not match the document")
	// ErrUndocumented reports a request no documented operation serves.
	ErrUndocumented = errors.New("openapi: no documented operation serves the request")
)

// CheckResponses returns a handler for tests: it serves next, reports
// through report every response of a documented route that does not match
// the document (ValidateResponse), and writes the response unchanged.
// Requests no documented route serves are not checked. Each response is
// buffered, so it does not suit streaming handlers; do not use it in
// production.
func (d *Document) CheckResponses(next http.Handler, report func(*http.Request, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buffered := &bufferedResponse{header: http.Header{}}
		next.ServeHTTP(buffered, r)
		if buffered.status == 0 {
			buffered.status = http.StatusOK
		}
		if err := d.ValidateResponse(r.Method, r.URL.Path, buffered.status, buffered.header, buffered.body.Bytes()); err != nil && !errors.Is(err, ErrUndocumented) {
			report(r, err)
		}
		for key, values := range buffered.header {
			w.Header()[key] = values
		}
		w.WriteHeader(buffered.status)
		_, _ = w.Write(buffered.body.Bytes())
	})
}

type bufferedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (b *bufferedResponse) Header() http.Header { return b.header }

func (b *bufferedResponse) WriteHeader(status int) {
	if b.status == 0 {
		b.status = status
	}
}

func (b *bufferedResponse) Write(data []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	return b.body.Write(data)
}

// ValidateResponse checks a response a documented route returned against the
// document: the status must be declared (or fall under "default"), a body
// must be JSON matching the declared schema, and a response declared without
// a body must have none. method and path are the request's, such as GET and
// /v1/posts/0192...; the route is found with the router's matching rules.
// It validates the subset of JSON Schema that Build produces.
func (d *Document) ValidateResponse(method, path string, status int, header http.Header, body []byte) error {
	request := &http.Request{Method: method, URL: &url.URL{Path: path}, Host: "localhost"}
	_, pattern := d.matcher.Handler(request)
	route, ok := d.patterns[pattern]
	if !ok {
		return fmt.Errorf("%w: %s %s", ErrUndocumented, method, path)
	}
	paths, _ := d.tree["paths"].(map[string]any)
	item, _ := paths[route.path].(map[string]any)
	operation, _ := item[route.method].(map[string]any)
	responses, _ := operation["responses"].(map[string]any)
	response, ok := responses[strconv.Itoa(status)].(map[string]any)
	if !ok {
		response, ok = responses["default"].(map[string]any)
	}
	if !ok {
		return fmt.Errorf("%w: %s %s does not declare status %d", ErrContract, method, route.path, status)
	}
	content, hasContent := response["content"].(map[string]any)
	if method == http.MethodHead {
		// HEAD answers a GET route's status and headers without the body.
		return nil
	}
	if !hasContent {
		if len(bytes.TrimSpace(body)) != 0 {
			return fmt.Errorf("%w: %s %s %d is declared without a body but has one", ErrContract, method, route.path, status)
		}
		return nil
	}
	mediaType, _, err := mime.ParseMediaType(header.Get("Content-Type"))
	if err != nil || mediaType != jsonMediaType {
		return fmt.Errorf("%w: %s %s %d has Content-Type %q, want %s", ErrContract, method, route.path, status, header.Get("Content-Type"), jsonMediaType)
	}
	media, _ := content[jsonMediaType].(map[string]any)
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return fmt.Errorf("%w: %s %s %d body is not JSON: %v", ErrContract, method, route.path, status, err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: %s %s %d body has data after the JSON value", ErrContract, method, route.path, status)
	}
	v := validator{document: d.tree}
	if err := v.validate(media["schema"], value, ""); err != nil {
		return fmt.Errorf("%w: %s %s %d: %v", ErrContract, method, route.path, status, err)
	}
	return nil
}

type validator struct {
	document map[string]any
	depth    int
}

func (v *validator) validate(rawSchema, value any, at string) error {
	schema, ok := rawSchema.(map[string]any)
	if !ok {
		return fmt.Errorf("%s: the document has no schema here", pointer(at))
	}
	v.depth++
	defer func() { v.depth-- }()
	if v.depth > 256 {
		return fmt.Errorf("%s: value nests too deeply", pointer(at))
	}
	if ref, ok := schema["$ref"].(string); ok {
		target, err := v.resolve(ref)
		if err != nil {
			return err
		}
		if err := v.validate(target, value, at); err != nil {
			return err
		}
	}
	if branches, ok := schema["anyOf"].([]any); ok {
		var failures []string
		matched := false
		for _, branch := range branches {
			err := v.validate(branch, value, at)
			if err == nil {
				matched = true
				break
			}
			failures = append(failures, err.Error())
		}
		if !matched {
			return fmt.Errorf("%s: matches no alternative (%s)", pointer(at), strings.Join(failures, "; "))
		}
	}
	if kinds, ok := typesOf(schema["type"]); ok && !slices.ContainsFunc(kinds, func(kind string) bool { return isKind(value, kind) }) {
		return fmt.Errorf("%s: %s is not of type %s", pointer(at), describe(value), strings.Join(kinds, " or "))
	}
	switch value := value.(type) {
	case string:
		return v.validateString(schema, value, at)
	case json.Number:
		return validateNumber(schema, value, at)
	case []any:
		if minimum, ok := number(schema["minItems"]); ok && float64(len(value)) < minimum {
			return fmt.Errorf("%s: has %d items, fewer than %v", pointer(at), len(value), minimum)
		}
		if maximum, ok := number(schema["maxItems"]); ok && float64(len(value)) > maximum {
			return fmt.Errorf("%s: has %d items, more than %v", pointer(at), len(value), maximum)
		}
		if items, ok := schema["items"]; ok {
			for i, item := range value {
				if err := v.validate(items, item, at+"/"+strconv.Itoa(i)); err != nil {
					return err
				}
			}
		}
	case map[string]any:
		return v.validateObject(schema, value, at)
	}
	return nil
}

func (v *validator) validateObject(schema, value map[string]any, at string) error {
	required, _ := schema["required"].([]any)
	for _, name := range required {
		if key, ok := name.(string); ok {
			if _, present := value[key]; !present {
				return fmt.Errorf("%s: required member %q is missing", pointer(at), key)
			}
		}
	}
	properties, _ := schema["properties"].(map[string]any)
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		member := at + "/" + strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
		if property, ok := properties[key]; ok {
			if err := v.validate(property, value[key], member); err != nil {
				return err
			}
			continue
		}
		switch additional := schema["additionalProperties"].(type) {
		case bool:
			if !additional {
				return fmt.Errorf("%s: member %q is not in the schema", pointer(at), key)
			}
		case map[string]any:
			if err := v.validate(additional, value[key], member); err != nil {
				return err
			}
		}
	}
	return nil
}

func (v *validator) validateString(schema map[string]any, value, at string) error {
	switch schema["format"] {
	case "date-time":
		if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
			return fmt.Errorf("%s: %q is not an RFC 3339 date-time", pointer(at), value)
		}
	case "uuid":
		if !uuidText.MatchString(value) {
			return fmt.Errorf("%s: %q is not a UUID", pointer(at), value)
		}
	}
	if pattern, ok := schema["pattern"].(string); ok {
		expression, err := regexp.Compile(pattern)
		if err != nil {
			return fmt.Errorf("%s: the document's pattern %q does not compile: %v", pointer(at), pattern, err)
		}
		if !expression.MatchString(value) {
			return fmt.Errorf("%s: %q does not match %s", pointer(at), value, pattern)
		}
	}
	if schema["contentEncoding"] == "base64" {
		if _, err := base64.StdEncoding.DecodeString(value); err != nil {
			return fmt.Errorf("%s: is not base64", pointer(at))
		}
	}
	return nil
}

var uuidText = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func validateNumber(schema map[string]any, value json.Number, at string) error {
	parsed, err := value.Float64()
	if err != nil {
		return fmt.Errorf("%s: %s is not a number", pointer(at), value)
	}
	if minimum, ok := number(schema["minimum"]); ok && parsed < minimum {
		return fmt.Errorf("%s: %s is less than %v", pointer(at), value, minimum)
	}
	if maximum, ok := number(schema["maximum"]); ok && parsed > maximum {
		return fmt.Errorf("%s: %s is greater than %v", pointer(at), value, maximum)
	}
	return nil
}

func (v *validator) resolve(ref string) (any, error) {
	name, ok := strings.CutPrefix(ref, "#/components/schemas/")
	if !ok {
		return nil, fmt.Errorf("unsupported reference %q", ref)
	}
	components, _ := v.document["components"].(map[string]any)
	schemas, _ := components["schemas"].(map[string]any)
	target, ok := schemas[name]
	if !ok {
		return nil, fmt.Errorf("unknown reference %q", ref)
	}
	return target, nil
}

func typesOf(raw any) ([]string, bool) {
	switch raw := raw.(type) {
	case string:
		return []string{raw}, true
	case []any:
		kinds := make([]string, 0, len(raw))
		for _, kind := range raw {
			if text, ok := kind.(string); ok {
				kinds = append(kinds, text)
			}
		}
		return kinds, true
	}
	return nil, false
}

func isKind(value any, kind string) bool {
	switch value := value.(type) {
	case nil:
		return kind == "null"
	case bool:
		return kind == "boolean"
	case string:
		return kind == "string"
	case []any:
		return kind == "array"
	case map[string]any:
		return kind == "object"
	case json.Number:
		if kind == "number" {
			return true
		}
		if kind != "integer" {
			return false
		}
		if _, err := value.Int64(); err == nil {
			return true
		}
		parsed, err := value.Float64()
		return err == nil && parsed == float64(int64(parsed))
	}
	return false
}

func describe(value any) string {
	switch value.(type) {
	case nil:
		return "null"
	case bool:
		return "a boolean"
	case string:
		return "a string"
	case []any:
		return "an array"
	case map[string]any:
		return "an object"
	case json.Number:
		return "a number"
	}
	return fmt.Sprintf("%T", value)
}

func number(raw any) (float64, bool) {
	if value, ok := raw.(json.Number); ok {
		parsed, err := value.Float64()
		return parsed, err == nil
	}
	return 0, false
}

func pointer(at string) string {
	if at == "" {
		return "body"
	}
	return "body" + at
}
