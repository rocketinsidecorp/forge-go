package cli

import (
	"bytes"
	"embed"
	"fmt"
	"strconv"
	"strings"
	"text/template"
)

// The templates use [[ ]] delimiters because the generated Go code is full of
// literal braces (struct literals, table tests).
//
//go:embed resourcetemplates/*.tmpl
var resourceTemplates embed.FS

type renderedSource struct {
	dir     string // application-relative directory, slash separated
	suffix  string // appended to the resource name: ".go" or "_test.go"
	content string
}

type renderedResource struct {
	migrationUp   string
	migrationDown string
	sources       []renderedSource
}

// resourceView is everything the templates read. All derived names and SQL
// are computed here so the templates stay declarative.
type resourceView struct {
	Name        string // product
	Plural      string // products
	Title       string // Product
	PluralTitle string // Products
	Module      string
	// Command is the generator command line that renders this resource,
	// recorded in its create migration (forge:generate) so that `forge
	// generate tenancy` can render it again.
	Command  string
	Tenant   bool
	Identity bool
	Database string
	Postgres bool
	MySQL    bool
	SQLite   bool
	Fields   []fieldView
	SQL      sqlStatements

	HasText      bool // a string or text field exists (validation helpers)
	HasString    bool
	HasTextType  bool
	HasTime      bool
	HasUUID      bool
	HasOptional  bool
	HasValidated bool
	UsesStrings  bool // the domain calls strings.TrimSpace
}

type fieldView struct {
	resourceField
	Title      string // PriceCents
	Normalizer string // normalizeProductPriceCents
	Err        string // ErrInvalidProductPriceCents
	Message    string // public error message
	Resource   string // Product
	Camel      string // product: the resource in lower camel case, for identifiers
}

func (f fieldView) IsString() bool { return f.Type == "string" }
func (f fieldView) IsText() bool   { return f.Type == "text" }
func (f fieldView) IsInt() bool    { return f.Type == "int" }
func (f fieldView) IsBool() bool   { return f.Type == "bool" }
func (f fieldView) IsTime() bool   { return f.Type == "time" }
func (f fieldView) IsUUID() bool   { return f.Type == "uuid" }

// Validated reports whether the domain checks values of this field's type
// beyond what JSON decoding already guarantees.
func (f fieldView) Validated() bool { return !f.IsInt() && !f.IsBool() }

// GoType is the Go type of one value.
func (f fieldView) GoType() string {
	switch f.Type {
	case "string", "text":
		return "string"
	case "int":
		return "int64"
	case "bool":
		return "bool"
	case "time":
		return "time.Time"
	default:
		return "uuid.UUID"
	}
}

// RequestType is the Go type a JSON request member decodes into. Times and
// UUIDs arrive as strings and are parsed by the handler, so a malformed value
// is reported with the field's own error code instead of a generic decode
// failure.
func (f fieldView) RequestType() string {
	if f.Type == "time" || f.Type == "uuid" {
		return "string"
	}
	return f.GoType()
}

// FieldType is the Go type in the entity and the request structs: a pointer
// for optional fields.
func (f fieldView) FieldType() string {
	if f.Optional {
		return "*" + f.GoType()
	}
	return f.GoType()
}

func newResourceView(spec resourceSpec) resourceView {
	title := exportedName(spec.Name)
	view := resourceView{
		Name: spec.Name, Plural: spec.Plural, Title: title, PluralTitle: exportedName(spec.Plural),
		Module: spec.Module, Command: generatorCommand(spec.resourceRequest), Tenant: spec.TenantScoped, Identity: spec.Identity, Database: spec.Database,
		Postgres: spec.Database == "postgres", MySQL: spec.Database == "mysql", SQLite: spec.Database == "sqlite",
	}
	for _, field := range spec.Fields {
		fieldTitle := exportedName(field.Name)
		item := fieldView{
			resourceField: field, Title: fieldTitle, Resource: title, Camel: lowerCamel(spec.Name),
			Normalizer: "normalize" + title + fieldTitle,
			Err:        "ErrInvalid" + title + fieldTitle,
		}
		item.Message = fieldMessage(item)
		view.Fields = append(view.Fields, item)
		view.HasString = view.HasString || field.Type == "string"
		view.HasTextType = view.HasTextType || field.Type == "text"
		view.HasTime = view.HasTime || field.Type == "time"
		view.HasUUID = view.HasUUID || field.Type == "uuid"
		view.HasOptional = view.HasOptional || field.Optional
		view.HasValidated = view.HasValidated || item.Validated()
		view.UsesStrings = view.UsesStrings || field.Type == "string" || (field.Type == "text" && !field.Optional)
	}
	view.HasText = view.HasString || view.HasTextType
	view.SQL = buildStatements(view)
	return view
}

