package cli

import (
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/rocketinsidecorp/forge-go/internal/scaffold"
)

// The generators wire their routes into app/bootstrap/routes_gen.go, the
// seam scaffold.RenderRoutes renders, instead of printing code to paste.
// bootstrap.go (the application owner's code) calls the seam once; the
// generators never edit it. This file decides what the seam holds: the
// generators' own blocks in the order they were generated, and before them
// the shared blocks those need (guards, the view renderer, live updates),
// each defined once and dropped when nothing uses it any more.

// routesSeam describes the seam of the application in projectRoot: its
// signature, and whether bootstrap.go calls it. An application generated
// before the seam existed does not, and its generators print wiring.
type routesSeam struct {
	app   scaffold.RoutesApp
	wired bool
	// overridden are the shared names (guards) the application declares
	// itself in another file of package bootstrap: the seam leaves them out
	// and uses the application's.
	overridden map[string]bool
}

// openRoutesSeam inspects the application. adapter is what databaseAdapter
// reports ("none" without a database).
func openRoutesSeam(projectRoot *os.Root, module, adapter string, identity bool) routesSeam {
	seam := routesSeam{app: scaffold.RoutesApp{Module: module, API: apiOnlyApplication(projectRoot), Identity: identity}}
	if adapter != "none" {
		seam.app.Adapter = adapter
	}
	source, err := projectRoot.ReadFile(filepath.Join("app", "bootstrap", "bootstrap.go"))
	seam.wired = err == nil && strings.Contains(string(source), scaffold.RoutesFunction+"(")
	seam.overridden = bootstrapDeclarations(projectRoot)
	return seam
}

// bootstrapDeclarations are the package-level names the files of package
// bootstrap other than the seam declare.
func bootstrapDeclarations(projectRoot *os.Root) map[string]bool {
	names := make(map[string]bool)
	dir := filepath.Join("app", "bootstrap")
	entries, err := fs.ReadDir(projectRoot.FS(), dir)
	if err != nil {
		return names
	}
	fset := token.NewFileSet()
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") || path == scaffold.RoutesFile {
			continue
		}
		source, err := fs.ReadFile(projectRoot.FS(), path)
		if err != nil {
			continue
		}
		parsed, err := parser.ParseFile(fset, path, source, parser.SkipObjectResolution)
		if err != nil {
			continue
		}
		for _, name := range topLevelNames(parsed) {
			names[name] = true
		}
	}
	return names
}

// routesChange is what one generator adds to the seam, or takes out of it.
type routesChange struct {
	// entry is the generator's own block. Only its ID matters for a removal.
	entry scaffold.RouteBlock
	// shared are the shared blocks the entry may need, by ID, with the body
	// to define them with when the seam does not have them yet.
	shared map[string]string
	remove bool
}

// sharedBlocks are the shared blocks in the order the seam defines them: the
// signer before the renderer that signs with it, the guards, then the
// stream the pages publish to.
var sharedBlocks = []scaffold.RouteBlock{
	{Kind: "signer", Name: "streamSigner"},
	{Kind: "renderer", Name: "renderer"},
	{Kind: "guard", Name: "protect"},
	{Kind: "guard", Name: "public"},
	{Kind: "guard", Name: "protectPlatformWrite"},
	{Kind: "guard", Name: "protectCookie"},
	{Kind: "streams", Name: "streams"},
}

const (
	plainRenderer = `	renderer, err := views.New(views.WithI18n(translator))
	if err != nil {
		return err
	}
`
	streamsRenderer = `	// The pages sign the names of the streams they subscribe to.
	renderer, err := views.New(views.WithI18n(translator), views.WithStreams(streamSigner))
	if err != nil {
		return err
	}
`
)

// planRoutes returns routes_gen.go with change applied, and the verb to report:
// "wire" or "unwire" when the file changes, "identical" when it already says
// the same, "" when a removal finds nothing to remove. It refuses a seam that
// was edited by hand, and an entry the seam already wires differently.
func planRoutes(projectRoot *os.Root, seam routesSeam, change routesChange) ([]byte, string, error) {
	current, err := projectRoot.ReadFile(scaffold.RoutesFile)
	var blocks []scaffold.RouteBlock
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, "", err
	default:
		if blocks, err = scaffold.ParseRoutes(current); err != nil {
			return nil, "", err
		}
	}
	if err := rejectExistingSymlinkPaths(projectRoot, []string{scaffold.RoutesFile}); err != nil {
		return nil, "", err
	}
	shared := make(map[string]string)
	var entries []scaffold.RouteBlock
	for _, block := range blocks {
		if block.Shared() {
			shared[block.ID()] = block.Body
		} else {
			entries = append(entries, block)
		}
	}
	found := slices.IndexFunc(entries, func(block scaffold.RouteBlock) bool { return block.ID() == change.entry.ID() })
	verb := "wire"
	switch {
	case change.remove && found < 0:
		return current, "", nil
	case change.remove:
		removed := entries[found]
		entries = slices.Delete(entries, found, found+1)
		if err := rejectDependents(removed, entries); err != nil {
			return nil, "", err
		}
		verb = "unwire"
	case found >= 0:
		existing, err := renderRoutes(seam, shared, entries, change.shared)
		if err != nil {
			return nil, "", err
		}
		entries[found] = change.entry
		replaced, err := renderRoutes(seam, shared, entries, change.shared)
		if err != nil {
			return nil, "", err
		}
		if string(existing) != string(replaced) {
			return nil, "", fmt.Errorf("refusing to generate: %s already wires the %s with other code (generated with other arguments, or by another Forge version). Run `forge destroy` with the arguments it was generated with first", filepath.ToSlash(scaffold.RoutesFile), change.entry.Kind+" "+change.entry.Name)
		}
	default:
		entries = append(entries, change.entry)
	}
	rendered, err := renderRoutes(seam, shared, entries, change.shared)
	if err != nil {
		return nil, "", err
	}
	if current != nil && string(rendered) == string(current) {
		if change.remove {
			return current, "unwire", nil
		}
		return current, "identical", nil
	}
	return rendered, verb, nil
}

