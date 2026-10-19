package cli

import (
	"cmp"
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/rocketinsidecorp/forge-go/router"
)

// routeScope describes what static route discovery can see; forge inspect
// reports it and forge routes prints it.
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

func runRoutes(args []string, output io.Writer) error {
	return runRoutesAt(args, output, ".")
}

type routeRow struct {
	method, path, action, source string
}

// runRoutesAt prints the application's routes as a table. Like forge
// inspect it reads source files only and never runs application code.
func runRoutesAt(args []string, output io.Writer, rootPath string) error {
	if len(args) != 0 {
		return errors.New("usage: forge routes")
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return errors.New("go.mod not found: run this inside a Forge application")
	}
	defer func() { _ = root.Close() }()
	if _, err := root.Stat("go.mod"); err != nil {
		return errors.New("go.mod not found: run this inside a Forge application")
	}
	report := projectInspection{Complete: true}
	var rows []routeRow
	fileCount := 0
	for _, directory := range []string{"app", "cmd"} {
		inspectSourceTree(root, directory, &fileCount, &report, func(name string, fileSet *token.FileSet, file *ast.File) {
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
