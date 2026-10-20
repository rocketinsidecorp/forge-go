package openapi

import (
	"cmp"
	"encoding"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/rocketinsidecorp/forge-go/uuid"
)

// uuidV7Pattern is the canonical (lowercase) text of a UUIDv7, which is what
// uuid.UUID encodes to.
const uuidV7Pattern = "^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$"

var (
	timeType        = reflect.TypeFor[time.Time]()
	uuidType        = reflect.TypeFor[uuid.UUID]()
	rawMessageType  = reflect.TypeFor[json.RawMessage]()
	typerType       = reflect.TypeFor[Typer]()
	jsonMarshaler   = reflect.TypeFor[json.Marshaler]()
	jsonUnmarshaler = reflect.TypeFor[json.Unmarshaler]()
	textMarshaler   = reflect.TypeFor[encoding.TextMarshaler]()
	textUnmarshaler = reflect.TypeFor[encoding.TextUnmarshaler]()
)

// schemas reflects Go types into JSON Schema. Named struct types become
// components referenced with $ref, which also terminates recursion; the
// component names are assigned once every type is known (assignNames), so the
// result does not depend on the order types were met.
type schemas struct {
	components map[reflect.Type]*component
	typers     map[reflect.Type]bool
}

type component struct {
	typ    reflect.Type
	ref    *componentRef
	schema map[string]any
}

// componentRef marshals to the component's final reference.
type componentRef struct{ name string }

func (r *componentRef) MarshalJSON() ([]byte, error) {
	return json.Marshal("#/components/schemas/" + r.name)
}

func newSchemas() *schemas {
	return &schemas{components: map[reflect.Type]*component{}, typers: map[reflect.Type]bool{}}
}

func describable(t reflect.Type, reason string) error {
	return fmt.Errorf("%w %s: %s", ErrNotDescribable, t, reason)
}

// schema returns the schema of a value of type t as encoding/json encodes it.
// Pointers, slices and maps are nullable because nil encodes as null.
func (s *schemas) schema(t reflect.Type) (map[string]any, error) {
	if result, ok, err := s.special(t); ok || err != nil {
		return result, err
	}
	switch t.Kind() {
	case reflect.Pointer:
		inner, err := s.schema(t.Elem())
		if err != nil {
			return nil, err
		}
		return nullable(inner), nil
	case reflect.Bool:
		return map[string]any{"type": "boolean"}, nil
	case reflect.Int, reflect.Int64, reflect.Uint, reflect.Uint64, reflect.Uintptr, reflect.Uint32:
		result := map[string]any{"type": "integer", "format": "int64"}
		if t.Kind() >= reflect.Uint {
			result["minimum"] = 0
		}
		return result, nil
	case reflect.Int32:
		return map[string]any{"type": "integer", "format": "int32"}, nil
	case reflect.Int8:
		return map[string]any{"type": "integer", "format": "int32", "minimum": math.MinInt8, "maximum": math.MaxInt8}, nil
	case reflect.Int16:
		return map[string]any{"type": "integer", "format": "int32", "minimum": math.MinInt16, "maximum": math.MaxInt16}, nil
	case reflect.Uint8:
		return map[string]any{"type": "integer", "format": "int32", "minimum": 0, "maximum": math.MaxUint8}, nil
	case reflect.Uint16:
		return map[string]any{"type": "integer", "format": "int32", "minimum": 0, "maximum": math.MaxUint16}, nil
	case reflect.Float32:
		return map[string]any{"type": "number", "format": "float"}, nil
	case reflect.Float64:
		return map[string]any{"type": "number", "format": "double"}, nil
	case reflect.String:
		return map[string]any{"type": "string"}, nil
	case reflect.Interface:
		return map[string]any{}, nil
	case reflect.Slice:
		// encoding/json writes []byte as base64 unless the element type
		// has its own encoding.
		if t.Elem().Kind() == reflect.Uint8 && !hasEncoding(t.Elem()) {
			return nullable(map[string]any{"type": "string", "contentEncoding": "base64"}), nil
		}
		items, err := s.schema(t.Elem())
		if err != nil {
			return nil, err
		}
		return nullable(map[string]any{"type": "array", "items": items}), nil
	case reflect.Array:
		items, err := s.schema(t.Elem())
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "array", "items": items, "minItems": t.Len(), "maxItems": t.Len()}, nil
	case reflect.Map:
		key := t.Key()
		switch {
		case key.Kind() == reflect.String,
			key.Kind() >= reflect.Int && key.Kind() <= reflect.Uintptr,
			key.Implements(textMarshaler):
		default:
			return nil, describable(t, "map keys must be strings, integers or encoding.TextMarshaler")
		}
		values, err := s.schema(t.Elem())
		if err != nil {
			return nil, err
		}
		return nullable(map[string]any{"type": "object", "additionalProperties": values}), nil
	case reflect.Struct:
		if t.Name() == "" {
			return s.object(t)
		}
		return s.reference(t)
	default:
		return nil, describable(t, "encoding/json cannot encode "+t.Kind().String()+" values")
	}
}

