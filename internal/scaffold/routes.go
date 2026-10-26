package scaffold

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// The generators' seam. `forge generate scaffold`, `resource` and
// `controller` mount their routes in app/bootstrap/routes_gen.go, which they
// own and rewrite whole, instead of printing wiring for the application owner
// to paste. bootstrap.go, the owner's code, calls RoutesFunction once and is
// never edited by a generator. The file is a list of blocks, each opened by a
// "// forge:KIND NAME" comment: the shared ones (guards, the view renderer,
// the live-update stream) first, then one per generated resource, scaffold or
// controller in the order they were generated. A checksum line records what
// the generator last wrote, so a hand edit is detected and refused rather
// than overwritten.

// RoutesFile is the application-relative path of the seam.
var RoutesFile = filepath.Join("app", "bootstrap", "routes_gen.go")

// RoutesFunction is the function routes_gen.go declares and bootstrap.go
// calls. The generators wire nothing in an application whose bootstrap.go
// does not call it (one created before the seam existed): they print the
// wiring instead.
const RoutesFunction = "generatedRoutes"

// RoutesApp is what decides the signature of RoutesFunction: the parameters
// bootstrap.go passes, which the blocks may use.
type RoutesApp struct {
	Module string
	// Adapter is postgres, mysql or sqlite, or empty without a database.
	Adapter string
	// API reports an API-only application (`forge new --api`): no
	// translator, since there are no pages.
	API bool
	// Identity reports Forge identity (full-stack PostgreSQL): the session
	// repository and the authentication middleware are passed too.
	Identity bool
}

// RouteBlock is one block of routes_gen.go.
type RouteBlock struct {
	// Kind is guard, signer, renderer or streams for the shared blocks,
	// resource, scaffold or controller for a generator's own.
	Kind string
	// Name is the variable a shared block declares, or the plural (resource,
	// scaffold) or name (controller) of a generated one.
	Name string
	// Body is the statements, indented with one tab, each line ending in a
	// newline.
	Body string
}

// ID is "KIND NAME", the text of the block's marker after "forge:".
func (b RouteBlock) ID() string { return b.Kind + " " + b.Name }

// Shared reports whether the block is shared by the generated ones rather
// than written for one of them.
func (b RouteBlock) Shared() bool {
	switch b.Kind {
	case "guard", "signer", "renderer", "streams":
		return true
	}
	return false
}

// ErrRoutesEdited reports a routes_gen.go whose checksum does not match its
// content: someone edited it after a generator wrote it.
var ErrRoutesEdited = errors.New("app/bootstrap/routes_gen.go was edited by hand since a generator last wrote it (its forge:checksum no longer matches), so nothing was written. " +
	"Generators own that file: move your change into bootstrap.go (or, to replace a guard such as protect, declare a function of that name in another file of package bootstrap), then restore the file with `git checkout app/bootstrap/routes_gen.go` and run the command again")

const (
	checksumPrefix      = "// forge:checksum "
	checksumPlaceholder = "0000000000000000000000000000000000000000000000000000000000000000"
)

var markerLine = regexp.MustCompile(`^\t// forge:([a-z]+) ([A-Za-z_][A-Za-z0-9_]*)$`)

// routesPackages are the packages a block may use, by the name it uses them
// under. Imports are derived from the code: a package is imported exactly
// when a block (or the signature) refers to it.
func routesPackages(module, adapter string) map[string]string {
	packages := map[string]string{
		"context":      "context",
		"http":         "net/http",
		"os":           "os",
		"time":         "time",
		"forge":        FrameworkModule,
		"auth":         FrameworkModule + "/auth",
		"authpostgres": FrameworkModule + "/auth/postgres",
		"broadcast":    FrameworkModule + "/broadcast",
		"fault":        FrameworkModule + "/fault",
		"i18n":         FrameworkModule + "/i18n",
		"sqldb":        FrameworkModule + "/sqldb",
		"uuid":         FrameworkModule + "/uuid",
		"web":          FrameworkModule + "/web",
		"application":  module + "/app/application",
		"htmlstreams":  module + "/app/adapters/htmlstreams",
		"htmlui":       module + "/app/adapters/htmlui",
		"httpapi":      module + "/app/adapters/httpapi",
		"persistence":  module + "/app/adapters/persistence",
		"views":        module + "/app/views",
	}
	if adapter != "" {
		packages[adapter] = FrameworkModule + "/" + adapter
	}
	return packages
}

