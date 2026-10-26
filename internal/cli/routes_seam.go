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

// seamReservedNames are the names of the seam's guards, shared blocks and
// parameters, in snake_case as a resource, plural or controller would spell
// them: generated code named like them would be confusing to read next to
// them, so the generators refuse them.
var seamReservedNames = []string{
	"protect", "public", "protect_cookie", "protect_platform_write", "renderer", "signer", "stream_signer",
	"streams", "generated_routes", "repository", "middleware",
}

// rejectSeamReservedName refuses a name the routes seam uses.
func rejectSeamReservedName(kind, name string) error {
	if slices.Contains(seamReservedNames, name) {
		return fmt.Errorf("%s %q is reserved: app/bootstrap/routes_gen.go uses the names %s; choose another name", kind, name, strings.Join(seamReservedNames, ", "))
	}
	return nil
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
	// A temporary file left by an interrupted write would make the write
	// fail after the generator's files exist: refuse before anything.
	if _, err := projectRoot.Lstat(routesTemporary); err == nil {
		return nil, "", fmt.Errorf("%s exists: an earlier write of the routes seam was interrupted. Check %s, then remove %s and run the command again", filepath.ToSlash(routesTemporary), filepath.ToSlash(scaffold.RoutesFile), filepath.ToSlash(routesTemporary))
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, "", err
	}
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
	if !change.remove && current != nil && string(rendered) == string(current) {
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
		name := match[1] + match[2]
		uses := regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\b`)
		for _, block := range remaining {
			if uses.MatchString(block.Body) {
				return fmt.Errorf("refusing to destroy: the %s block of %s uses %s, which the %s block declares; destroy %s first", block.ID(), filepath.ToSlash(scaffold.RoutesFile), name, removed.ID(), block.ID())
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
	return writeRootFileAtomic(projectRoot, scaffold.RoutesFile, content)
}

// routesTemporary is the file the seam is written to before it is renamed
// over routes_gen.go.
var routesTemporary = scaffold.RoutesFile + ".tmp"

// writeRootFileAtomic replaces path with content through a temporary file in
// the same directory, synced, renamed over it, and the directory synced, so a
// crash or a full disk never leaves a half-written seam. The file keeps its
// permissions (0600, as every generated file, when it is new).
func writeRootFileAtomic(root *os.Root, path string, content []byte) error {
	mode := os.FileMode(0o600)
	if info, err := root.Lstat(path); err == nil {
		mode = info.Mode().Perm()
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	temporary := path + ".tmp"
	file, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("write %s: %w (remove %s if no generator is running)", path, err, temporary)
	}
	_, writeErr := file.Write(content)
	chmodErr := file.Chmod(mode) // the umask may have narrowed it
	syncErr := file.Sync()
	if err := errors.Join(writeErr, chmodErr, syncErr, file.Close()); err != nil {
		return errors.Join(err, root.Remove(temporary))
	}
	if err := root.Rename(temporary, path); err != nil {
		return errors.Join(err, root.Remove(temporary))
	}
	directory, err := root.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

// generateLock is the file that keeps two generators (or destroys) from
// running in one application at once: both would read the seam, each add
// its block, and the second write would drop the first's.
var generateLock = filepath.Join(".forge", "generate.lock")

// errGenerateLocked refuses a generator while another one holds the lock.
var errGenerateLocked = fmt.Errorf("another forge generate or forge destroy is running in this application (%s exists): wait for it to finish, or remove the file if none is running", filepath.ToSlash(generateLock))

// withGenerateLock runs fn holding the application's generate lock: the
// file is created exclusively in .forge (made if missing, never removed:
// another run may be using it) and removed afterwards. Anything that keeps
// the lock from being taken refuses the run rather than running unlocked:
// an existing lock, a .forge that is not a plain directory (a symlink, a
// file), a go.mod that is not a regular file, or any other error. A
// directory without go.mod is not an application, and fn reports that
// itself.
func withGenerateLock(root string, fn func() error) error {
	projectRoot, err := os.OpenRoot(root)
	if err != nil {
		return fn()
	}
	defer func() { _ = projectRoot.Close() }()
	switch info, err := projectRoot.Lstat("go.mod"); {
	case errors.Is(err, fs.ErrNotExist):
		return fn()
	case err != nil:
		return fmt.Errorf("take the generate lock: %w", err)
	case !info.Mode().IsRegular():
		return errors.New("refusing to run: go.mod is not a regular file (a symlink or a directory), so the generate lock cannot be taken safely")
	}
	directory := filepath.Dir(generateLock)
	info, err := projectRoot.Lstat(directory)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := projectRoot.MkdirAll(directory, 0o750); err != nil {
			return fmt.Errorf("take the generate lock: %w", err)
		}
	case err != nil:
		return fmt.Errorf("take the generate lock: %w", err)
	case !info.IsDir():
		return fmt.Errorf("refusing to run: %s is not a directory (a symlink or a file), so the generate lock cannot be taken safely", directory)
	}
	lock, err := projectRoot.OpenFile(generateLock, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return errGenerateLocked
	}
	if err != nil {
		return fmt.Errorf("take the generate lock: %w", err)
	}
	if err := lock.Close(); err != nil {
		return errors.Join(fmt.Errorf("take the generate lock: %w", err), projectRoot.Remove(generateLock))
	}
	defer func() { _ = projectRoot.Remove(generateLock) }()
	return fn()
}

// sharedDeclaration finds the variable a shared block declares.
var sharedDeclaration = regexp.MustCompile(`(?m)^(?:\t(\w+)(?:, err)? := |func (\w+)\()`)

// sharedFromParts lists the shared blocks a generator's wiring defines, by
// the ID of the variable each declares.
func sharedFromParts(parts resourceWiringParts) map[string]string {
	shared := make(map[string]string)
	for _, body := range []string{parts.Guard, parts.PlatformGuard, parts.CookieGuard, parts.StreamSigner, parts.StreamsRegister} {
		for _, match := range sharedDeclaration.FindAllStringSubmatch(body, -1) {
			for _, block := range sharedBlocks {
				if block.Name == match[1]+match[2] {
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
