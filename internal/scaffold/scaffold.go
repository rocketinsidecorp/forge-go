// Package scaffold generates new Forge applications. Every generated
// application is containerized by default: it ships a production Dockerfile,
// a .dockerignore, a compose stack and a CI workflow that builds the image.
package scaffold

import (
	"bytes"
	"crypto/rand"
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"go/format"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"text/template"

	"github.com/rocketinsidecorp/forge-go/credentials"
	"github.com/rocketinsidecorp/forge-go/i18n"
)

// FrameworkModule is the module path generated applications depend on.
const FrameworkModule = "github.com/rocketinsidecorp/forge-go"

//go:embed all:templates
var templateFS embed.FS

var (
	namePattern   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	modulePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._~-]*(/[A-Za-z0-9._~-]+)*$`)
)

// Options describes the application to generate.
type Options struct {
	// Name is the application and binary name: lowercase letters, digits and hyphens.
	Name string
	// Module is the Go module path; it defaults to Name.
	Module string
	// Dir is the target directory; it defaults to Name and must be absent or empty.
	Dir string
	// Database is one of Databases (aliases such as "postgres" and
	// "sqlite3" are accepted) or empty for none. It selects the adapter,
	// the compose and CI services and the development settings.
	Database string
	// Locale is the default locale, one of Locales; empty means "en". Its
	// translations are generated next to en.json.
	Locale string
}

// Locales are the locales Forge ships translations of the generated pages
// for: `forge new --locale` accepts them.
var Locales = []string{"en", "es", "pt-BR"}

// templateData is what templates see.
type templateData struct {
	Options
	// DB is nil without a database.
	DB *databaseProfile
	// DBUser and DBName are identifiers derived from Name.
	DBUser string
	DBName string
	// DBHostPort is where compose publishes a database server on the host.
	DBHostPort int
	// AppDatabaseURL is used inside compose, DevDatabaseURL on the host and
	// TestDatabaseURL by tests on the host (empty when tests need none).
	AppDatabaseURL  string
	DevDatabaseURL  string
	TestDatabaseURL string
	// MFAEncryptionKey is the base64-encoded 32-byte key for MFA secret encryption.
	MFAEncryptionKey string
}

type file struct {
	template string
	path     func(name string) string
	scope    fileScope
}

// fileScope selects the applications a file is generated for.
type fileScope int

const (
	everyApplication fileScope = iota
	// withDatabase files are generated only with Options.Database.
	withDatabase
	// withIdentity files are generated only for databases with Forge
	// identity (PostgreSQL): the HTML sign-in, sign-out and password pages.
	// `forge generate authentication` renders the same files.
	withIdentity
)

type asset struct {
	source string
	target string
}

func fixed(path string) func(string) string { return func(string) string { return path } }

var files = []file{
	{"go.mod.tmpl", fixed("go.mod"), everyApplication},
	{"main.go.tmpl", func(name string) string { return filepath.Join("cmd", name, "main.go") }, everyApplication},
	{"bootstrap.go.tmpl", fixed(filepath.Join("app", "bootstrap", "bootstrap.go")), everyApplication},
	{"bootstrap_test.go.tmpl", fixed(filepath.Join("app", "bootstrap", "bootstrap_test.go")), everyApplication},
	{"welcome.go.tmpl", fixed(filepath.Join("app", "bootstrap", "welcome.go")), everyApplication},
	{"app_assets.go.tmpl", fixed(filepath.Join("app", "assets", "assets.go")), everyApplication},
	{"app_assets_test.go.tmpl", fixed(filepath.Join("app", "assets", "assets_test.go")), everyApplication},
	{"javascript.go.tmpl", fixed(filepath.Join("app", "javascript", "javascript.go")), everyApplication},
	{"locales.go.tmpl", fixed(filepath.Join("config", "locales", "locales.go")), everyApplication},
	{"locales_test.go.tmpl", fixed(filepath.Join("config", "locales", "locales_test.go")), everyApplication},
	{"views.go.tmpl", fixed(filepath.Join("app", "views", "views.go")), everyApplication},
	{"views_test.go.tmpl", fixed(filepath.Join("app", "views", "views_test.go")), everyApplication},
	{"application.gohtml.tmpl", fixed(filepath.Join("app", "views", "layouts", "application.gohtml")), everyApplication},
	{"flash.gohtml.tmpl", fixed(filepath.Join("app", "views", "shared", "_flash.gohtml")), everyApplication},
	{"htmlauth.go.tmpl", fixed(filepath.Join("app", "adapters", "htmlauth", "htmlauth.go")), withIdentity},
	{"htmlauth_test.go.tmpl", fixed(filepath.Join("app", "adapters", "htmlauth", "htmlauth_test.go")), withIdentity},
	{"sessions_new.gohtml.tmpl", fixed(filepath.Join("app", "views", "sessions", "new.gohtml")), withIdentity},
	{"sessions_mfa.gohtml.tmpl", fixed(filepath.Join("app", "views", "sessions", "mfa.gohtml")), withIdentity},
	{"sessions_sign_out.gohtml.tmpl", fixed(filepath.Join("app", "views", "sessions", "_sign_out.gohtml")), withIdentity},
	{"passwords_new.gohtml.tmpl", fixed(filepath.Join("app", "views", "passwords", "new.gohtml")), withIdentity},
	{"passwords_edit.gohtml.tmpl", fixed(filepath.Join("app", "views", "passwords", "edit.gohtml")), withIdentity},
	{"Dockerfile.tmpl", fixed("Dockerfile"), everyApplication},
	{"dockerignore.tmpl", fixed(".dockerignore"), everyApplication},
	{"compose.yaml.tmpl", fixed("compose.yaml"), everyApplication},
	{"compose.observability.yaml.tmpl", fixed("compose.observability.yaml"), everyApplication},
	{"gitignore.tmpl", fixed(".gitignore"), everyApplication},
	{"ci.yml.tmpl", fixed(filepath.Join(".github", "workflows", "ci.yml")), everyApplication},
	{"dependabot.yml.tmpl", fixed(filepath.Join(".github", "dependabot.yml")), everyApplication},
	{"README.md.tmpl", fixed("README.md"), everyApplication},
	{"env.development.tmpl", fixed(".env.development"), everyApplication},
	{"env.local.tmpl", fixed(".env.local"), withDatabase},
	{"db.go.tmpl", fixed(filepath.Join("db", "db.go")), withDatabase},
	{"seeds.go.tmpl", fixed(filepath.Join("db", "seeds.go")), withDatabase},
	{"keep.tmpl", fixed(filepath.Join("db", "migrations", ".keep")), withDatabase},
}

var assets = []asset{
	{source: "templates/assets/forge-go-logo.png", target: filepath.Join("app", "assets", "forge-go-logo.png")},
	{source: "templates/assets/application.css", target: filepath.Join("app", "assets", "application.css")},
	{source: "templates/javascript/application.js", target: filepath.Join("app", "javascript", "application.js")},
	{source: "templates/javascript/controllers/application.js", target: filepath.Join("app", "javascript", "controllers", "application.js")},
	{source: "templates/javascript/controllers/index.js", target: filepath.Join("app", "javascript", "controllers", "index.js")},
	{source: "templates/javascript/controllers/hello_controller.js", target: filepath.Join("app", "javascript", "controllers", "hello_controller.js")},
}

// observabilityDir is the embedded directory of static configuration for the
// opt-in observability stack (compose.observability.yaml). Its files are
// copied unchanged, so they may contain {{ }} and $ sequences that belong to
// Grafana and Prometheus. Compose bind-mounts them into containers that run as
// other users, so they are written world-readable (they hold no secrets).
const observabilityDir = "templates/observability"

// viewTemplateSuffix marks templates that produce Go HTML templates. They are
// parsed with [[ ]] delimiters so the {{ }} actions pass through unchanged.
const viewTemplateSuffix = ".gohtml.tmpl"

// Normalize validates options and fills defaults.
func Normalize(opts Options) (Options, error) {
	if !namePattern.MatchString(opts.Name) {
		return Options{}, errors.New("application name must start with a lowercase letter and contain only lowercase letters, digits and hyphens (max 63)")
	}
	if opts.Module == "" {
		opts.Module = opts.Name
	}
	if !modulePattern.MatchString(opts.Module) || strings.Contains(opts.Module, "..") {
		return Options{}, fmt.Errorf("invalid Go module path %q", opts.Module)
	}
	if opts.Dir == "" {
		opts.Dir = opts.Name
	}
	database, err := normalizeDatabase(opts.Database)
	if err != nil {
		return Options{}, err
	}
	opts.Database = database
	if opts.Locale == "" {
		opts.Locale = i18n.DefaultLocale
	}
	locale, err := i18n.CanonicalLocale(opts.Locale)
	if err != nil || !slices.Contains(Locales, locale) {
		return Options{}, fmt.Errorf("unsupported locale %q: Forge generates translations for %s; add others to config/locales afterwards (docs/I18N.md)", opts.Locale, strings.Join(Locales, ", "))
	}
	opts.Locale = locale
	return opts, nil
}

// localeAssets are the translation files of the generated pages: en.json,
// and the default locale's when it is another one, plus the sign-in pages'
// authentication.LOCALE.json with Forge identity.
func localeAssets(locale string, identity bool) []asset {
	locales := []string{i18n.DefaultLocale}
	if locale != "" && locale != i18n.DefaultLocale {
		locales = append(locales, locale)
	}
	var result []asset
	for _, name := range locales {
		result = append(result, asset{source: "templates/locales/" + name + ".json", target: filepath.Join("config", "locales", name+".json")})
		if identity {
			result = append(result, authenticationTranslation(name))
		}
	}
	return result
}

func authenticationTranslation(locale string) asset {
	name := "authentication." + locale + ".json"
	return asset{source: "templates/locales/" + name, target: filepath.Join("config", "locales", name)}
}

// AuthenticationTranslation returns config/locales/authentication.LOCALE.json,
// the text of the sign-in, two-factor and password-reset pages, for a locale
// in Locales.
func AuthenticationTranslation(locale string) (BaseFile, error) {
	if !slices.Contains(Locales, locale) {
		return BaseFile{}, fmt.Errorf("no authentication translations for %q", locale)
	}
	file := authenticationTranslation(locale)
	content, err := templateFS.ReadFile(file.source)
	if err != nil {
		return BaseFile{}, err
	}
	return BaseFile{Path: file.target, Content: content}, nil
}

// I18nBase returns the translations package `forge new` generates
// (config/locales: locales.go, its test and en.json). Generators add it to an
// application that lacks it, so pages that use t have their translations.
func I18nBase(name, module string) ([]BaseFile, error) {
	opts, err := Normalize(Options{Name: name, Module: module})
	if err != nil {
		return nil, err
	}
	rendered, err := renderSelected(opts, func(f file) bool {
		return strings.HasPrefix(f.path(""), filepath.Join("config", "locales")+string(filepath.Separator))
	})
	if err != nil {
		return nil, err
	}
	for _, locale := range localeAssets(opts.Locale, false) {
		content, err := templateFS.ReadFile(locale.source)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", locale.source, err)
		}
		rendered = append(rendered, BaseFile{Path: locale.target, Content: content})
	}
	return rendered, nil
}

// Generate renders every file and writes it under opts.Dir. It never
// overwrites: an existing non-empty target directory is rejected, and a
// directory created by Generate is removed again if writing fails.
func Generate(opts Options) ([]string, error) {
	opts, err := Normalize(opts)
	if err != nil {
		return nil, err
	}
	rendered, err := render(opts)
	if err != nil {
		return nil, err
	}

	created, err := prepareDir(opts.Dir)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(files)+len(assets))
	for _, f := range selectedFiles(opts) {
		relative := f.path(opts.Name)
		target := filepath.Join(opts.Dir, relative)
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return nil, cleanup(opts.Dir, created, err)
		}
		if err := os.WriteFile(target, rendered[relative], 0o600); err != nil {
			return nil, cleanup(opts.Dir, created, err)
		}
		paths = append(paths, relative)
	}
	for _, asset := range append(slices.Clone(assets), localeAssets(opts.Locale, hasIdentity(opts))...) {
		content, err := templateFS.ReadFile(asset.source)
		if err != nil {
			return nil, cleanup(opts.Dir, created, fmt.Errorf("read asset %s: %w", asset.source, err))
		}
		target := filepath.Join(opts.Dir, asset.target)
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return nil, cleanup(opts.Dir, created, err)
		}
		if err := os.WriteFile(target, content, 0o600); err != nil {
			return nil, cleanup(opts.Dir, created, err)
		}
		paths = append(paths, asset.target)
	}
	observability, err := writeObservability(opts.Dir)
	if err != nil {
		return nil, cleanup(opts.Dir, created, err)
	}
	paths = append(paths, observability...)
	// Encrypted credentials for development: config/credentials/development.json.enc
	// (committed) and its key (git-ignored, mode 0600).
	if _, err := credentials.Init(filepath.Join(opts.Dir, credentials.DefaultDir), "development"); err != nil {
		return nil, cleanup(opts.Dir, created, fmt.Errorf("create development credentials: %w", err))
	}
	paths = append(paths,
		filepath.Join(credentials.DefaultDir, credentials.KeyFileName("development")),
		filepath.Join(credentials.DefaultDir, credentials.FileName("development")))
	return paths, nil
}

func parseTemplates(views bool) (*template.Template, error) {
	if views {
		parsed, err := template.New("").Delims("[[", "]]").Option("missingkey=error").ParseFS(templateFS, "templates/views/*"+viewTemplateSuffix)
		if err != nil {
			return nil, fmt.Errorf("parse view templates: %w", err)
		}
		return parsed, nil
	}
	parsed, err := template.New("").Option("missingkey=error").ParseFS(templateFS, "templates/*.tmpl")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	return parsed, nil
}

func render(opts Options) (map[string][]byte, error) {
	templates, err := parseTemplates(false)
	if err != nil {
		return nil, err
	}
	viewTemplates, err := parseTemplates(true)
	if err != nil {
		return nil, err
	}
	data := templateData{Options: opts, DBHostPort: dbHostPort(opts.Name)}
	data.DBUser, data.DBName = databaseIdentifiers(opts.Name)
	if data.DB = profileFor(opts.Database, data.DBUser, data.DBName); data.DB != nil {
		data.AppDatabaseURL, data.DevDatabaseURL, data.TestDatabaseURL = databaseURLs(data.DB, data.DBUser, data.DBName, data.DBHostPort)
	}
	// Keep the generated MFA key in the git-ignored .env.local, not the
	// committed .env.development defaults.
	if data.DB != nil && data.DB.Identity {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, fmt.Errorf("generate MFA encryption key: %w", err)
		}
		data.MFAEncryptionKey = base64.RawURLEncoding.EncodeToString(key)
	}
	output := make(map[string][]byte, len(files))
	for _, f := range selectedFiles(opts) {
		var buffer bytes.Buffer
		set := templates
		if strings.HasSuffix(f.template, viewTemplateSuffix) {
			set = viewTemplates
		}
		if err := set.ExecuteTemplate(&buffer, f.template, data); err != nil {
			return nil, fmt.Errorf("render %s: %w", f.template, err)
		}
		content := buffer.Bytes()
		path := f.path(opts.Name)
		if strings.HasSuffix(path, ".go") {
			if content, err = format.Source(content); err != nil {
				return nil, fmt.Errorf("format %s: %w", path, err)
			}
		}
		output[path] = content
	}
	return output, nil
}

// hasIdentity reports whether the application gets Forge identity (the
// sign-in pages): PostgreSQL applications.
func hasIdentity(opts Options) bool {
	profile := profileFor(opts.Database, "", "")
	return profile != nil && profile.Identity
}

func selectedFiles(opts Options) []file {
	identity := hasIdentity(opts)
	selected := make([]file, 0, len(files))
	for _, f := range files {
		switch {
		case f.scope == withDatabase && opts.Database == "",
			f.scope == withIdentity && !identity:
			continue
		}
		selected = append(selected, f)
	}
	return selected
}

func prepareDir(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return false, err
		}
		return true, nil
	case err != nil:
		return false, err
	case len(entries) > 0:
		return false, fmt.Errorf("target directory %s is not empty", dir)
	default:
		return false, nil
	}
}

func cleanup(dir string, created bool, cause error) error {
	if created {
		if err := os.RemoveAll(dir); err != nil {
			return errors.Join(cause, fmt.Errorf("remove partially generated %s: %w", dir, err))
		}
	}
	return cause
}

// writeObservability copies the static observability configuration under dir
// and returns the written paths relative to dir.
func writeObservability(dir string) ([]string, error) {
	var paths []string
	err := fs.WalkDir(templateFS, observabilityDir, func(source string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		content, err := templateFS.ReadFile(source)
		if err != nil {
			return fmt.Errorf("read %s: %w", source, err)
		}
		relative := filepath.FromSlash(strings.TrimPrefix(source, "templates/"))
		target := filepath.Join(dir, relative)
		// Containers read these files as other users: directories must be
		// traversable and files readable by everyone. No secrets live here.
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil { //nolint:gosec // G301: public, non-secret configuration
			return err
		}
		if err := os.WriteFile(target, content, 0o644); err != nil { //nolint:gosec // G306: public, non-secret configuration
			return err
		}
		paths = append(paths, relative)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("write observability configuration: %w", err)
	}
	return paths, nil
}
