package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

const (
	maxInspectGoFiles = 2000
	maxInspectFile    = 1 << 20
)

var errInspectIncomplete = errors.New("project inspection is incomplete")

type projectInspection struct {
	SchemaVersion    int                `json:"schema_version"`
	Kind             string             `json:"kind"`
	Complete         bool               `json:"complete"`
	Module           string             `json:"module,omitempty"`
	GoVersion        string             `json:"go_version,omitempty"`
	FrameworkVersion string             `json:"framework_version,omitempty"`
	Database         string             `json:"database_adapter"`
	Commands         []string           `json:"commands"`
	Migrations       []string           `json:"migrations"`
	Routes           []string           `json:"static_routes"`
	RouteScope       string             `json:"route_scope"`
	Modules          []string           `json:"forge_modules"`
	Diagnostics      []doctorDiagnostic `json:"diagnostics"`
}

func runInspect(args []string, output io.Writer) error {
	return runInspectAt(args, output, ".", os.Environ())
}

func runInspectAt(args []string, output io.Writer, root string, baseEnv []string) error {
	if len(args) != 1 || args[0] != "--json" {
		return usageError{"usage: forge inspect --json"}
	}
	report := inspectProject(root, baseEnv)
	if err := json.NewEncoder(output).Encode(report); err != nil {
		return err
	}
	if !report.Complete {
		return errInspectIncomplete
	}
	return nil
}

func inspectProject(rootPath string, baseEnv []string) projectInspection {
	report := projectInspection{
		SchemaVersion: 1,
		Kind:          "forge.application",
		Complete:      true,
		Database:      "unknown",
		Commands:      []string{},
		Migrations:    []string{},
		Routes:        []string{},
		RouteScope:    routeScope,
		Modules:       []string{},
		Diagnostics:   []doctorDiagnostic{},
	}

	root, err := os.OpenRoot(rootPath)
	if err != nil {
		addInspectDiagnostic(&report, "project.unreadable", "Project directory could not be inspected.", "Run forge inspect from a readable application directory.")
		return report
	}
	defer func() { _ = root.Close() }()

	goModFile, err := root.Open("go.mod")
	if err != nil {
		addInspectDiagnostic(&report, "project.go_mod_missing", "go.mod could not be read.", "Run forge inspect from the application root containing go.mod.")
		return report
	}
	goMod, readErr := io.ReadAll(io.LimitReader(goModFile, maxInspectFile+1))
	closeErr := goModFile.Close()
	if readErr != nil || closeErr != nil {
		addInspectDiagnostic(&report, "project.go_mod_unreadable", "go.mod could not be read.", "Check permissions and file health for go.mod.")
		return report
	}
	if len(goMod) > maxInspectFile {
		addInspectDiagnostic(&report, "project.go_mod_too_large", "go.mod exceeds the inspection size limit.", "Keep go.mod below 1 MiB and run forge inspect again.")
		return report
	}
	var imports = map[string]struct{}{}
	routeSet := map[string]struct{}{}
	report.Module, report.GoVersion, report.FrameworkVersion = inspectGoMod(goMod)
	if report.Module == "" {
		addInspectDiagnostic(&report, "project.go_mod_invalid", "go.mod has no module declaration.", "Add a valid module directive and run forge inspect again.")
	}
	inspectCommands(root, &report)
	inspectMigrations(root, &report)
	inspectEnvironment(rootPath, baseEnv, &report)
	fileCount := 0
	for _, directory := range []string{"app", "cmd"} {
		inspectSourceTree(root, directory, &fileCount, &report, func(name string, fileSet *token.FileSet, file *ast.File) {
			inspectImports(file, imports)
			if strings.HasSuffix(name, "_test.go") {
				// Tests mount throwaway routes on test servers; they are not
				// the application's routes.
				return
			}
			for _, route := range discoverRoutes(fileSet, file) {
				routeSet[route.pattern] = struct{}{}
			}
		})
	}
	for imported := range imports {
		report.Modules = append(report.Modules, imported)
	}
	for route := range routeSet {
		report.Routes = append(report.Routes, route)
	}
	slices.Sort(report.Modules)
	slices.Sort(report.Routes)
	if _, found := imports["core"]; !found && !strings.Contains(string(goMod), "github.com/rocketinsidecorp/forge-go") {
		addInspectDiagnostic(&report, "project.not_forge", "No Forge framework import was found.", "Run forge inspect inside a Forge application.")
	}
	return report
}

func inspectGoMod(content []byte) (module, goVersion, frameworkVersion string) {
	scanner := bufio.NewScanner(strings.NewReader(string(content)))
	scanner.Buffer(make([]byte, 1024), maxInspectFile)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "module":
			module = fields[1]
		case "go":
			goVersion = fields[1]
		}
		if len(fields) >= 2 && fields[0] == "github.com/rocketinsidecorp/forge-go" && fields[1] != "=>" {
			frameworkVersion = fields[1]
		}
		if len(fields) == 3 && fields[0] == "require" && fields[1] == "github.com/rocketinsidecorp/forge-go" {
			frameworkVersion = fields[2]
		}
	}
	return module, goVersion, frameworkVersion
}

func inspectCommands(root *os.Root, report *projectInspection) {
	entries, err := fs.ReadDir(root.FS(), "cmd")
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		addInspectDiagnostic(report, "project.commands_unreadable", "Command directory could not be inspected.", "Check permissions on cmd/.")
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if info, err := fs.Stat(root.FS(), filepath.Join("cmd", entry.Name(), "main.go")); err == nil && !info.IsDir() {
			report.Commands = append(report.Commands, entry.Name())
		}
	}
}

