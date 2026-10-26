package forge

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/router"
)

// routesScope is printed under the table of the "routes" command and
// describes what it lists.
const routesScope = "every route registered while composing the application, HTML and JSON alike, and Forge's built-in routes; routes added only while serving are not included"

// builtInRoutePaths are the GET routes forge.New registers in every
// application.
var builtInRoutePaths = []string{"/health", liveRoutePath, "/health/ready"}

// describeRoutes composes the application without serving it and prints the
// routes registered on it as a table (METHOD, PATH, ACTION, SOURCE) sorted by
// path, then method. Configure runs as for serve, so it needs the same
// configuration; nothing listens on the network. Unlike a scan of the source it
// sees routes registered through constants, loops and helper functions.
func describeRoutes(ctx context.Context, cfg config.Config, logger *slog.Logger, configure Configure, stdout, stderr io.Writer) int {
	app, release, ok := composeWithoutServing(ctx, cfg, logger, configure, "routes", stderr)
	if !ok {
		return 1
	}
	defer release()
	if err := writeRoutes(stdout, app); err != nil {
		fmt.Fprintf(stderr, "routes: %v\n", err)
		return 1
	}
	return 0
}

func writeRoutes(output io.Writer, app *App) error {
	root, _ := os.Getwd()
	type row struct{ method, path, action, source string }
	var rows []row
	for _, registration := range app.router.Registrations() {
		rows = append(rows, row{
			method: cmp.Or(registration.Method, "ANY"),
			path:   registration.Pattern,
			action: cmp.Or(string(registration.Action), "-"),
			source: routeSource(root, registration),
		})
	}
	if app.servesOpenAPI() {
		// Mounted when the first request arrives, so it is not registered yet.
		rows = append(rows, row{http.MethodGet, OpenAPIPath, "-", "forge (built in; development and test, or FORGE_OPENAPI_ENABLED=true)"})
	}
	slices.SortStableFunc(rows, func(a, b row) int {
		return cmp.Or(strings.Compare(a.path, b.path), strings.Compare(a.method, b.method))
	})
	table := tabwriter.NewWriter(output, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "METHOD\tPATH\tACTION\tSOURCE")
	for _, r := range rows {
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\n", r.method, r.path, r.action, r.source)
	}
	if err := table.Flush(); err != nil {
		return err
	}
	_, err := fmt.Fprintf(output, "\nScope: %s.\n", routesScope)
	return err
}

// routeSource is where the route was registered, relative to the working
// directory (the application root) when it is inside it.
func routeSource(root string, registration router.Registration) string {
	// The health routes are registered while forge.New builds the
	// application, so their nearest caller outside the framework is whatever
	// called New; they always belong to Forge, which owns the paths.
	if registration.Source == "" || (registration.Method == http.MethodGet && slices.Contains(builtInRoutePaths, registration.Pattern)) {
		return "forge (built in)"
	}
	file, line := registration.Source, ""
	if i := strings.LastIndex(file, ":"); i >= 0 {
		file, line = file[:i], file[i:]
	}
	if root != "" && filepath.IsAbs(file) {
		if relative, err := filepath.Rel(root, file); err == nil && !strings.HasPrefix(relative, "..") {
			file = relative
		}
	}
	return filepath.ToSlash(file) + line
}