// rejectDependents refuses to take out a block whose variables (the
// productService a resource declares) a remaining block still uses: a later
// block that builds on an earlier one (nested routes, a join between two
// resources) must be destroyed first, or the seam would not compile.
func rejectDependents(removed scaffold.RouteBlock, remaining []scaffold.RouteBlock) error {
	for _, match := range sharedDeclaration.FindAllStringSubmatch(removed.Body, -1) {
		uses := regexp.MustCompile(`\b` + regexp.QuoteMeta(match[1]) + `\b`)
		for _, block := range remaining {
			if uses.MatchString(block.Body) {
				return fmt.Errorf("refusing to destroy: the %s block of %s uses %s, which the %s block declares; destroy %s first", block.ID(), filepath.ToSlash(scaffold.RoutesFile), match[1], removed.ID(), block.ID())
			}
		}
	}
	return nil
}

// renderRoutes renders the seam with entries and the shared blocks they
// need: existing ones as they are, missing ones from offered.
func renderRoutes(seam routesSeam, existing map[string]string, entries []scaffold.RouteBlock, offered map[string]string) ([]byte, error) {
	var text strings.Builder
	for _, entry := range entries {
		text.WriteString(entry.Body)
	}
	used := text.String()
	needs := func(name string) bool {
		return regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\b`).MatchString(used)
	}
	streams := regexp.MustCompile(`\bstreams\.`).MatchString(used)
	var blocks []scaffold.RouteBlock
	for _, block := range sharedBlocks {
		switch block.Kind {
		case "signer", "streams":
			if !streams {
				continue
			}
		case "renderer":
			if !needs("renderer") {
				continue
			}
			block.Body = plainRenderer
			if streams {
				block.Body = streamsRenderer
			}
			blocks = append(blocks, block)
			continue
		case "guard":
			if !needs(block.Name) || seam.overridden[block.Name] {
				continue
			}
		}
		body, ok := existing[block.ID()]
		if !ok {
			body, ok = offered[block.ID()]
		}
		if !ok || body == "" {
			return nil, fmt.Errorf("%s needs %s, which it does not define: restore the file from version control, or run `forge destroy` and the generator again", filepath.ToSlash(scaffold.RoutesFile), block.Name)
		}
		block.Body = body
		blocks = append(blocks, block)
	}
	return scaffold.RenderRoutes(seam.app, append(blocks, entries...))
}

// writeRoutes writes the seam planned by planRoutes.
func writeRoutes(root string, content []byte) error {
	projectRoot, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer func() { _ = projectRoot.Close() }()
	if err := rejectExistingSymlinkPaths(projectRoot, []string{scaffold.RoutesFile}); err != nil {
		return err
	}
	return projectRoot.WriteFile(scaffold.RoutesFile, content, 0o600)
}

// sharedDeclaration finds the variable a shared block declares.
var sharedDeclaration = regexp.MustCompile(`(?m)^\t(\w+)(?:, err)? := `)

// sharedFromParts lists the shared blocks a generator's wiring defines, by
// the ID of the variable each declares.
func sharedFromParts(parts resourceWiringParts) map[string]string {
	shared := make(map[string]string)
	for _, body := range []string{parts.Guard, parts.PlatformGuard, parts.CookieGuard, parts.StreamSigner, parts.StreamsRegister} {
		for _, match := range sharedDeclaration.FindAllStringSubmatch(body, -1) {
			for _, block := range sharedBlocks {
				if block.Name == match[1] {
					shared[block.ID()] = body
				}
			}
		}
	}
	return shared
}

// seamWiring is the wiring of a generator in an application with the seam:
// the planned seam, and what to tell the developer.
type seamWiring struct {
	content []byte
	verb    string
	notes   []string
}

// reportSeam prints the seam line and the notes of a generator's wiring.
func reportSeam(wiring seamWiring, dryRun bool, then string) string {
	var out strings.Builder
	verb := wiring.verb
	if dryRun && (verb == "wire" || verb == "unwire") {
		verb = "would " + verb
	}
	if verb != "" {
		fmt.Fprintf(&out, "  %s  %s\n", verb, scaffold.RoutesFile)
	}
	for _, note := range wiring.notes {
		fmt.Fprintf(&out, "\n  %s\n", note)
	}
	if then != "" {
		fmt.Fprintf(&out, "\n  Then: %s\n", then)
	}
	return out.String()
}
