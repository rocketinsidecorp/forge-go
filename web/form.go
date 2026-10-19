package web

import (
	"bytes"
	"encoding"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/rocketinsidecorp/forge-go/fault"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

// Limits for application/x-www-form-urlencoded bodies. The server's body
// limit (FORGE_HTTP_MAX_BODY_BYTES) still applies and may be lower.
const (
	MaxFormBytes  = 1 << 20
	MaxFormFields = 1000
)

// Form fields consumed by Forge middleware. DecodeForm ignores them and a
// struct may not bind them.
const (
	// MethodOverrideField carries PUT, PATCH or DELETE in a POST form; see
	// MethodOverride.
	MethodOverrideField = "_method"
	// CSRFTokenField carries the CSRF token in HTML forms; see package csrf.
	CSRFTokenField = "authenticity_token"
)

const formMediaType = "application/x-www-form-urlencoded"

// maxEchoedFieldName bounds the client-supplied field name an error repeats.
const maxEchoedFieldName = 64

// ErrInvalidFields is the fault behind FormErrors, so web.Error answers a
// FormErrors value with 422 invalid_fields.
var ErrInvalidFields = fault.New("invalid_fields", "one or more fields are invalid", fault.CategoryInvalid, http.StatusUnprocessableEntity)

// IsForm reports whether the request body is declared as
// application/x-www-form-urlencoded.
func IsForm(r *http.Request) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && mediaType == formMediaType
}

// ParseForm reads an application/x-www-form-urlencoded body once, with a
// size and field-count bound, and stores it in r.PostForm so later calls
// (and r.FormValue) reuse it. Query-string values are never included. Errors
// are faults: 415 unsupported_media_type, 413 body_too_large, 400
// invalid_form or unreadable_body.
func ParseForm(r *http.Request) (url.Values, error) {
	if !IsForm(r) {
		return nil, fault.New("unsupported_media_type", "request body must be application/x-www-form-urlencoded", fault.CategoryInvalid, http.StatusUnsupportedMediaType)
	}
	if r.PostForm != nil {
		return r.PostForm, nil
	}
	var body []byte
	if r.Body != nil {
		read, err := io.ReadAll(io.LimitReader(r.Body, MaxFormBytes+1))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				return nil, bodyTooLarge(err)
			}
			return nil, fault.New("unreadable_body", "the request body could not be read", fault.CategoryInvalid, http.StatusBadRequest).WithCause(err)
		}
		body = read
	}
	if len(body) > MaxFormBytes {
		return nil, bodyTooLarge(nil)
	}
	if len(body) > 0 && bytes.Count(body, []byte("&"))+1 > MaxFormFields {
		return nil, invalidForm("request body has too many form fields", nil)
	}
	values, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, invalidForm("request body is not a valid form", err)
	}
	r.PostForm = values
	return values, nil
}

func bodyTooLarge(cause error) error {
	tooLarge := fault.New("body_too_large", "request body too large", fault.CategoryInvalid, http.StatusRequestEntityTooLarge)
	if cause != nil {
		return tooLarge.WithCause(cause)
	}
	return tooLarge
}

func invalidForm(message string, cause error) error {
	invalid := fault.New("invalid_form", message, fault.CategoryInvalid, http.StatusBadRequest)
	if cause != nil {
		return invalid.WithCause(cause)
	}
	return invalid
}

// FieldError is one invalid form field. Message is safe to show to the user
// and never contains the submitted value.
type FieldError struct {
	Field   string
	Code    string
	Message string
}

func (e FieldError) Error() string { return e.Field + " " + e.Message }

// FormErrors collects field errors for re-rendering a form with 422. It
// unwraps to ErrInvalidFields.
type FormErrors []FieldError

func (e FormErrors) Error() string {
	parts := make([]string, 0, len(e))
	for _, field := range e {
		parts = append(parts, field.Error())
	}
	return "invalid fields: " + strings.Join(parts, "; ")
}

func (e FormErrors) Unwrap() error { return ErrInvalidFields }

// Add appends a field error, for example a domain validation failure.
func (e *FormErrors) Add(field, code, message string) {
	*e = append(*e, FieldError{Field: field, Code: code, Message: message})
}

// Has reports whether field has at least one error.
func (e FormErrors) Has(field string) bool {
	for _, current := range e {
		if current.Field == field {
			return true
		}
	}
	return false
}

