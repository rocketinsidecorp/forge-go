package cli

import (
	"embed"
	"fmt"
	"strconv"
	"strings"
)

//go:embed scaffoldtemplates/*.tmpl
var scaffoldTemplates embed.FS

// scaffoldView is a resourceView plus what the HTML pages need. The resource
// itself is rendered from the embedded resourceView, so the scaffold can never
// disagree with it about names, types or optionality.
type scaffoldView struct {
	resourceView
	Camel            string // order_item -> orderItem
	PluralCamel      string // order_items -> orderItems
	Human            string // order item
	HumanTitle       string // Order item
	PluralHuman      string // order items
	PluralHumanTitle string // Order items

	HasBool      bool
	NeedsStrconv bool // the pages format an integer or an optional bool
}

func newScaffoldView(spec resourceSpec) scaffoldView {
	view := scaffoldView{
		resourceView: newResourceView(spec),
		Camel:        lowerCamel(spec.Name),
		PluralCamel:  lowerCamel(spec.Plural),
		Human:        strings.ReplaceAll(spec.Name, "_", " "),
		PluralHuman:  strings.ReplaceAll(spec.Plural, "_", " "),
	}
	view.HumanTitle = capitalize(view.Human)
	view.PluralHumanTitle = capitalize(view.PluralHuman)
	for _, field := range view.Fields {
		view.HasBool = view.HasBool || field.IsBool()
		view.NeedsStrconv = view.NeedsStrconv || field.IsInt() || (field.IsBool() && field.Optional)
	}
	return view
}

func capitalize(text string) string {
	return strings.ToUpper(text[:1]) + text[1:]
}

// Label is the field's name for people: price_cents becomes "Price cents".
func (f fieldView) Label() string { return capitalize(strings.ReplaceAll(f.Name, "_", " ")) }

// IsCheckbox reports whether the field is a checkbox. An optional bool is a
// select instead: an unchecked box cannot tell "no" from "not set".
func (f fieldView) IsCheckbox() bool { return f.IsBool() && !f.Optional }

// ValuesType is the Go type of the field in the form's values: text, except
// for a checkbox.
func (f fieldView) ValuesType() string {
	if f.IsCheckbox() {
		return "bool"
	}
	return "string"
}

// uuidV7Pattern is the HTML pattern of a UUIDv7 (the domain still decides).
const uuidV7Pattern = "[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-7[0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}"

// InputAttributes are the attributes of the <input> for the field that depend
// on its type. (Text is a textarea and bool is a checkbox or a select.)
func (f fieldView) InputAttributes() string {
	switch f.Type {
	case "int":
		return `type="number" step="1" inputmode="numeric"`
	case "time":
		return `type="datetime-local" step="any"`
	case "uuid":
		return `type="text" pattern="` + uuidV7Pattern + `" maxlength="36" autocomplete="off" spellcheck="false"`
	default:
		return `type="text" maxlength="255"`
	}
}

// showExpr formats value (a Go expression of the field's value type) for display.
func (f fieldView) showExpr(value string) string {
	switch f.Type {
	case "int":
		return "strconv.FormatInt(" + value + ", 10)"
	case "bool":
		return f.Camel + "YesNo(" + value + ")"
	case "time":
		return receiver(value) + ".UTC().Format(" + f.displayLayout() + ")"
	case "uuid":
		return receiver(value) + ".String()"
	default:
		return value
	}
}

// receiver parenthesizes a dereference so a method can be called on it.
func receiver(value string) string {
	if strings.HasPrefix(value, "*") {
		return "(" + value + ")"
	}
	return value
}

func (f fieldView) displayLayout() string { return f.Camel + "DisplayTime" }

// formExpr formats value for an input.
func (f fieldView) formExpr(value string) string {
	switch f.Type {
	case "int":
		return "strconv.FormatInt(" + value + ", 10)"
	case "bool":
		if f.Optional {
			return "strconv.FormatBool(" + value + ")"
		}
		return value
	case "time":
		return receiver(value) + ".UTC().Format(" + f.Camel + "InputTime)"
	case "uuid":
		return receiver(value) + ".String()"
	default:
		return value
	}
}

// ShowStatement assigns the field of entity to the page item, as text.
func (f fieldView) ShowStatement() string {
	return f.assign("item", "entity", f.showExpr)
}

// FormStatement assigns the field of entity to the form values.
func (f fieldView) FormStatement() string {
	return f.assign("values", "entity", f.formExpr)
}