// special handles types whose JSON encoding is not their Go structure.
func (s *schemas) special(t reflect.Type) (map[string]any, bool, error) {
	switch {
	case t.Implements(typerType) || (t.Kind() != reflect.Pointer && reflect.PointerTo(t).Implements(typerType)):
		if s.typers[t] {
			return nil, true, describable(t, "OpenAPIType refers back to the type itself")
		}
		value := reflect.New(t)
		var typer Typer
		if t.Implements(typerType) {
			typer, _ = value.Elem().Interface().(Typer)
		} else {
			typer, _ = value.Interface().(Typer)
		}
		if typer == nil {
			return nil, true, describable(t, "OpenAPIType is not callable on the zero value")
		}
		target := typer.OpenAPIType()
		if target.IsZero() {
			return nil, true, describable(t, "OpenAPIType returned no type")
		}
		s.typers[t] = true
		defer delete(s.typers, t)
		result, err := s.schema(target.t)
		return result, true, err
	case t == timeType:
		return map[string]any{"type": "string", "format": "date-time"}, true, nil
	case t == uuidType:
		return map[string]any{"type": "string", "format": "uuid", "pattern": uuidV7Pattern, "description": "UUIDv7 (RFC 9562)"}, true, nil
	case t == rawMessageType:
		return map[string]any{}, true, nil
	case implementsEither(t, jsonMarshaler, jsonUnmarshaler):
		// Any JSON: the type encodes itself and does not say how. Implement
		// Typer to describe it.
		return map[string]any{}, true, nil
	case implementsEither(t, textMarshaler, textUnmarshaler):
		return map[string]any{"type": "string"}, true, nil
	}
	return nil, false, nil
}

func implementsEither(t, first, second reflect.Type) bool {
	if t.Kind() == reflect.Pointer || t.Kind() == reflect.Interface {
		return false
	}
	pointer := reflect.PointerTo(t)
	return t.Implements(first) || pointer.Implements(first) || t.Implements(second) || pointer.Implements(second)
}

func hasEncoding(t reflect.Type) bool {
	return implementsEither(t, jsonMarshaler, jsonUnmarshaler) || implementsEither(t, textMarshaler, textUnmarshaler)
}

func (s *schemas) reference(t reflect.Type) (map[string]any, error) {
	entry, ok := s.components[t]
	if !ok {
		entry = &component{typ: t, ref: &componentRef{}}
		s.components[t] = entry
		object, err := s.object(t)
		if err != nil {
			delete(s.components, t)
			return nil, err
		}
		entry.schema = object
	}
	return map[string]any{"$ref": entry.ref}, nil
}

// object describes a struct: its encoding/json fields, required unless
// omitempty or omitzero (or promoted through a nil-able embedded pointer),
// and no other members, which is also what web.DecodeJSON accepts.
func (s *schemas) object(t reflect.Type) (map[string]any, error) {
	properties := map[string]any{}
	var required []string
	for _, field := range jsonFields(t) {
		schema, err := s.schema(field.typ)
		if err != nil {
			return nil, err
		}
		if field.quoted {
			schema = quoted(schema)
		}
		options, err := openapiOptions(field.tag)
		if err != nil {
			return nil, fmt.Errorf("%s.%s: %w", t, field.goName, err)
		}
		schema = annotate(schema, field.tag, options)
		properties[field.name] = schema
		if options.required || (!field.omitEmpty && !field.omitZero && !field.throughPointer) {
			required = append(required, field.name)
		}
	}
	result := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	if len(required) > 0 {
		slices.Sort(required)
		result["required"] = required
	}
	return result, nil
}

type tagOptions struct{ required, nonNull bool }

func openapiOptions(tag reflect.StructTag) (tagOptions, error) {
	var options tagOptions
	value, ok := tag.Lookup("openapi")
	if !ok {
		return options, nil
	}
	for option := range strings.SplitSeq(value, ",") {
		switch strings.TrimSpace(option) {
		case "required":
			options.required = true
		case "nonnull":
			options.nonNull = true
		default:
			return options, fmt.Errorf(`%w: unknown openapi tag option %q (want "required" or "nonnull")`, ErrNotDescribable, option)
		}
	}
	return options, nil
}

