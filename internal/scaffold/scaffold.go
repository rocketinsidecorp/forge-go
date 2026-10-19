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
	"strings"
	"text/template"
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
}

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
	// databaseOnly files are generated only with Options.Database.
	databaseOnly bool
}

type asset struct {
	source string
	target string
}

func fixed(path string) func(string) string { return func(string) string { return path } }

var files = []file{
	{"go.mod.tmpl", fixed("go.mod"), false},
	{"main.go.tmpl", func(name string) string { return filepath.Join("cmd", name, "main.go") }, false},
	{"bootstrap.go.tmpl", fixed(filepath.Join("app", "bootstrap", "bootstrap.go")), false},
	{"bootstrap_test.go.tmpl", fixed(filepath.Join("app", "bootstrap", "bootstrap_test.go")), false},
	{"welcome.go.tmpl", fixed(filepath.Join("app", "bootstrap", "welcome.go")), false},
	{"views.go.tmpl", fixed(filepath.Join("app", "views", "views.go")), false},
	{"views_test.go.tmpl", fixed(filepath.Join("app", "views", "views_test.go")), false},
	{"application.gohtml.tmpl", fixed(filepath.Join("app", "views", "layouts", "application.gohtml")), false},
	{"flash.gohtml.tmpl", fixed(filepath.Join("app", "views", "shared", "_flash.gohtml")), false},
	{"Dockerfile.tmpl", fixed("Dockerfile"), false},
	{"dockerignore.tmpl", fixed(".dockerignore"), false},
	{"compose.yaml.tmpl", fixed("compose.yaml"), false},
	{"gitignore.tmpl", fixed(".gitignore"), false},
	{"ci.yml.tmpl", fixed(filepath.Join(".github", "workflows", "ci.yml")), false},
	{"dependabot.yml.tmpl", fixed(filepath.Join(".github", "dependabot.yml")), false},
	{"README.md.tmpl", fixed("README.md"), false},
	{"env.development.tmpl", fixed(".env.development"), false},
	{"env.local.tmpl", fixed(".env.local"), true},
	{"db.go.tmpl", fixed(filepath.Join("db", "db.go")), true},
	{"keep.tmpl", fixed(filepath.Join("db", "migrations", ".keep")), true},
}

var assets = []asset{{
	source: "templates/assets/forge-go-logo.png",
	target: filepath.Join("app", "bootstrap", "assets", "forge-go-logo.png"),
}, {
	source: "templates/assets/application.css",
	target: filepath.Join("app", "bootstrap", "assets", "application.css"),
}}

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
	return opts, nil
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
	for _, asset := range assets {
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

func selectedFiles(opts Options) []file {
	selected := make([]file, 0, len(files))
	for _, f := range files {
		if !f.databaseOnly || opts.Database != "" {
			selected = append(selected, f)
		}
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