func fieldMessage(f fieldView) string {
	required := ""
	if !f.Optional {
		required = " (required)"
	}
	switch f.Type {
	case "string":
		if f.Optional {
			return f.Name + " must be at most 255 characters of single-line text"
		}
		return f.Name + " must be 1 to 255 characters of single-line text"
	case "text":
		if f.Optional {
			return f.Name + " must be text of at most 65535 bytes"
		}
		return f.Name + " must be non-blank text of at most 65535 bytes"
	case "int":
		return f.Name + " must be an integer" + required
	case "bool":
		return f.Name + " must be true or false" + required
	case "time":
		return f.Name + " must be an RFC 3339 timestamp between the years 1000 and 9999" + required
	default:
		return f.Name + " must be a UUIDv7" + required
	}
}

// sampleExpr is a valid Go expression of the field's value type, distinct for
// distinct n. Times are whole microseconds in UTC so every database stores
// them exactly.
func sampleExpr(f fieldView, n int) string {
	switch f.Type {
	case "string":
		return strconv.Quote(strings.ReplaceAll(f.Name, "_", " ") + " " + strconv.Itoa(n))
	case "text":
		return strconv.Quote("Text for " + f.Name + " " + strconv.Itoa(n) + ".\nSecond line.")
	case "int":
		return "int64(" + strconv.Itoa(1000+n) + ")"
	case "bool":
		return strconv.FormatBool(n%2 == 1)
	case "time":
		return fmt.Sprintf("time.Date(2026, time.January, %d, 12, 30, 15, 123456000, time.UTC)", 1+n%28)
	default:
		return "uuid.MustNew()"
	}
}

// fieldValue is the sample as an assignable value: new(...) for optional
// fields.
func fieldValue(f fieldView, n int) string {
	if f.Optional {
		return "new(" + sampleExpr(f, n) + ")"
	}
	return sampleExpr(f, n)
}

// SampleInput lists `Field: value,` lines for a domain.<Title>Input literal.
// Optional fields are set only for odd n unless all is true, so tests cover
// both NULL and non-NULL columns.
func (v resourceView) SampleInput(n int, all bool) string {
	var out strings.Builder
	for _, f := range v.Fields {
		if f.Optional && !all && n%2 == 0 {
			continue
		}
		fmt.Fprintf(&out, "%s: %s,\n", f.Title, fieldValue(f, n))
	}
	return out.String()
}

// SampleBody lists `"json_name": value,` lines for a map[string]any request
// body with the same rules as SampleInput.
func (v resourceView) SampleBody(n int, all bool) string {
	var out strings.Builder
	for _, f := range v.Fields {
		if f.Optional && !all && n%2 == 0 {
			continue
		}
		fmt.Fprintf(&out, "%q: %s,\n", f.Name, sampleExpr(f, n))
	}
	return out.String()
}

// SampleChanges lists `Field: &value,` lines for a domain.<Title>Changes
// literal that changes every field.
func (v resourceView) SampleChanges(n int) string {
	var out strings.Builder
	for _, f := range v.Fields {
		fmt.Fprintf(&out, "%s: new(%s),\n", f.Title, sampleExpr(f, n))
	}
	return out.String()
}

// Literal wraps expr in new(...) for optional fields, so it is assignable to
// the entity and input field.
func (f fieldView) Literal(expr string) string {
	if f.Optional {
		return "new(" + expr + ")"
	}
	return expr
}

// SampleExpr is a sample value of the field's value type.
func (f fieldView) SampleExpr(n int) string { return sampleExpr(f, n) }

// SampleValue is a sample assignable to the entity or input field.
func (f fieldView) SampleValue(n int) string { return fieldValue(f, n) }

// Access dereferences an optional field of the entity named by prefix.
func (f fieldView) Access(prefix string) string {
	if f.Optional {
		return "*" + prefix + "." + f.Title
	}
	return prefix + "." + f.Title
}