// Message returns the first message for field, or "".
func (e FormErrors) Message(field string) string {
	for _, current := range e {
		if current.Field == field {
			return current.Message
		}
	}
	return ""
}

// Messages returns every message for field.
func (e FormErrors) Messages(field string) []string {
	var messages []string
	for _, current := range e {
		if current.Field == field {
			messages = append(messages, current.Message)
		}
	}
	return messages
}

// DecodeForm strictly decodes an application/x-www-form-urlencoded body into
// the struct dst points to. Only exported fields tagged `form:"name"` are
// bound; every tagged field is assigned, so an absent checkbox is false and
// an absent optional (pointer) field is nil.
//
// Supported field types: string, bool, signed and unsigned integers,
// time.Time (HTML date, datetime-local in UTC, or RFC 3339), uuid.UUID,
// types implementing encoding.TextUnmarshaler, pointers to these (an empty
// value is nil), and []string.
//
// Transport problems are faults (see ParseForm); unknown fields and repeated
// values for a scalar field are 400 invalid_form. Values that do not convert
// are returned together as FormErrors (422). The _method and
// authenticity_token fields are consumed by middleware and ignored here.
func DecodeForm(r *http.Request, dst any) error {
	target := reflect.ValueOf(dst)
	if target.Kind() != reflect.Pointer || target.IsNil() || target.Elem().Kind() != reflect.Struct {
		return errors.New("web: DecodeForm destination must be a non-nil pointer to a struct")
	}
	plan, err := formPlanFor(target.Elem().Type())
	if err != nil {
		return err
	}
	values, err := ParseForm(r)
	if err != nil {
		return err
	}
	for name, submitted := range values {
		if name == MethodOverrideField || name == CSRFTokenField {
			continue
		}
		index, known := plan.byName[name]
		if !known {
			if len(name) > maxEchoedFieldName {
				return invalidForm("request body has an unknown field", nil)
			}
			return invalidForm(fmt.Sprintf("unknown field %q", name), nil)
		}
		if len(submitted) > 1 && plan.fields[index].kind != kindStrings {
			return invalidForm(fmt.Sprintf("field %q was submitted more than once", name), nil)
		}
	}
	var problems FormErrors
	object := target.Elem()
	for _, field := range plan.fields {
		if problem := field.assign(object.FieldByIndex(field.index), values[field.name]); problem != nil {
			problems = append(problems, *problem)
		}
	}
	if len(problems) > 0 {
		return problems
	}
	return nil
}

type fieldKind int

const (
	kindString fieldKind = iota
	kindBool
	kindInt
	kindUint
	kindTime
	kindUUID
	kindText
	kindStrings
)

type formField struct {
	name    string
	index   []int
	kind    fieldKind
	pointer bool
	typ     reflect.Type // the element type for pointers
}

type formPlan struct {
	fields []formField
	byName map[string]int
}

var formPlans sync.Map // reflect.Type -> *formPlan

var (
	timeType          = reflect.TypeFor[time.Time]()
	uuidType          = reflect.TypeFor[uuid.UUID]()
	textUnmarshalType = reflect.TypeFor[encoding.TextUnmarshaler]()
)

func formPlanFor(structType reflect.Type) (*formPlan, error) {
	if cached, ok := formPlans.Load(structType); ok {
		return cached.(*formPlan), nil
	}
	plan := &formPlan{byName: map[string]int{}}
	for i := range structType.NumField() {
		field := structType.Field(i)
		name, tagged := field.Tag.Lookup("form")
		if !tagged || name == "-" {
			continue
		}
		if !field.IsExported() {
			return nil, fmt.Errorf("web: form field %s.%s is not exported", structType.Name(), field.Name)
		}
		if name == "" || name == MethodOverrideField || name == CSRFTokenField {
			return nil, fmt.Errorf("web: form field %s.%s has an empty or reserved name %q", structType.Name(), field.Name, name)
		}
		if _, duplicate := plan.byName[name]; duplicate {
			return nil, fmt.Errorf("web: form name %q is bound twice in %s", name, structType.Name())
		}
		bound := formField{name: name, index: field.Index, typ: field.Type}
		if field.Type.Kind() == reflect.Pointer {
			bound.pointer = true
			bound.typ = field.Type.Elem()
		}
		kind, ok := formKindOf(bound.typ)
		if !ok || (bound.pointer && kind == kindStrings) {
			return nil, fmt.Errorf("web: form field %s.%s has unsupported type %s", structType.Name(), field.Name, field.Type)
		}
		bound.kind = kind
		plan.byName[name] = len(plan.fields)
		plan.fields = append(plan.fields, bound)
	}
	actual, _ := formPlans.LoadOrStore(structType, plan)
	return actual.(*formPlan), nil
}

