package cli

import (
	"embed"
	"errors"
	"fmt"
	"go/format"
	"io"
	"path/filepath"
	"slices"
	"strings"
)

const mailerUsage = "usage: forge generate mailer NAME METHOD[:field,field...]... [--dry-run]"

const (
	maxMailerMethods         = 20
	maxMailerMethodFields    = 20
	mailerPreviewNameMaximum = 40
)

//go:embed mailertemplates/*.tmpl
var mailerTemplates embed.FS

// mailerRequest is a syntactically valid `generate mailer` command line.
type mailerRequest struct {
	Name    string
	Methods []mailerMethod
	DryRun  bool
	Destroy bool
}

// mailerMethod is one email of a mailer: its name and the string fields its
// templates take.
type mailerMethod struct {
	Name   string
	Fields []string
}

// parseMailerArgs reads `NAME METHOD[:field,field...]... [--dry-run]`. It
// touches no files. Template data is text: every field is a string (rich
// types are a change to the generated data struct).
func parseMailerArgs(args []string) (mailerRequest, error) {
	flags, positional, err := splitFlags(args, mailerUsage, "--dry-run")
	if err != nil {
		return mailerRequest{}, err
	}
	request := mailerRequest{DryRun: flags["--dry-run"]}
	if len(positional) == 0 {
		return mailerRequest{}, errors.New(mailerUsage)
	}
	request.Name = positional[0]
	if err := validateMailerName(request.Name); err != nil {
		return mailerRequest{}, err
	}
	if len(positional) == 1 {
		return mailerRequest{}, errors.New("declare at least one email (method), for example: welcome or welcome:name,url")
	}
	if len(positional)-1 > maxMailerMethods {
		return mailerRequest{}, fmt.Errorf("a mailer has at most %d emails", maxMailerMethods)
	}
	// Generated Go names: the method, its Build twin and the data type.
	goNames := make(map[string]string)
	for _, declaration := range positional[1:] {
		method, err := parseMailerMethod(declaration)
		if err != nil {
			return mailerRequest{}, err
		}
		title := exportedName(method.Name)
		for _, name := range []string{title, "Build" + title, request.titleData(title)} {
			if other, taken := goNames[name]; taken {
				return mailerRequest{}, fmt.Errorf("email %q generates the Go name %s, which email %q already generates; rename one", method.Name, name, other)
			}
			goNames[name] = method.Name
		}
		request.Methods = append(request.Methods, method)
	}
	return request, nil
}

func (r mailerRequest) titleData(methodTitle string) string {
	return exportedName(r.Name) + methodTitle + "Data"
}

func validateMailerName(name string) error {
	switch {
	case len(name) > maxGeneratorNameLength || !snakeName.MatchString(name):
		return fmt.Errorf("mailer name %q must be snake_case (lowercase letters and digits, words separated by single underscores, starting with a letter, at most %d characters)", name, maxGeneratorNameLength)
	case strings.HasSuffix(name, "_mailer"):
		return fmt.Errorf("mailer name %q ends in _mailer, which Forge adds to the Go names; name it %q", name, strings.TrimSuffix(name, "_mailer"))
	}
	return nil
}

func parseMailerMethod(declaration string) (mailerMethod, error) {
	name, fieldList, hasFields := strings.Cut(declaration, ":")
	if len(name) > mailerPreviewNameMaximum || !snakeName.MatchString(name) {
		return mailerMethod{}, fmt.Errorf("email %q must be snake_case (lowercase letters and digits, words separated by single underscores, starting with a letter, at most %d characters), optionally followed by :field,field", name, mailerPreviewNameMaximum)
	}
	method := mailerMethod{Name: name}
	if !hasFields {
		return method, nil
	}
	fields := strings.Split(fieldList, ",")
	if len(fields) > maxMailerMethodFields {
		return mailerMethod{}, fmt.Errorf("email %q has more than %d fields", name, maxMailerMethodFields)
	}
	seen := make(map[string]bool)
	for _, field := range fields {
		if len(field) > maxGeneratorNameLength || !snakeName.MatchString(field) {
			return mailerMethod{}, fmt.Errorf("field %q of email %q must be snake_case (lowercase letters and digits, words separated by single underscores, starting with a letter, at most %d characters); write fields as %s:field,field", field, name, maxGeneratorNameLength, name)
		}
		exported := exportedName(field)
		if seen[exported] {
			return mailerMethod{}, fmt.Errorf("field %q of email %q is declared twice (or exports to the same Go field as another)", field, name)
		}
		seen[exported] = true
		method.Fields = append(method.Fields, field)
	}
	return method, nil
}