// annotate applies the doc, example, format and openapi:"nonnull" tags.
func annotate(schema map[string]any, tag reflect.StructTag, options tagOptions) map[string]any {
	if options.nonNull {
		schema = nonNull(schema)
	}
	if format, ok := tag.Lookup("format"); ok {
		target(schema)["format"] = format
	}
	if doc, ok := tag.Lookup("doc"); ok {
		schema["description"] = doc
	}
	if example, ok := tag.Lookup("example"); ok {
		var value any
		if err := json.Unmarshal([]byte(example), &value); err != nil {
			value = example
		}
		schema["examples"] = []any{value}
	}
	return schema
}

// target returns the schema a format applies to: the non-null branch of a
// nullable reference, or the schema itself.
func target(schema map[string]any) map[string]any {
	if branches, ok := schema["anyOf"].([]any); ok && len(branches) == 2 {
		if first, ok := branches[0].(map[string]any); ok {
			return first
		}
	}
	return schema
}

// quoted is the schema of a scalar field with the ",string" option: the
// value is written inside a JSON string.
func quoted(schema map[string]any) map[string]any {
	if kinds, ok := schema["type"].([]string); ok && slices.Contains(kinds, "null") {
		return map[string]any{"type": []string{"string", "null"}}
	}
	return map[string]any{"type": "string"}
}

func nullable(schema map[string]any) map[string]any {
	switch kind := schema["type"].(type) {
	case string:
		schema["type"] = []string{kind, "null"}
		return schema
	case []string:
		if !slices.Contains(kind, "null") {
			schema["type"] = append(kind, "null")
		}
		return schema
	}
	if len(schema) == 0 {
		return schema // any value, null included
	}
	if branches, ok := schema["anyOf"].([]any); ok {
		for _, branch := range branches {
			if b, ok := branch.(map[string]any); ok && b["type"] == "null" {
				return schema
			}
		}
	}
	return map[string]any{"anyOf": []any{schema, map[string]any{"type": "null"}}}
}

func nonNull(schema map[string]any) map[string]any {
	if kind, ok := schema["type"].([]string); ok {
		kind = slices.DeleteFunc(slices.Clone(kind), func(k string) bool { return k == "null" })
		if len(kind) == 1 {
			schema["type"] = kind[0]
		} else {
			schema["type"] = kind
		}
		return schema
	}
	if branches, ok := schema["anyOf"].([]any); ok && len(branches) == 2 {
		if b, ok := branches[1].(map[string]any); ok && b["type"] == "null" {
			if first, ok := branches[0].(map[string]any); ok {
				return first
			}
		}
	}
	return schema
}

// field is one member of a struct's JSON object.
type field struct {
	name, goName   string
	typ            reflect.Type
	tag            reflect.StructTag
	index          []int
	tagged         bool
	omitEmpty      bool
	omitZero       bool
	quoted         bool
	throughPointer bool
}