// routesAliases are the imports whose name differs from the last element of
// their path.
var routesAliases = map[string]bool{"forge": true, "authpostgres": true}

// RoutesParameters is the parameter list of RoutesFunction for app.
func RoutesParameters(app RoutesApp) string {
	parameters := []string{"ctx context.Context", "app *forge.App"}
	switch app.Adapter {
	case "":
	case "postgres":
		parameters = append(parameters, "database *postgres.DB")
	default:
		parameters = append(parameters, "database *sqldb.DB")
	}
	if !app.API {
		parameters = append(parameters, "translator *i18n.Translator")
	}
	if app.Identity {
		parameters = append(parameters, "repository *authpostgres.Repository", "middleware *auth.Middleware")
	}
	return strings.Join(parameters, ", ")
}

// RoutesCall is the statement bootstrap.go runs RoutesFunction with.
func RoutesCall(app RoutesApp) string {
	arguments := []string{"ctx", "app"}
	if app.Adapter != "" {
		arguments = append(arguments, "database")
	}
	if !app.API {
		arguments = append(arguments, "translator")
	}
	if app.Identity {
		arguments = append(arguments, "repository", "middleware")
	}
	return RoutesFunction + "(" + strings.Join(arguments, ", ") + ")"
}

// RenderRoutes renders routes_gen.go with blocks, in the order given,
// formatted and with its checksum.
func RenderRoutes(app RoutesApp, blocks []RouteBlock) ([]byte, error) {
	var body strings.Builder
	for _, block := range blocks {
		if !markerLine.MatchString("\t// forge:" + block.ID()) {
			return nil, fmt.Errorf("invalid routes block %q", block.ID())
		}
		fmt.Fprintf(&body, "\t// forge:%s\n%s", block.ID(), block.Body)
	}
	code := fmt.Sprintf("func %s(%s) error {\n%s\treturn nil\n}\n", RoutesFunction, RoutesParameters(app), body.String())
	imports, err := routesImports(app, code)
	if err != nil {
		return nil, err
	}
	var source bytes.Buffer
	source.WriteString(routesHeader)
	source.WriteString(checksumPrefix + checksumPlaceholder + "\n\npackage bootstrap\n\n")
	if len(imports) > 0 {
		source.WriteString("import (\n")
		for _, line := range imports {
			if line != "" {
				line = "\t" + line
			}
			source.WriteString(line + "\n")
		}
		source.WriteString(")\n\n")
	}
	source.WriteString(routesDoc)
	source.WriteString(code)
	formatted, err := format.Source(source.Bytes())
	if err != nil {
		return nil, fmt.Errorf("format %s: %w\n%s", RoutesFile, err, source.String())
	}
	sum := sha256.Sum256(formatted)
	return bytes.Replace(formatted, []byte(checksumPrefix+checksumPlaceholder), []byte(checksumPrefix+hex.EncodeToString(sum[:])), 1), nil
}

const routesHeader = `// Routes the generators add. ` + "`forge generate scaffold`, `forge generate\n// resource` and `forge generate controller` write this file and `forge\n// destroy`" + ` takes their blocks back out; each block starts with a
// "forge:" comment. Do not edit it: the generators refuse to change it once
// its checksum no longer matches. Change bootstrap.go instead. To replace a
// guard (protect, protectCookie, protectPlatformWrite, public), declare a
// function of that name in another file of package bootstrap: the generators
// then leave it out of this file.
//
`

const routesDoc = `// generatedRoutes mounts what the generators added. bootstrap.go calls it.
`