func formKindOf(typ reflect.Type) (fieldKind, bool) {
	switch {
	case typ == timeType:
		return kindTime, true
	case typ == uuidType:
		return kindUUID, true
	case reflect.PointerTo(typ).Implements(textUnmarshalType):
		return kindText, true
	}
	switch typ.Kind() {
	case reflect.String:
		return kindString, true
	case reflect.Bool:
		return kindBool, true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return kindInt, true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return kindUint, true
	case reflect.Slice:
		if typ.Elem().Kind() == reflect.String {
			return kindStrings, true
		}
	}
	return 0, false
}

func (f formField) problem(code, message string) *FieldError {
	return &FieldError{Field: f.name, Code: code, Message: message}
}

// assign sets destination from the submitted values; DecodeForm has already
// rejected repeated values for scalar fields.
func (f formField) assign(destination reflect.Value, submitted []string) *FieldError {
	for _, value := range submitted {
		if !utf8.ValidString(value) {
			return f.problem("invalid", "is not valid text")
		}
	}
	if f.kind == kindStrings {
		if submitted == nil {
			destination.SetZero()
			return nil
		}
		destination.Set(reflect.ValueOf(append([]string(nil), submitted...)).Convert(destination.Type()))
		return nil
	}
	value, present := "", len(submitted) == 1
	if present {
		value = submitted[0]
	}
	if f.kind != kindString {
		value = strings.TrimSpace(value)
	}
	if f.pointer {
		if !present || value == "" {
			destination.SetZero()
			return nil
		}
		element := reflect.New(f.typ)
		if problem := f.convert(element.Elem(), value, true); problem != nil {
			destination.SetZero()
			return problem
		}
		destination.Set(element)
		return nil
	}
	return f.convert(destination, value, present)
}

func (f formField) convert(destination reflect.Value, value string, present bool) *FieldError {
	switch f.kind {
	case kindString:
		destination.SetString(value)
		return nil
	case kindBool:
		switch value {
		case "on", "true", "1":
			destination.SetBool(true)
		case "", "off", "false", "0":
			if present && value == "" {
				return f.problem("invalid", "is not a valid choice")
			}
			destination.SetBool(false)
		default:
			return f.problem("invalid", "is not a valid choice")
		}
		return nil
	}
	if value == "" {
		destination.SetZero()
		return f.problem("blank", "can't be blank")
	}
	switch f.kind {
	case kindInt:
		parsed, err := strconv.ParseInt(value, 10, destination.Type().Bits())
		if err != nil {
			return f.numberProblem(err)
		}
		destination.SetInt(parsed)
	case kindUint:
		parsed, err := strconv.ParseUint(value, 10, destination.Type().Bits())
		if err != nil {
			return f.numberProblem(err)
		}
		destination.SetUint(parsed)
	case kindTime:
		parsed, ok := parseFormTime(value)
		if !ok {
			return f.problem("invalid", "is not a valid date and time")
		}
		destination.Set(reflect.ValueOf(parsed))
	case kindUUID:
		parsed, err := uuid.Parse(value)
		if err != nil {
			return f.problem("invalid", "is not a valid identifier")
		}
		destination.Set(reflect.ValueOf(parsed))
	case kindText:
		unmarshaler, _ := destination.Addr().Interface().(encoding.TextUnmarshaler)
		if err := unmarshaler.UnmarshalText([]byte(value)); err != nil {
			return f.problem("invalid", "is invalid")
		}
	}
	return nil
}

func (f formField) numberProblem(err error) *FieldError {
	if errors.Is(err, strconv.ErrRange) {
		return f.problem("out_of_range", "is out of range")
	}
	return f.problem("invalid", "must be a whole number")
}

// formTimeLayouts are RFC 3339 and the values of HTML datetime-local (with
// and without seconds) and date inputs, which carry no zone and are read as
// UTC.
var formTimeLayouts = []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02"}

func parseFormTime(value string) (time.Time, bool) {
	for _, layout := range formTimeLayouts {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC(), true
		}
	}
	return time.Time{}, false
}