// invalidCase is a value of Expr (a Go expression of the field's value type)
// that the domain must reject.
type invalidCase struct {
	Name  string
	Field fieldView
	Expr  string
}

// InvalidCases lists values every field's validation must reject.
func (v resourceView) InvalidCases() []invalidCase {
	var cases []invalidCase
	for _, f := range v.Fields {
		add := func(label, expr string) {
			cases = append(cases, invalidCase{Name: f.Name + " " + label, Field: f, Expr: expr})
		}
		switch f.Type {
		case "string":
			if !f.Optional {
				add("empty", `""`)
				add("blank", `"   "`)
			}
			add("too long", `strings.Repeat("a", 256)`)
			add("newline", `"a\nb"`)
			add("NUL", `"a\x00b"`)
			add("invalid UTF-8", `"\xff"`)
		case "text":
			if !f.Optional {
				add("empty", `""`)
				add("blank", `" \n\t "`)
			}
			add("too long", `strings.Repeat("a", 65536)`)
			add("NUL", `"a\x00b"`)
			add("invalid UTF-8", `"\xff"`)
		case "time":
			add("zero", `time.Time{}`)
			add("year before 1000", `time.Date(999, time.December, 31, 23, 59, 59, 0, time.UTC)`)
			add("year after 9999", `time.Date(10000, time.January, 1, 0, 0, 0, 0, time.UTC)`)
		case "uuid":
			add("zero", `uuid.UUID{}`)
			add("not version 7", `uuid.UUID{6: 0x40, 8: 0x80}`)
		}
	}
	return cases
}

// RawInsert is a hand-written INSERT of one row with only the required
// fields, for tests that talk to the table without the repository. It binds
// the id, the organization (when scoped), the required fields, the version and
// both timestamps in that order; RawInsertArgs lists their Go expressions, with
// the organization as orgID.
func (v resourceView) RawInsert() string {
	names, placeholders := v.rawInsertColumns()
	return fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", v.Q(v.Plural), strings.Join(names, ", "), strings.Join(placeholders, ", "))
}

func (v resourceView) rawInsertColumns() (names, placeholders []string) {
	add := func(name string) {
		names = append(names, v.Q(name))
		placeholders = append(placeholders, fmt.Sprintf("$%d", len(names)))
	}
	add("id")
	if v.Tenant {
		add("org_id")
	}
	for _, f := range v.RequiredFields() {
		add(f.Name)
	}
	add("version")
	add("created_at")
	add("updated_at")
	return names, placeholders
}

// RawInsertArgs lists the Go argument expressions for RawInsert.
func (v resourceView) RawInsertArgs() string {
	args := []string{"uuid.MustNew()"}
	if v.Tenant {
		args = append(args, "orgID")
	}
	for _, f := range v.RequiredFields() {
		args = append(args, sampleExpr(f, 1))
	}
	return strings.Join(append(args, "1", "time.Now().UTC()", "time.Now().UTC()"), ", ")
}

// wrongType is a JSON member whose value has the wrong JSON type for its field.
type wrongType struct {
	Field   fieldView
	Literal string
}

// WrongType picks the first field and a JSON value of the wrong JSON type for
// it, which must be reported as invalid_json.
func (v resourceView) WrongType() *wrongType {
	f := v.Fields[0]
	if f.Type == "int" || f.Type == "bool" {
		return &wrongType{Field: f, Literal: `"text"`}
	}
	return &wrongType{Field: f, Literal: "123"}
}

// badValue is a JSON value of the right type that the field must reject with
// its own error code.
type badValue struct {
	Field fieldView
	Value string // a Go expression for the JSON value
}

// BadValues lists, for every field that validates its value, a value that
// decodes but is invalid.
func (v resourceView) BadValues() []badValue {
	var values []badValue
	for _, f := range v.Fields {
		switch {
		case f.IsTime():
			values = append(values, badValue{f, `"yesterday"`})
		case f.IsUUID():
			// Well-formed but version 4: only UUIDv7 is accepted.
			values = append(values, badValue{f, `"550e8400-e29b-41d4-a716-446655440000"`}, badValue{f, `"not-a-uuid"`})
		case (f.IsString() || f.IsText()) && !f.Optional:
			values = append(values, badValue{f, `"   "`})
		}
	}
	return values
}

