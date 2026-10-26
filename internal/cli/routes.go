package cli

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/rocketinsidecorp/forge-go/router"
)

// routeScope describes what static route discovery can see; forge inspect
// reports it and forge routes prints it when it falls back to the source.
const routeScope = "literal first arguments to Handle and HandleFunc calls, and the conventional routes of Resources calls with a literal path, in app/ and cmd/; dynamically composed routes are not included"

// builtInRoutes are registered by forge.New in every application.
var builtInRoutes = []string{"GET /health", "GET /health/live", "GET /health/ready"}

type discoveredRoute struct {
	// pattern is the ServeMux pattern as written, such as "GET /posts/{id}".
	pattern string
	// action is the resource action for a route a Resources call expands to.
	action router.Action
	line   int
}

// discoverRoutes finds Handle and HandleFunc calls whose first argument is a
// string literal, and Resources calls whose path is a string literal. A
// Resources call is expanded to every conventional route: which actions the
// handler implements is only known when the application runs.
func discoverRoutes(fileSet *token.FileSet, file *ast.File) []discoveredRoute {
	var routes []discoveredRoute
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		literal, ok := call.Args[0].(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(literal.Value)
		if err != nil || strings.TrimSpace(value) == "" {
			return true
		}
		line := 0
		if fileSet != nil {
			line = fileSet.Position(call.Pos()).Line
		}
		switch selector.Sel.Name {
		case "Handle", "HandleFunc":
			routes = append(routes, discoveredRoute{pattern: value, line: line})
		case "Resources":
			expanded, err := router.ResourceRoutes(value)
			if err != nil || len(call.Args) < 2 {
				return true
			}
			for _, route := range expanded {
				routes = append(routes, discoveredRoute{pattern: route.Method + " " + route.Pattern, action: route.Action, line: line})
			}
		}
		return true
	})
	return routes
}

func runRoutes(args []string, output, stderr io.Writer) error {
	return runRoutesAt(args, output, stderr, ".")
}

type routeRow struct {
	method, path, action, source string
}

// runRoutesAt prints the application's routes as a table. It builds the
// application and runs its routes command, which composes the application
// without serving it, so it lists every route that is registered, including
// those built from constants, loops and helper functions such as the
// /v1/<plural> routes of generated resources. Source scanning cannot see
// those: their patterns are values computed while the application runs. Only
// when the application cannot be run (no cmd/, an application built with a
// framework version that predates the routes command, a Configure that
// fails) does it fall back to scanning the source, say why, and print what the
// scan can find. That explanation goes to stderr, so stdout carries only the
// table and a pipe such as `forge routes | grep /v1` stays clean.
//
// An optional NAME picks the cmd/NAME binary when the application has several.
func runRoutesAt(args []string, output, stderr io.Writer, rootPath string) error {
	if len(args) > 1 || (len(args) == 1 && strings.HasPrefix(args[0], "-")) {
		return usageError{routesUsage}
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return errors.New("go.mod not found: run this inside a Forge application")
	}
	defer func() { _ = root.Close() }()
	if _, err := root.Stat("go.mod"); err != nil {
		return errors.New("go.mod not found: run this inside a Forge application")
	}
	table, runErr := applicationRoutes(rootPath, args)
	if runErr == nil {
		_, err := output.Write(table)
		return err
	}
	if len(args) == 1 {
		// The user named a binary: not finding it is an error, not a reason
		// to list something else.
		var missing *commandNotFoundError
		if errors.As(runErr, &missing) {
			return missing.err
		}
	}
	return scanRoutes(root, output, stderr, runErr)
}

const routesUsage = "usage: forge routes [NAME]"

// commandNotFoundError marks a failure to choose the application binary.
type commandNotFoundError struct{ err error }

func (e *commandNotFoundError) Error() string { return e.err.Error() }

// applicationRoutes runs the application's routes command and returns its
// output.
func applicationRoutes(rootPath string, nameArgs []string) ([]byte, error) {
	name, err := resolveCommand(rootPath, nameArgs)
	if err != nil {
		return nil, &commandNotFoundError{err}
	}
	env, err := developmentEnvironment(rootPath)
	if err != nil {
		return nil, err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var stdout, stderr bytes.Buffer
	if err := runAppWithInput(ctx, rootPath, name, []string{"routes"}, env, nil, &stdout, &stderr); err != nil {
		return nil, fmt.Errorf("%w: %s", err, firstLine(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// firstLine is the first line of text that says something: not empty and not
// the "# package" header go build prints before its errors.
func firstLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			return line
		}
	}
	return "no output"
}

// scanRoutes prints the routes found by scanning the source, after explaining
// why the application itself could not list them on stderr.
func scanRoutes(root *os.Root, output, stderr io.Writer, runErr error) error {
	fmt.Fprintf(stderr, "note: the application could not list its routes (%v), so the table comes from a scan of the source. It misses routes whose paths are built from constants or loops, such as the /v1/<plural> routes of generated resources. Fix that error, or build the application with a current Forge, to list every route.\n", runErr)
	report := projectInspection{Complete: true}
	var rows []routeRow
	fileCount := 0
	for _, directory := range []string{"app", "cmd"} {
		inspectSourceTree(root, directory, &fileCount, &report, func(name string, fileSet *token.FileSet, file *ast.File) {
			if strings.HasSuffix(name, "_test.go") {
				// Tests mount throwaway routes on test servers; they are not
				// the application's routes.
				return
			}
			for _, route := range discoverRoutes(fileSet, file) {
				method, path := splitRoutePattern(route.pattern)
				rows = append(rows, routeRow{method: method, path: path, action: string(route.action), source: fmt.Sprintf("%s:%d", filepath.ToSlash(name), route.line)})
			}
		})
	}
	for _, pattern := range builtInRoutes {
		method, path := splitRoutePattern(pattern)
		rows = append(rows, routeRow{method: method, path: path, source: "forge (built in)"})
	}
	rows = append(rows, routeRow{method: http.MethodGet, path: "/openapi.json", source: "forge (built in; development and test, or FORGE_OPENAPI_ENABLED=true)"})
	slices.SortStableFunc(rows, func(a, b routeRow) int {
		return cmp.Or(strings.Compare(a.path, b.path), strings.Compare(a.method, b.method), strings.Compare(a.source, b.source))
	})
	rows = slices.Compact(rows)

	table := tabwriter.NewWriter(output, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "METHOD\tPATH\tACTION\tSOURCE")
	for _, row := range rows {
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\n", row.method, row.path, cmp.Or(row.action, "-"), row.source)
	}
	if err := table.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(output, "\nScope: %s.\n", routeScope)
	fmt.Fprintln(output, "Resources routes are listed for every action; only the actions the handler implements are registered.")
	for _, diagnostic := range report.Diagnostics {
		fmt.Fprintf(output, "warning: %s %s\n", diagnostic.Summary, diagnostic.Remediation)
	}
	return nil
}

// splitRoutePattern separates "GET /posts" into its method and path; a
// pattern without a method matches any method.
func splitRoutePattern(pattern string) (string, string) {
	fields := strings.Fields(pattern)
	if len(fields) == 2 {
		return fields[0], fields[1]
	}
	return "ANY", strings.TrimSpace(pattern)
}