func inspectMigrations(root *os.Root, report *projectInspection) {
	entries, err := fs.ReadDir(root.FS(), filepath.Join("db", "migrations"))
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		addInspectDiagnostic(report, "project.migrations_unreadable", "Migration directory could not be inspected.", "Check permissions on db/migrations/.")
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() && strings.HasSuffix(name, ".up.sql") {
			report.Migrations = append(report.Migrations, name)
		}
	}
}

func inspectEnvironment(root string, baseEnv []string, report *projectInspection) {
	adapter, err := databaseAdapter(root, baseEnv)
	report.Database = adapter
	if err != nil {
		addInspectDiagnostic(report, "environment_file.invalid", "Development environment files could not be loaded.", "Check .env.development and .env.local syntax; values are omitted from this report.")
	}
}

// databaseAdapter reports which adapter FORGE_DATABASE_URL selects for the
// application in root: postgres, mysql (MySQL and MariaDB), sqlite, none when
// no URL is configured, or unknown. The URL itself is never returned.
func databaseAdapter(root string, baseEnv []string) (string, error) {
	env, err := environment(root, baseEnv)
	if err != nil {
		return "unknown", err
	}
	value, configured := lookupIn(env)("FORGE_DATABASE_URL")
	if !configured || value == "" {
		return "none", nil
	}
	scheme, _, ok := strings.Cut(value, ":")
	if !ok {
		return "unknown", nil
	}
	switch strings.ToLower(scheme) {
	case "postgres", "postgresql":
		return "postgres", nil
	case "mysql":
		return "mysql", nil
	case "sqlite":
		return "sqlite", nil
	default:
		return "unknown", nil
	}
}

// sourceVisitor receives every Go file inspectSourceTree parses.
type sourceVisitor func(name string, fileSet *token.FileSet, file *ast.File)

func inspectSourceTree(root *os.Root, directory string, fileCount *int, report *projectInspection, visit sourceVisitor) {
	if _, err := root.Stat(directory); errors.Is(err, fs.ErrNotExist) {
		return
	} else if err != nil {
		addInspectDiagnostic(report, "project.source_unreadable", "Application source could not be inspected.", "Check permissions on app/ and cmd/.")
		return
	}
	projectFS := root.FS()
	err := fs.WalkDir(projectFS, directory, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			addInspectDiagnostic(report, "project.source_unreadable", "Application source could not be inspected.", "Check permissions on app/ and cmd/.")
			return nil //nolint:nilerr // the failure is reported as a diagnostic and the scan continues; inspect never aborts
		}
		if entry.IsDir() || !strings.HasSuffix(name, ".go") {
			return nil
		}
		*fileCount++
		if *fileCount > maxInspectGoFiles {
			addInspectDiagnostic(report, "project.scan_limit", "Source scan reached the file limit; the inventory is partial.", "Inspect smaller source roots or use the source index generated by your editor.")
			return fs.SkipAll
		}
		info, err := entry.Info()
		if err != nil || info.Size() > maxInspectFile {
			addInspectDiagnostic(report, "project.scan_limit", "A source file could not be read within the inspection limits.", "Keep individual Go source files below 1 MiB.")
			return nil //nolint:nilerr // the failure is reported as a diagnostic and the scan continues; inspect never aborts
		}
		source, err := fs.ReadFile(projectFS, name)
		if err != nil {
			addInspectDiagnostic(report, "project.source_unreadable", "Application source could not be inspected.", "Check permissions on app/ and cmd/.")
			return nil //nolint:nilerr // the failure is reported as a diagnostic and the scan continues; inspect never aborts
		}
		fileSet := token.NewFileSet()
		file, err := parser.ParseFile(fileSet, name, source, parser.ParseComments)
		if err != nil {
			addInspectDiagnostic(report, "project.source_parse_error", "Some Go files could not be parsed; the inventory is partial.", "Fix source syntax errors and run forge inspect again.")
			return nil //nolint:nilerr // the failure is reported as a diagnostic and the scan continues; inspect never aborts
		}
		visit(name, fileSet, file)
		return nil
	})
	if err != nil {
		addInspectDiagnostic(report, "project.source_unreadable", "Application source could not be inspected.", "Check permissions on app/ and cmd/.")
	}
}

func inspectImports(file *ast.File, modules map[string]struct{}) {
	const prefix = "github.com/rocketinsidecorp/forge-go"
	for _, imported := range file.Imports {
		value, err := strconv.Unquote(imported.Path.Value)
		if err != nil {
			continue
		}
		if value == prefix {
			modules["core"] = struct{}{}
			continue
		}
		if !strings.HasPrefix(value, prefix+"/") {
			continue
		}
		rest := strings.TrimPrefix(value, prefix+"/")
		module, _, _ := strings.Cut(rest, "/")
		modules[module] = struct{}{}
	}
}

func addInspectDiagnostic(report *projectInspection, code, summary, remediation string) {
	report.Complete = false
	for _, current := range report.Diagnostics {
		if current.Code == code {
			return
		}
	}
	report.Diagnostics = append(report.Diagnostics, doctorDiagnostic{
		Code:        code,
		Severity:    "warning",
		Summary:     summary,
		Remediation: remediation,
	})
}