// FirstRequired is the first required field, or nil.
func (v resourceView) FirstRequired() *fieldView {
	for _, f := range v.Fields {
		if !f.Optional {
			return &f
		}
	}
	return nil
}

// FirstSimpleInvalid is the first invalid value that needs no imports beyond
// the ones every test file has, or nil.
func (v resourceView) FirstSimpleInvalid() *invalidCase {
	for _, c := range v.InvalidCases() {
		if !strings.Contains(c.Expr, "strings.") {
			return &c
		}
	}
	return nil
}

// OptionalFields returns the fields declared with a trailing ?.
func (v resourceView) OptionalFields() []fieldView {
	var fields []fieldView
	for _, f := range v.Fields {
		if f.Optional {
			fields = append(fields, f)
		}
	}
	return fields
}

// RequiredFields returns the fields that must be present.
func (v resourceView) RequiredFields() []fieldView {
	var fields []fieldView
	for _, f := range v.Fields {
		if !f.Optional {
			fields = append(fields, f)
		}
	}
	return fields
}

// Fixed namespaces the template engine exposes.
var resourceTemplateFuncs = template.FuncMap{
	"quote": strconv.Quote,
	"add":   func(a, b int) int { return a + b },
}

func renderTemplate(name string, data any) (string, error) {
	return renderEmbedded(resourceTemplates, "resourcetemplates/"+name, data)
}

// renderEmbedded renders the template at path in files.
func renderEmbedded(files embed.FS, path string, data any) (string, error) {
	name := path[strings.LastIndex(path, "/")+1:]
	text, err := files.ReadFile(path)
	if err != nil {
		return "", err
	}
	parsed, err := template.New(name).Delims("[[", "]]").Funcs(resourceTemplateFuncs).Option("missingkey=error").Parse(string(text))
	if err != nil {
		return "", fmt.Errorf("parse template %s: %w", name, err)
	}
	var out bytes.Buffer
	if err := parsed.Execute(&out, data); err != nil {
		return "", fmt.Errorf("render template %s: %w", name, err)
	}
	return out.String(), nil
}

// renderResource renders the migration pair and every Go source file.
func renderResource(spec resourceSpec) (renderedResource, error) {
	view := newResourceView(spec)
	var result renderedResource
	result.migrationUp, result.migrationDown = renderMigration(view)

	persistence := "persistence_sql.go.tmpl"
	if view.Postgres {
		persistence = "persistence_postgres.go.tmpl"
	}
	for _, source := range []struct {
		dir, suffix, template string
	}{
		{"app/domain", ".go", "domain.go.tmpl"},
		{"app/domain", "_test.go", "domain_test.go.tmpl"},
		{"app/application", ".go", "application.go.tmpl"},
		{"app/application", "_test.go", "application_test.go.tmpl"},
		{"app/adapters/persistence", ".go", persistence},
		{"app/adapters/persistence", "_test.go", "persistence_test.go.tmpl"},
		{"app/adapters/httpapi", ".go", "httpapi.go.tmpl"},
		{"app/adapters/httpapi", "_test.go", "httpapi_test.go.tmpl"},
	} {
		if spec.Model && source.dir == "app/adapters/httpapi" {
			continue
		}
		content, err := renderTemplate(source.template, view)
		if err != nil {
			return renderedResource{}, err
		}
		result.sources = append(result.sources, renderedSource{dir: source.dir, suffix: source.suffix, content: content})
	}
	return result, nil
}

// generatorCommand is the canonical command line of request: the generator,
// the name, the fields with their canonical types, the plural and the flags
// that change what is rendered.
func generatorCommand(request resourceRequest) string {
	kind := "resource"
	switch {
	case request.Model:
		kind = "model"
	case request.Scaffold:
		kind = "scaffold"
	}
	parts := []string{kind, request.Name}
	for _, field := range request.Fields {
		declaration := field.Name + ":" + field.Type
		if field.Optional {
			declaration += "?"
		}
		parts = append(parts, declaration)
	}
	parts = append(parts, "--plural", request.Plural)
	for _, flag := range []struct {
		set  bool
		name string
	}{{request.TenantScoped, "--org-scoped"}, {request.Global, "--global"}, {request.Broadcast, "--broadcast"}, {request.Public, "--public"}} {
		if flag.set {
			parts = append(parts, flag.name)
		}
	}
	return strings.Join(parts, " ")
}