// routesImports lists the import lines code needs, sorted by path.
func routesImports(app RoutesApp, code string) ([]string, error) {
	file, err := parser.ParseFile(token.NewFileSet(), RoutesFile, "package bootstrap\n\n"+code, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("generated %s does not parse: %w\n%s", RoutesFile, err, code)
	}
	packages := routesPackages(app.Module, app.Adapter)
	locals := localNames(file)
	used := make(map[string]bool)
	ast.Inspect(file, func(node ast.Node) bool {
		if selector, ok := node.(*ast.SelectorExpr); ok {
			if ident, ok := selector.X.(*ast.Ident); ok && !locals[ident.Name] {
				if _, known := packages[ident.Name]; known {
					used[ident.Name] = true
				}
			}
		}
		return true
	})
	var lines []string
	for name := range used {
		line := strconv.Quote(packages[name])
		if routesAliases[name] {
			line = name + " " + line
		}
		lines = append(lines, line)
	}
	// The standard library first, then a blank line and the rest, as
	// goimports groups them.
	standard := func(line string) bool {
		path := strings.Trim(importPath(line), `"`)
		first, _, _ := strings.Cut(path, "/")
		return !strings.Contains(first, ".") && path != app.Module && !strings.HasPrefix(path, app.Module+"/")
	}
	slices.SortFunc(lines, func(a, b string) int {
		if standard(a) != standard(b) {
			if standard(a) {
				return -1
			}
			return 1
		}
		return strings.Compare(importPath(a), importPath(b))
	})
	if split := slices.IndexFunc(lines, func(line string) bool { return !standard(line) }); split > 0 {
		lines = slices.Insert(lines, split, "")
	}
	return lines, nil
}

func importPath(line string) string {
	if _, quoted, ok := strings.Cut(line, " "); ok {
		return quoted
	}
	return line
}

// localNames are the parameters and variables code declares: a selector on
// one of them (streams.Broker) is not a package.
func localNames(file *ast.File) map[string]bool {
	names := make(map[string]bool)
	ast.Inspect(file, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.Field:
			for _, name := range node.Names {
				names[name.Name] = true
			}
		case *ast.AssignStmt:
			if node.Tok == token.DEFINE {
				for _, left := range node.Lhs {
					if ident, ok := left.(*ast.Ident); ok {
						names[ident.Name] = true
					}
				}
			}
		}
		return true
	})
	return names
}

// ParseRoutes verifies the checksum of routes_gen.go and returns its blocks.
// It returns ErrRoutesEdited when the file is not exactly what a generator
// wrote.
func ParseRoutes(source []byte) ([]RouteBlock, error) {
	text := string(source)
	start := strings.Index(text, "\n"+checksumPrefix)
	if start < 0 {
		return nil, ErrRoutesEdited
	}
	start += 1 + len(checksumPrefix)
	end := strings.IndexByte(text[start:], '\n')
	if end < 0 {
		return nil, ErrRoutesEdited
	}
	recorded := text[start : start+end]
	sum := sha256.Sum256([]byte(text[:start] + checksumPlaceholder + text[start+end:]))
	if len(recorded) != len(checksumPlaceholder) || recorded != hex.EncodeToString(sum[:]) {
		return nil, ErrRoutesEdited
	}
	lines := strings.SplitAfter(text, "\n")
	opening := slices.IndexFunc(lines, func(line string) bool { return strings.HasPrefix(line, "func "+RoutesFunction+"(") })
	if opening < 0 {
		return nil, ErrRoutesEdited
	}
	var blocks []RouteBlock
	for _, line := range lines[opening+1:] {
		if line == "\treturn nil\n" {
			return blocks, nil
		}
		if match := markerLine.FindStringSubmatch(strings.TrimSuffix(line, "\n")); match != nil {
			blocks = append(blocks, RouteBlock{Kind: match[1], Name: match[2]})
			continue
		}
		if len(blocks) == 0 {
			return nil, ErrRoutesEdited
		}
		blocks[len(blocks)-1].Body += line
	}
	return nil, ErrRoutesEdited
}

// routesFile renders the empty seam `forge new` generates.
func routesFile(data templateData) ([]byte, error) {
	return RenderRoutes(routesAppOf(data), nil)
}

func routesAppOf(data templateData) RoutesApp {
	app := RoutesApp{Module: data.Module, API: data.API}
	if data.DB != nil {
		app.Adapter, app.Identity = data.DB.Adapter, data.DB.Identity
	}
	return app
}
