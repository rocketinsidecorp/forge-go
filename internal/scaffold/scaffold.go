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
	"path"
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
	// APIAuth also mounts the JSON authentication APIs (`forge new
	// --api-auth`): bearer login and logout, cookie login and logout for
	// scripts, MFA completion, JSON password recovery and GET
	// /v1/auth/session. Without it the HTML pages are the only way to sign
	// in. It needs Forge identity (PostgreSQL).
	APIAuth bool
	// API generates an API-only application (`forge new --api`, like `rails
	// new --api`): JSON handlers only, with no views, assets, JavaScript,
	// translations, sign-in pages or browser middleware (CSRF, method
	// override, flash). It conflicts with APIAuth and with a Locale other
	// than the default, as in Forge-Rust.
	API bool
}

// APIOnlyMarker is the phrase the generated app/bootstrap/bootstrap.go of an
// API-only application carries in its package comment. The generators read it
// to refuse what needs views (pages, scaffolds, HTML controllers, sign-in
// pages) instead of adding a views base to such an application.
const APIOnlyMarker = "API-only application (forge new --api)"

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
	// AppMigrationURL and DevMigrationURL are FORGE_MIGRATION_DATABASE_URL
	// inside compose and on the host: the role that owns the schema, when it
	// differs from the serving one (PostgreSQL).
	AppMigrationURL string
	DevMigrationURL string
	// MFAEncryptionKey is the base64-encoded 32-byte key for MFA secret encryption.
	MFAEncryptionKey string
	// RoutesCall is how bootstrap.go calls the generators' routes
	// (routes_gen.go).
	RoutesCall string
}

type file struct {
	template string
	path     func(name string) string
	scope    fileScope
}

// routesTemplate names the one generated file that is built by code rather
// than a template: the routes seam, whose checksum covers its formatted text.
const routesTemplate = "(routes_gen.go)"

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
	// withPostgres files are generated only for PostgreSQL applications.
	withPostgres
	// withViews files are generated only for full-stack applications (not
	// `forge new --api`): server-rendered views, assets, JavaScript and the
	// translations of the pages.
	withViews
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
	// The routes the generators add (see routes.go); empty at first.
	{routesTemplate, fixed(RoutesFile), everyApplication},
	// The dependency rules of the rings, as a test (see the file's comment).
	{"architecture_test.go.tmpl", fixed(filepath.Join("app", "architecture_test.go")), everyApplication},
	{"welcome.go.tmpl", fixed(filepath.Join("app", "bootstrap", "welcome.go")), everyApplication},
	{"welcome_test.go.tmpl", fixed(filepath.Join("app", "bootstrap", "welcome_test.go")), everyApplication},
	{"app_assets.go.tmpl", fixed(filepath.Join("app", "assets", "assets.go")), withViews},
	{"app_assets_test.go.tmpl", fixed(filepath.Join("app", "assets", "assets_test.go")), withViews},
	{"javascript.go.tmpl", fixed(filepath.Join("app", "javascript", "javascript.go")), withViews},
	{"locales.go.tmpl", fixed(filepath.Join("config", "locales", "locales.go")), withViews},
	{"locales_test.go.tmpl", fixed(filepath.Join("config", "locales", "locales_test.go")), withViews},
	{"views.go.tmpl", fixed(filepath.Join("app", "views", "views.go")), withViews},
	{"views_test.go.tmpl", fixed(filepath.Join("app", "views", "views_test.go")), withViews},
	{"application.gohtml.tmpl", fixed(filepath.Join("app", "views", "layouts", "application.gohtml")), withViews},
	{"flash.gohtml.tmpl", fixed(filepath.Join("app", "views", "shared", "_flash.gohtml")), withViews},
	{"htmlauth.go.tmpl", fixed(filepath.Join("app", "adapters", "htmlauth", "htmlauth.go")), withIdentity},
	{"htmlauth_test.go.tmpl", fixed(filepath.Join("app", "adapters", "htmlauth", "htmlauth_test.go")), withIdentity},
	{"htmlauth_mfa_test.go.tmpl", fixed(filepath.Join("app", "adapters", "htmlauth", "mfa_test.go")), withIdentity},
	{"sessions_new.gohtml.tmpl", fixed(filepath.Join("app", "views", "sessions", "new.gohtml")), withIdentity},
	{"sessions_mfa.gohtml.tmpl", fixed(filepath.Join("app", "views", "sessions", "mfa.gohtml")), withIdentity},
	{"sessions_sign_out.gohtml.tmpl", fixed(filepath.Join("app", "views", "sessions", "_sign_out.gohtml")), withIdentity},
	{"mfa_show.gohtml.tmpl", fixed(filepath.Join("app", "views", "mfa", "show.gohtml")), withIdentity},
	{"mfa_new.gohtml.tmpl", fixed(filepath.Join("app", "views", "mfa", "new.gohtml")), withIdentity},
	{"mfa_backup_codes.gohtml.tmpl", fixed(filepath.Join("app", "views", "mfa", "backup_codes.gohtml")), withIdentity},
	{"mfa_step_up.gohtml.tmpl", fixed(filepath.Join("app", "views", "mfa", "_step_up.gohtml")), withIdentity},
	{"passwords_new.gohtml.tmpl", fixed(filepath.Join("app", "views", "passwords", "new.gohtml")), withIdentity},
	{"passwords_edit.gohtml.tmpl", fixed(filepath.Join("app", "views", "passwords", "edit.gohtml")), withIdentity},
	{"Dockerfile.tmpl", fixed("Dockerfile"), everyApplication},
	{"dockerignore.tmpl", fixed(".dockerignore"), everyApplication},
	{"compose.yaml.tmpl", fixed("compose.yaml"), everyApplication},
	{"postgres_init.sql.tmpl", fixed(filepath.Join("docker", "postgres", "init.sql")), withPostgres},
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