func (f fieldView) assign(target, entity string, format func(string) string) string {
	if f.Optional {
		return fmt.Sprintf("if %[1]s.%[2]s != nil {\n%[3]s.%[2]s = %[4]s\n}", entity, f.Title, target, format("*"+entity+"."+f.Title))
	}
	return fmt.Sprintf("%s.%s = %s", target, f.Title, format(entity+"."+f.Title))
}

// EchoStatement reads the field back from the submitted request.
func (f fieldView) EchoStatement() string {
	if f.IsCheckbox() {
		return fmt.Sprintf("values.%s = r.PostForm.Get(%q) == \"on\"", f.Title, f.Name)
	}
	return fmt.Sprintf("values.%s = r.PostForm.Get(%q)", f.Title, f.Name)
}

// IndexColumns are the fields the index table shows: up to four, never long
// text.
func (v scaffoldView) IndexColumns() []fieldView {
	var columns []fieldView
	for _, f := range v.Fields {
		if !f.IsText() && len(columns) < 4 {
			columns = append(columns, f)
		}
	}
	return columns
}

// formValue is the text a browser submits for field f in sample n, as a Go
// expression, and whether it submits anything (an unchecked checkbox sends
// nothing).
func formValue(f fieldView, n int) (string, bool) {
	switch f.Type {
	case "string", "text":
		return sampleExpr(f, n), true
	case "int":
		return strconv.Quote(strconv.Itoa(1000 + n)), true
	case "bool":
		if f.Optional {
			return strconv.Quote(strconv.FormatBool(n%2 == 1)), true
		}
		return `"on"`, n%2 == 1
	case "time":
		// What datetime-local submits: minutes, no zone.
		return strconv.Quote(fmt.Sprintf("2026-01-%02dT12:30", 1+n%28)), true
	default:
		return "uuid.MustNew().String()", true
	}
}

// FormSetters lists `values.Set(...)` statements for a submission of sample n
// that every field accepts. Optional fields are left out for even n unless
// all is true, like SampleInput.
func (v scaffoldView) FormSetters(n int, all bool) string {
	var out strings.Builder
	for _, f := range v.Fields {
		if f.Optional && !all && n%2 == 0 {
			continue
		}
		if value, submitted := formValue(f, n); submitted {
			fmt.Fprintf(&out, "\tvalues.Set(%q, %s)\n", f.Name, value)
		}
	}
	return out.String()
}

// pageMarker is text that appears on the pages of a stored sample.
type pageMarker struct{ First, Second string }

// Marker is the first field whose value the pages print verbatim (a string,
// text or integer), as Go expressions for samples 1 and 2, or nil.
func (v scaffoldView) Marker() *pageMarker {
	for _, f := range v.Fields {
		switch f.Type {
		case "string", "text":
			return &pageMarker{First: sampleExpr(f, 1), Second: sampleExpr(f, 2)}
		case "int":
			return &pageMarker{First: strconv.Quote(strconv.Itoa(1001)), Second: strconv.Quote(strconv.Itoa(1002))}
		}
	}
	return nil
}

// InvalidField is the field the problem tests break: the first one.
func (v scaffoldView) InvalidField() fieldView { return v.Fields[0] }

// InvalidValue is a Go string expression for text the first field rejects,
// either when the form is decoded or by the domain.
func (v scaffoldView) InvalidValue() string {
	f := v.Fields[0]
	switch f.Type {
	case "string":
		if f.Optional {
			return `"a\x00b"`
		}
		return `""`
	case "text":
		if f.Optional {
			return `"a\x00b"`
		}
		return `"  "`
	case "int":
		return `"abc"`
	case "bool":
		return `"maybe"`
	case "time":
		return `"yesterday"`
	default:
		return `"not-a-uuid"`
	}
}

// scaffoldPage is one generated view of a resource.
type scaffoldPage struct{ name, template string }

var scaffoldPages = []scaffoldPage{
	{"index", "index.gohtml.tmpl"},
	{"show", "show.gohtml.tmpl"},
	{"new", "new.gohtml.tmpl"},
	{"edit", "edit.gohtml.tmpl"},
	{"_form", "_form.gohtml.tmpl"},
	{"not_found", "not_found.gohtml.tmpl"},
}

func renderScaffoldTemplate(name string, data any) (string, error) {
	return renderEmbedded(scaffoldTemplates, "scaffoldtemplates/"+name, data)
}