// jsonFields lists the fields encoding/json encodes for struct type t,
// applying its rules for embedded structs: promoted fields, the shallowest
// field winning, a tagged field winning at equal depth, and fields that are
// still ambiguous dropped.
func jsonFields(t reflect.Type) []field {
	type pending struct {
		typ            reflect.Type
		index          []int
		throughPointer bool
	}
	var fields []field
	next := []pending{{typ: t}}
	visited := map[reflect.Type]bool{}
	var count, nextCount map[reflect.Type]int
	for len(next) > 0 {
		current := next
		next = nil
		count, nextCount = nextCount, map[reflect.Type]int{}
		for _, parent := range current {
			if visited[parent.typ] {
				continue
			}
			visited[parent.typ] = true
			for i := range parent.typ.NumField() {
				sf := parent.typ.Field(i)
				if sf.Anonymous {
					embedded := sf.Type
					if embedded.Kind() == reflect.Pointer {
						embedded = embedded.Elem()
					}
					if !sf.IsExported() && embedded.Kind() != reflect.Struct {
						continue
					}
				} else if !sf.IsExported() {
					continue
				}
				tag := sf.Tag.Get("json")
				if tag == "-" {
					continue
				}
				name, options, _ := strings.Cut(tag, ",")
				if !validTagName(name) {
					name = ""
				}
				index := append(slices.Clone(parent.index), i)
				fieldType := sf.Type
				if fieldType.Name() == "" && fieldType.Kind() == reflect.Pointer {
					fieldType = fieldType.Elem()
				}
				if name != "" || !sf.Anonymous || fieldType.Kind() != reflect.Struct {
					f := field{
						name: cmp.Or(name, sf.Name), goName: sf.Name, typ: sf.Type, tag: sf.Tag, index: index,
						tagged: name != "", throughPointer: parent.throughPointer,
					}
					for option := range strings.SplitSeq(options, ",") {
						switch option {
						case "omitempty":
							f.omitEmpty = true
						case "omitzero":
							f.omitZero = true
						case "string":
							switch fieldType.Kind() {
							case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
								reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
								reflect.Float32, reflect.Float64, reflect.String:
								f.quoted = true
							}
						}
					}
					fields = append(fields, f)
					if count[parent.typ] > 1 {
						// The same type embedded twice at this depth: its
						// fields annihilate each other below.
						fields = append(fields, fields[len(fields)-1])
					}
					continue
				}
				nextCount[fieldType]++
				if nextCount[fieldType] == 1 {
					next = append(next, pending{typ: fieldType, index: index, throughPointer: parent.throughPointer || sf.Type.Kind() == reflect.Pointer})
				}
			}
		}
	}
	slices.SortStableFunc(fields, func(a, b field) int {
		if c := strings.Compare(a.name, b.name); c != 0 {
			return c
		}
		if c := cmp.Compare(len(a.index), len(b.index)); c != 0 {
			return c
		}
		if a.tagged != b.tagged {
			if a.tagged {
				return -1
			}
			return 1
		}
		return slices.Compare(a.index, b.index)
	})
	var result []field
	for start := 0; start < len(fields); {
		end := start + 1
		for end < len(fields) && fields[end].name == fields[start].name {
			end++
		}
		group := fields[start:end]
		if len(group) == 1 || len(group[0].index) != len(group[1].index) || group[0].tagged != group[1].tagged {
			result = append(result, group[0])
		}
		start = end
	}
	return result
}

// validTagName mirrors encoding/json's accepted tag names.
func validTagName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		switch {
		case strings.ContainsRune("!#$%&()*+-./:;<=>?@[]^_{|}~ ", r):
		case !unicode.IsLetter(r) && !unicode.IsDigit(r):
			return false
		}
	}
	return true
}

// assignNames names every component after its Go type, with the first letter
// upper-cased and type arguments reduced to their names
// (pagination.Page[app/httpapi.postResponse] is Page_PostResponse). Types that
// would share a name are qualified with their package name, then with their
// full package path.
func (s *schemas) assignNames() map[string]map[string]any {
	byName := map[string][]*component{}
	for _, entry := range s.components {
		name := shortName(entry.typ)
		byName[name] = append(byName[name], entry)
	}
	result := map[string]map[string]any{}
	for name, entries := range byName {
		if len(entries) == 1 {
			entries[0].ref.name = name
			result[name] = entries[0].schema
			continue
		}
		qualified := map[string]int{}
		for _, entry := range entries {
			qualified[packageName(entry.typ)+"."+name]++
		}
		for _, entry := range entries {
			candidate := packageName(entry.typ) + "." + name
			if qualified[candidate] > 1 {
				candidate = sanitize(entry.typ.PkgPath()) + "." + name
			}
			entry.ref.name = candidate
			result[candidate] = entry.schema
		}
	}
	return result
}

func packageName(t reflect.Type) string {
	path := t.PkgPath()
	return sanitize(path[strings.LastIndex(path, "/")+1:])
}

func shortName(t reflect.Type) string {
	var builder strings.Builder
	token := func(text string) {
		if slash := strings.LastIndex(text, "/"); slash >= 0 {
			text = text[slash+1:]
		}
		if dot := strings.LastIndex(text, "."); dot >= 0 {
			text = text[dot+1:]
		}
		builder.WriteString(upperFirst(text))
	}
	name := t.Name()
	start := 0
	for i, r := range name {
		if strings.ContainsRune("[], *", r) {
			token(name[start:i])
			builder.WriteByte('_')
			start = i + utf8.RuneLen(r)
		}
	}
	token(name[start:])
	return sanitize(strings.Trim(collapse(builder.String()), "_"))
}

func collapse(name string) string {
	for strings.Contains(name, "__") {
		name = strings.ReplaceAll(name, "__", "_")
	}
	return name
}

func upperFirst(text string) string {
	r, size := utf8.DecodeRuneInString(text)
	if r == utf8.RuneError {
		return text
	}
	return string(unicode.ToUpper(r)) + text[size:]
}

// sanitize keeps the characters OpenAPI allows in component names.
func sanitize(name string) string {
	return strings.Map(func(r rune) rune {
		if r < utf8.RuneSelf && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '-' || r == '_') {
			return r
		}
		return '_'
	}, name)
}