// repositoryFiles are copied unchanged by Generate only (generators that add
// the view base to an existing application leave them alone): GitHub Actions
// expressions use {{ }} too.
var repositoryFiles = []asset{
	{source: "templates/github/release.yml", target: filepath.Join(".github", "workflows", "release.yml")},
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
	if opts.API && opts.APIAuth {
		return Options{}, errors.New("--api-auth cannot be used with --api: an API-only application has no sign-in pages for the JSON authentication APIs to sit beside")
	}
	if opts.API && opts.Locale != i18n.DefaultLocale {
		return Options{}, errors.New("--locale cannot be used with --api: an API-only application has no pages to translate")
	}
	if opts.APIAuth && !hasIdentity(opts) {
		return Options{}, errors.New("--api-auth needs Forge identity, which is PostgreSQL-only: use --database postgresql")
	}
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
	paths := make([]string, 0, len(files)+len(assets)+len(repositoryFiles))
	for _, f := range selectedFiles(opts) {
		relative := f.path(opts.Name)
		target := filepath.Join(opts.Dir, relative)
		if containerRead[relative] {
			if err := writeContainerRead(target, rendered[relative]); err != nil {
				return nil, cleanup(opts.Dir, created, err)
			}
			paths = append(paths, relative)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return nil, cleanup(opts.Dir, created, err)
		}
		if err := os.WriteFile(target, rendered[relative], 0o600); err != nil {
			return nil, cleanup(opts.Dir, created, err)
		}
		paths = append(paths, relative)
	}
	for _, asset := range copiedFiles(opts) {
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

// copiedFiles are the files Generate copies unchanged: the release workflow
// and, in a full-stack application, the assets, the JavaScript and the
// translations of the pages.
func copiedFiles(opts Options) []asset {
	copied := slices.Clone(repositoryFiles)
	if opts.API {
		return copied
	}
	copied = append(slices.Clone(assets), copied...)
	return append(copied, localeAssets(opts.Locale, hasIdentity(opts))...)
}

func parseTemplates(views bool) (*template.Template, error) {
	if views {
		parsed, err := template.New("").Delims("[[", "]]").Option("missingkey=error").ParseFS(templateFS, "templates/views/*"+viewTemplateSuffix)
		if err != nil {
			return nil, fmt.Errorf("parse view templates: %w", err)
		}
		return parsed, nil
	}
	parsed, err := parseFileTemplates(templateFS, fileTemplates)
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	return parsed, nil
}

// fileTemplates are the templates of generated files, one per file.
const fileTemplates = "templates/*.tmpl"

// parseFileTemplates parses every file matching pattern into one set, each
// under its base name, and refuses {{define}} and {{block}}: in a shared set
// a later file's {{define "ci.yml.tmpl"}} silently replaces ci.yml.tmpl, so
// Generate would write text the supply-chain gate never read. Each file is
// parsed alone first and must define nothing but its own name.
func parseFileTemplates(fsys fs.FS, pattern string) (*template.Template, error) {
	names, err := fs.Glob(fsys, pattern)
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		content, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, err
		}
		base := path.Base(name)
		alone, err := template.New(base).Parse(string(content))
		if err != nil {
			return nil, err
		}
		for _, defined := range alone.Templates() {
			if defined.Name() != base {
				return nil, fmt.Errorf("%s: {{define}} and {{block}} are not allowed in a file template (it defines %q)", name, defined.Name())
			}
		}
	}
	return template.New("").Option("missingkey=error").ParseFS(fsys, pattern)
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
		// An API-only application has no sign-in pages, so no Forge
		// identity wiring either (Forge-Rust's --api has no authentication).
		// On PostgreSQL it still composes the identity, audit and session
		// schema (main.go.tmpl), as Forge-Rust's --api applications do.
		data.DB.Identity = hasIdentity(opts)
		data.AppDatabaseURL, data.DevDatabaseURL, data.TestDatabaseURL = databaseURLs(data.DB, data.DBUser, data.DBName, data.DBHostPort)
		data.AppMigrationURL, data.DevMigrationURL = migrationURLs(data.DB, data.DBName, data.DBHostPort)
	}
	data.RoutesCall = RoutesCall(routesAppOf(data))
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
		if f.template == routesTemplate {
			content, err := routesFile(data)
			if err != nil {
				return nil, err
			}
			output[f.path(opts.Name)] = content
			continue
		}
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
// sign-in pages): full-stack PostgreSQL applications.
func hasIdentity(opts Options) bool {
	if opts.API {
		return false
	}
	profile := profileFor(opts.Database, "", "")
	return profile != nil && profile.Identity
}