// runGenerateMailer is `forge generate mailer`: a mailer type with one typed
// method per email, text and HTML templates, previews and tests, built on the
// framework's mail package.
func runGenerateMailer(args []string, stdout io.Writer, root string) error {
	request, err := parseMailerArgs(args)
	if err != nil {
		return err
	}
	files, err := planMailer(root, request)
	if err != nil {
		return err
	}
	if !request.DryRun {
		if err := applyGeneratedFiles(root, directoriesOf(files), files); err != nil {
			return err
		}
	}
	return reportFiles(stdout, request.DryRun, files, mailerWiring(request, mailerModule(root)))
}

// mailerModule reads the module path for the wiring, or "" when unreadable
// (planMailer has already refused such an application).
func mailerModule(root string) string {
	projectRoot, module, err := openApplication(root)
	if err != nil {
		return ""
	}
	_ = projectRoot.Close()
	return module
}

// mailerView is everything the mailer templates read.
type mailerView struct {
	Name       string // user
	Title      string // User
	Camel      string // user
	HumanTitle string // User
	Module     string
	Methods    []mailerMethodView
}

type mailerMethodView struct {
	Name   string // password_reset
	Title  string // PasswordReset
	Camel  string // passwordReset
	Human  string // password reset
	Fields []mailerFieldView
}

type mailerFieldView struct {
	Name   string // reset_url
	Title  string // ResetURL
	Label  string // Reset url
	Sample string // Sample reset url
}

func newMailerView(request mailerRequest, module string) mailerView {
	view := mailerView{
		Name: request.Name, Title: exportedName(request.Name), Camel: lowerCamel(request.Name),
		HumanTitle: capitalize(strings.ReplaceAll(request.Name, "_", " ")), Module: module,
	}
	for _, method := range request.Methods {
		item := mailerMethodView{
			Name: method.Name, Title: exportedName(method.Name), Camel: lowerCamel(method.Name),
			Human: strings.ReplaceAll(method.Name, "_", " "),
		}
		for _, field := range method.Fields {
			human := strings.ReplaceAll(field, "_", " ")
			item.Fields = append(item.Fields, mailerFieldView{
				Name: field, Title: exportedName(field), Label: capitalize(human), Sample: "Sample " + human,
			})
		}
		view.Methods = append(view.Methods, item)
	}
	return view
}

// mailersDirectory holds every mailer, its templates and its tests.
var mailersDirectory = filepath.Join("app", "mailers")

// planMailer renders every file of request. Nothing is written.
func planMailer(root string, request mailerRequest) ([]generatedFile, error) {
	projectRoot, module, err := openApplication(root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = projectRoot.Close() }()
	view := newMailerView(request, module)

	var files []generatedFile
	for _, source := range []struct{ suffix, template string }{
		{"_mailer.go", "mailer.go.tmpl"},
		{"_mailer_preview.go", "mailer_preview.go.tmpl"},
		{"_mailer_test.go", "mailer_test.go.tmpl"},
	} {
		content, err := renderEmbedded(mailerTemplates, "mailertemplates/"+source.template, view)
		if err != nil {
			return nil, err
		}
		path := filepath.Join(mailersDirectory, request.Name+source.suffix)
		formatted, err := format.Source([]byte(content))
		if err != nil {
			return nil, fmt.Errorf("format generated %s: %w", path, err)
		}
		files = append(files, generatedFile{path: path, content: string(formatted)})
	}
	for _, method := range view.Methods {
		message := struct {
			mailerMethodView
			Mailer, MailerHuman, Subject string
		}{method, request.Name, strings.ToLower(view.HumanTitle), capitalize(method.Human)}
		for _, part := range []struct{ suffix, template string }{
			{".subject.tmpl", "subject.tmpl"},
			{".text.tmpl", "text.tmpl"},
			{".html.gohtml", "html.tmpl"},
		} {
			content, err := renderEmbedded(mailerTemplates, "mailertemplates/"+part.template, message)
			if err != nil {
				return nil, err
			}
			files = append(files, generatedFile{path: filepath.Join(mailersDirectory, "templates", request.Name, method.Name+part.suffix), content: content})
		}
	}
	if err := checkPlannedFiles(projectRoot, files, request.Destroy); err != nil {
		return nil, err
	}
	return files, nil
}