func selectedFiles(opts Options) []file {
	identity := hasIdentity(opts)
	selected := make([]file, 0, len(files))
	for _, f := range files {
		switch {
		case f.scope == withDatabase && opts.Database == "",
			f.scope == withIdentity && !identity,
			f.scope == withViews && opts.API,
			f.scope == withPostgres && opts.Database != "postgresql":
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

// containerRead lists the generated files a container bind-mounts and reads
// as another user: the postgres image runs docker/postgres/init.sql as its
// own postgres user, which cannot read a 0600 file owned by the developer.
var containerRead = map[string]bool{
	filepath.Join("docker", "postgres", "init.sql"): true,
}

// writeContainerRead writes public, non-secret content that a container reads
// as another user: traversable directories and a world-readable file.
func writeContainerRead(target string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil { //nolint:gosec // G301: public, non-secret configuration
		return err
	}
	// MkdirAll leaves existing directories alone and the umask may have
	// narrowed new ones: make the mount path traversable explicitly.
	if err := os.Chmod(filepath.Dir(target), 0o755); err != nil { //nolint:gosec // G302: public, non-secret configuration
		return err
	}
	if err := os.WriteFile(target, content, 0o644); err != nil { //nolint:gosec // G306: public, non-secret configuration
		return err
	}
	return os.Chmod(target, 0o644) //nolint:gosec // G302: the umask must not hide the file from the container
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