// mailerWiring is what `forge generate mailer` prints: how to build the mailer
// and mount its previews in app/bootstrap/bootstrap.go, which the generator
// never edits.
func mailerWiring(request mailerRequest, module string) string {
	parts := buildMailerWiring(request, module)
	var out strings.Builder
	out.WriteString("\nForge did not edit app/bootstrap/bootstrap.go. Wire the mailer there:\n\n")
	out.WriteString("  1. Add these imports (skip any already present):\n\n")
	for _, path := range parts.Imports {
		fmt.Fprintf(&out, "\t%q\n", path)
	}
	fmt.Fprintf(&out, "\n  2. Once per application, %s, build the sender:\n\n%s", parts.Where, parts.Guard)
	fmt.Fprintf(&out, "\n  3. For this mailer, right after it:\n\n%s", parts.Usage)
	for _, note := range parts.Notes {
		fmt.Fprintf(&out, "\n  %s\n", note)
	}
	out.WriteString("\n  Then: forge test\n")
	return out.String()
}

func buildMailerWiring(request mailerRequest, module string) resourceWiringParts {
	title := exportedName(request.Name)
	camel := lowerCamel(request.Name)
	parts := resourceWiringParts{
		Imports: []string{"context", "errors", "github.com/rocketinsidecorp/forge-go/mail", module + "/app/mailers"},
		Where:   "inside Configure, after the `routes(ctx, app)` call and its error check (in a PostgreSQL application from `forge new`, inside identityRoutes after `middleware, err := auth.NewMiddleware(repository)`; without a database, inside routes before its final `return nil`)",
		Guard: `	mailSender, err := mail.FromConfig(app.Config(), mail.WithLogger(app.Logger()))
	var mailFrom mail.Address
	switch {
	case errors.Is(err, mail.ErrNotConfigured):
		// Mail is optional: until FORGE_MAIL_URL is set, every delivery fails
		// closed with mail.ErrNotConfigured instead of pretending to send.
		app.Logger().Warn("outgoing mail is not configured: set FORGE_MAIL_URL and FORGE_MAIL_FROM")
		mailSender = mail.SenderFunc(func(context.Context, mail.Message) error { return mail.ErrNotConfigured })
		mailFrom = mail.Address{Email: "noreply@example.invalid"}
	case err != nil:
		return err
	default:
		if mailFrom, err = mail.DefaultFrom(app.Config()); err != nil {
			return err // FORGE_MAIL_FROM
		}
	}
`,
	}
	parts.Usage = fmt.Sprintf(`	%[1]sMailer, err := mailers.New%[2]sMailer(mailSender, mailFrom)
	if err != nil {
		return err
	}
	// Previews render each email with fixture data and send nothing. They are
	// served only in development and test, and only with FORGE_MAIL_PREVIEW=true.
	if app.Config().Mail.Preview {
		%[1]sPreviews, err := mail.NewPreviewHandler(app.Config(), "/_mail/%[3]s", mailers.%[2]sMailerPreviews(%[1]sMailer)...)
		if err != nil {
			return err
		}
		if err := app.Handle("GET /_mail/%[3]s/", %[1]sPreviews); err != nil {
			return err
		}
	}
`, camel, title, request.Name)
	parts.Notes = []string{
		fmt.Sprintf("Pass %sMailer to the code that sends these emails; its methods are %s.", camel, mailerMethodList(request)),
		"Until FORGE_MAIL_URL and FORGE_MAIL_FROM are set every delivery fails with mail.ErrNotConfigured and the application logs a warning at start-up. In development use FORGE_MAIL_URL=file:tmp/mails (every message becomes an .eml file) and FORGE_MAIL_FROM=\"App <app@example.com>\"; see docs/MAIL.md. For browser previews set FORGE_MAIL_PREVIEW=true (development and test only), then open /_mail/" + request.Name + "/.",
	}
	return parts
}

func mailerMethodList(request mailerRequest) string {
	var names []string
	for _, method := range request.Methods {
		names = append(names, exportedName(method.Name))
	}
	slices.Sort(names)
	return strings.Join(names, ", ")
}
