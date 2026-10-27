package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/rocketinsidecorp/forge-go/internal/scaffold"
)

const destroyUsage = `usage: forge destroy <model|resource|scaffold|join|controller|mailer|job|view> ARGUMENTS... [--force] [--migrations] [--dry-run]
       ARGUMENTS are the ones the generator was run with: forge destroy scaffold post title:string --tenant-scoped`

// runDestroy is `forge destroy`, the counterpart of `forge generate` (Rails'
// `rails destroy`). It regenerates what the generator would create from the
// same arguments and removes exactly those files, and only while each one is
// still what the generator wrote: a file someone edited is never removed
// without --force. Migrations are kept unless --migrations says the database
// no longer has them, because a migration that already ran is history.
func runDestroy(args []string, stdout io.Writer) error {
	return runDestroyEnv(args, stdout, ".", os.Environ(), time.Now())
}

func runDestroyEnv(args []string, stdout io.Writer, root string, env []string, now time.Time) error {
	if len(args) == 0 {
		return usageError{destroyUsage}
	}
	generator := args[0]
	var force, migrations bool
	var rest []string
	for _, arg := range args[1:] {
		switch arg {
		case "--force":
			force = true
		case "--migrations":
			migrations = true
		default:
			rest = append(rest, arg)
		}
	}
	return withGenerateLock(root, func() error {
		plan, err := planDestroy(generator, rest, root, env, now)
		if err != nil {
			return err
		}
		return applyDestroy(stdout, root, plan, force, migrations)
	})
}

// destroyPlan is what a generator would create, found where it is.
type destroyPlan struct {
	files  []generatedFile
	dryRun bool
	// unwire says what to take out of app/bootstrap/bootstrap.go, which
	// neither generating nor destroying edits.
	unwire string
	// routes is the routes seam without the generator's block, when the
	// application has the seam and the block is there (routesVerb is then
	// "unwire"): it is written once the files are removed.
	routes     []byte
	routesVerb string
	// keyDependents, when set, says why the generation's migration may not
	// be removed (--migrations): it declares a unique key that other
	// associations' foreign keys reference.
	keyDependents string
	// rewrites take a scaffold's frame out of the referenced scaffolds'
	// _associations.gohtml.
	rewrites []fileRewrite
	// ownedElsewhere, when set, says why the files belong to a bigger
	// generation (a model inside a resource or scaffold, a view a controller
	// renders): removing them alone would break the application, so the
	// destroy is refused unless --force.
	ownedElsewhere string
}

func planDestroy(generator string, args []string, root string, env []string, now time.Time) (destroyPlan, error) {
	switch generator {
	case "model":
		request, err := parseModelArgs(args)
		if err != nil {
			return destroyPlan{}, err
		}
		request.Destroy = true
		if err := refuseReferenced(root, request); err != nil {
			return destroyPlan{}, err
		}
		spec, files, err := planResource(root, request, env, now)
		if err != nil {
			return destroyPlan{}, err
		}
		keys, err := keyDependentsRefusal(root, spec, false)
		if err != nil {
			return destroyPlan{}, err
		}
		line := fmt.Sprintf("%sService, err := %s", lowerCamel(spec.Name), serviceConstructor(spec))
		owned, err := httpLayerOf(root, "model", spec.Name)
		if err != nil {
			return destroyPlan{}, err
		}
		return destroyPlan{files: files, dryRun: request.DryRun, unwire: "the " + spec.Name + " use cases you built: " + line, ownedElsewhere: owned, keyDependents: keys}, nil
	case "resource", "scaffold":
		usage := resourceUsage
		if generator == "scaffold" {
			usage = scaffoldUsage
		}
		request, err := parseGeneratorArgs(args, usage)
		if err != nil {
			return destroyPlan{}, err
		}
		request.Destroy = true
		if err := refuseReferenced(root, request); err != nil {
			return destroyPlan{}, err
		}
		var extend planExtension
		if generator == "scaffold" {
			request.Scaffold = true
			extend = planScaffoldFiles
		}
		spec, files, err := planResourceWith(root, request, env, now, extend)
		if err != nil {
			return destroyPlan{}, err
		}
		var owned string
		if generator == "resource" {
			if owned, err = httpLayerOf(root, generator, spec.Name); err != nil {
				return destroyPlan{}, err
			}
		}
		keys, err := keyDependentsRefusal(root, spec, false)
		if err != nil {
			return destroyPlan{}, err
		}
		plan := destroyPlan{files: files, dryRun: request.DryRun, unwire: "the " + generator + " wiring:\n" + buildResourceWiring(spec).Usage, ownedElsewhere: owned, rewrites: spec.rewrites, keyDependents: keys}
		if spec.Seam {
			wiring, err := planResourceSeam(root, spec, true)
			if err != nil {
				return destroyPlan{}, err
			}
			plan.routes, plan.routesVerb, plan.unwire = wiring.content, wiring.verb, ""
		}
		return plan, nil
	case "controller":
		request, err := parseControllerArgs(args)
		if err != nil {
			return destroyPlan{}, err
		}
		request.Destroy = true
		spec, files, err := planController(root, request, env, now)
		if err != nil {
			return destroyPlan{}, err
		}
		plan := destroyPlan{files: files, dryRun: request.DryRun, unwire: "the controller wiring:\n" + buildControllerWiring(spec).Usage}
		if spec.Seam {
			wiring, err := planControllerSeam(root, spec, true)
			if err != nil {
				return destroyPlan{}, err
			}
			plan.routes, plan.routesVerb, plan.unwire = wiring.content, wiring.verb, ""
		}
		return plan, nil
	case "mailer":
		request, err := parseMailerArgs(args)
		if err != nil {
			return destroyPlan{}, err
		}
		request.Destroy = true
		files, err := planMailer(root, request)
		if err != nil {
			return destroyPlan{}, err
		}
		return destroyPlan{files: files, dryRun: request.DryRun, unwire: "the mailer wiring:\n" + buildMailerWiring(request, mailerModule(root)).Usage}, nil
	case "job":
		return planDestroyJob(args, root)
	case "view":
		return planDestroyView(args, root)
	case "join":
		return planDestroyJoin(args, root, env, now)
	case "migration":
		return destroyPlan{}, errors.New("forge destroy does not remove migrations: one that ran is part of the database's history. Revert it with `forge rollback` and delete db/migrations/<version>_NAME.{up,down}.sql yourself")
	default:
		return destroyPlan{}, usageError{fmt.Sprintf("unknown generator %q\n%s", generator, destroyUsage)}
	}
}

// refuseReferenced refuses to destroy a resource that other resources or
// joins still reference: their foreign keys need its table, and their code
// its repository. They are destroyed first, migrations included.
func refuseReferenced(root string, request resourceRequest) error {
	projectRoot, err := os.OpenRoot(root)
	if err != nil {
		return errors.New("go.mod not found: run this inside a Forge application")
	}
	defer func() { _ = projectRoot.Close() }()
	found, err := dependents(projectRoot, request.Name)
	if err != nil || len(found) == 0 {
		return err
	}
	return fmt.Errorf("refusing to destroy %s, nothing was removed: %s still reference it (foreign keys to %s, and code that reads it). Destroy them first, with `forge rollback` and --migrations for their migrations, then %s", request.Name, strings.Join(found, ", "), request.Plural, request.Name)
}

// httpLayerOf reports, for `destroy model` or `destroy resource`, the HTTP
// layer a bigger generator wrote around the same name: the JSON adapter of
// `generate resource` (for a model) or the pages of `generate scaffold`. The
// model's files are that generation's too, and removing them alone would
// leave adapters that no longer compile.
func httpLayerOf(root, generator, name string) (string, error) {
	projectRoot, err := os.OpenRoot(root)
	if err != nil {
		return "", errors.New("go.mod not found: run this inside a Forge application")
	}
	defer func() { _ = projectRoot.Close() }()
	candidates := []struct{ path, owner string }{
		{filepath.Join("app", "adapters", "htmlui", name+".go"), "scaffold"},
	}
	if generator == "model" {
		candidates = append(candidates, struct{ path, owner string }{filepath.Join("app", "adapters", "httpapi", name+".go"), "resource"})
	}
	for _, candidate := range candidates {
		_, err := projectRoot.Lstat(candidate.path)
		switch {
		case err == nil:
			return fmt.Sprintf("%[1]s exists, and `forge generate %[2]s %[3]s` writes it around this %[4]s: the %[4]s's files belong to that %[2]s, and removing them alone would leave code that no longer compiles. Run `forge destroy %[2]s` with the arguments it was generated with, or pass --force to remove only the %[4]s's files", filepath.ToSlash(candidate.path), candidate.owner, name, generator), nil
		case !errors.Is(err, fs.ErrNotExist):
			return "", err
		}
	}
	return "", nil
}

func planDestroyJob(args []string, root string) (destroyPlan, error) {
	if slices.Contains(args, "--queue") {
		request, err := parseQueueJobArgs(args)
		if err != nil {
			return destroyPlan{}, err
		}
		request.Destroy = true
		_, files, err := planQueueJob(root, request)
		if err != nil {
			return destroyPlan{}, err
		}
		return destroyPlan{files: files, dryRun: request.DryRun, unwire: fmt.Sprintf("the registration: jobs.Register%s(registry)", exportedName(request.Name))}, nil
	}
	flags, positional, err := splitFlags(args, generateUsage, "--dry-run")
	if err != nil {
		return destroyPlan{}, err
	}
	if len(positional) != 1 {
		return destroyPlan{}, usageError{destroyUsage}
	}
	name := positional[0]
	if len(name) > 64 || !jobName.MatchString(name) {
		return destroyPlan{}, errors.New("job name must be lowercase snake_case with letter-starting segments")
	}
	files, err := renderJobFiles(name)
	if err != nil {
		return destroyPlan{}, err
	}
	return destroyPlan{files: files, dryRun: flags["--dry-run"], unwire: fmt.Sprintf("the schedule: Run%s in bootstrap.JobsConfigure", exportedJobName(name))}, nil
}

func planDestroyView(args []string, root string) (destroyPlan, error) {
	flags, positional, err := splitFlags(args, generateUsage, "--dry-run")
	if err != nil {
		return destroyPlan{}, err
	}
	if len(positional) != 1 {
		return destroyPlan{}, usageError{destroyUsage}
	}
	dir, base, err := splitViewName(positional[0])
	if err != nil {
		return destroyPlan{}, err
	}
	projectRoot, _, err := openApplication(root)
	if err != nil {
		return destroyPlan{}, err
	}
	defer func() { _ = projectRoot.Close() }()
	path := filepath.Join("app", "views", dir, base+".gohtml")
	// A controller's pages are the same stub `generate view` writes, so the
	// content cannot tell them apart: a view the DIR controller renders
	// belongs to `destroy controller`.
	var owned string
	controller := filepath.Join("app", "adapters", "htmlui", dir+"_controller.go")
	source, err := projectRoot.ReadFile(controller)
	switch {
	case err == nil:
		if strings.Contains(string(source), filepath.ToSlash(path)) || strings.Contains(string(source), fmt.Sprintf("%q", base)) {
			owned = fmt.Sprintf("%[1]s is a page of the %[2]s controller (%[3]s renders it), and removing it alone would make that page fail. Run `forge destroy controller %[2]s` with the actions it was generated with, or pass --force to remove only the view", filepath.ToSlash(path), dir, filepath.ToSlash(controller))
		}
	case !errors.Is(err, fs.ErrNotExist):
		return destroyPlan{}, err
	}
	return destroyPlan{
		files:          []generatedFile{{path: path, content: generatedView}},
		dryRun:         flags["--dry-run"],
		unwire:         fmt.Sprintf("the handler that renders %q", dir+"/"+base),
		ownedElsewhere: owned,
	}, nil
}

// directoriesKept are never removed, however empty they become: they are the
// application's own layout, not something one generator made.
var directoriesKept = []string{".", "app", filepath.Join("app", "adapters"), filepath.Join("app", "views"), "db", filepath.Join("db", "migrations"), "cmd"}

type destroyAction struct {
	path   string
	verb   string // remove, keep, skip
	reason string
}

func applyDestroy(stdout io.Writer, rootPath string, plan destroyPlan, force, migrations bool) error {
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return errors.New("go.mod not found: run this inside a Forge application")
	}
	defer func() { _ = root.Close() }()
	if plan.ownedElsewhere != "" && !force {
		return errors.New("refusing to destroy, nothing was removed: " + plan.ownedElsewhere)
	}
	if plan.keyDependents != "" && migrations {
		// --force does not override it: it is about the database, not
		// about edited files.
		return errors.New("refusing to destroy with --migrations, nothing was removed: " + plan.keyDependents)
	}

	var actions []destroyAction
	var kept []string
	var modified []string
	var removable []string
	for _, file := range plan.files {
		if file.shared {
			continue
		}
		current, err := root.ReadFile(file.path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			actions = append(actions, destroyAction{file.path, "skip", "not found"})
			continue
		case err != nil:
			return err
		case file.migration && !migrations:
			actions = append(actions, destroyAction{file.path, "keep", "a migration may already have run: `forge rollback` first, then pass --migrations"})
			kept = append(kept, file.path)
			continue
		}
		info, err := root.Lstat(file.path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("refusing to remove %s: it is not a regular file", file.path)
		}
		if string(current) != file.content {
			if file.translation && !force {
				actions = append(actions, destroyAction{file.path, "keep", "translated since it was generated: remove it by hand if no longer needed"})
				kept = append(kept, file.path)
				continue
			}
			modified = append(modified, file.path)
			if !force {
				continue
			}
			actions = append(actions, destroyAction{file.path, "remove", "differs from what the generator writes; removed because of --force"})
		} else {
			actions = append(actions, destroyAction{file.path, "remove", ""})
		}
		removable = append(removable, file.path)
	}
	if len(modified) > 0 && !force {
		return fmt.Errorf("refusing to destroy: %d file(s) differ from what this generator would write from these arguments (edited since, generated with other arguments, or written by another generator), so nothing was removed:\n  %s\nReview them, or pass --force to remove them anyway", len(modified), strings.Join(modified, "\n  "))
	}
	unwire := plan.routesVerb == "unwire"
	rewrites, err := checkRewrites(root, plan.rewrites, false)
	if err != nil && !force {
		return err
	}
	if err != nil {
		// --force: the edited file keeps its frame, which now loads a page
		// that no longer exists; say so.
		rewrites = nil
		for _, rewrite := range plan.rewrites {
			actions = append(actions, destroyAction{rewrite.path, "keep", "edited since it was generated: remove the frame of this scaffold by hand"})
		}
	}
	if len(removable) == 0 && len(kept) > 0 && !unwire {
		return fmt.Errorf("nothing to destroy but the migrations, which are kept:\n  %s\nRevert them with `forge rollback` if they ran, then run this command again with --migrations", strings.Join(kept, "\n  "))
	}
	if len(removable) == 0 && !unwire {
		return errors.New("nothing to destroy: none of the files this generator creates exist here (check the arguments: they must be the ones the generator was run with)")
	}
	if err := rejectExistingSymlinkPaths(root, removable); err != nil {
		return err
	}
	if !plan.dryRun {
		for _, path := range removable {
			if err := root.Remove(path); err != nil {
				return err
			}
		}
		removeEmptyParents(root, removable)
		if unwire {
			if err := rejectExistingSymlinkPaths(root, []string{scaffold.RoutesFile}); err != nil {
				return err
			}
			if err := writeRootFileAtomic(root, scaffold.RoutesFile, plan.routes); err != nil {
				return err
			}
		}
		for _, rewrite := range rewrites {
			if err := writeRootFileAtomic(root, rewrite.path, []byte(rewrite.to)); err != nil {
				return err
			}
		}
	}
	for _, rewrite := range rewrites {
		verb := "update"
		if plan.dryRun {
			verb = "would update"
		}
		actions = append(actions, destroyAction{rewrite.path, verb, "the frame of this scaffold is taken out"})
	}
	for _, action := range actions {
		verb := action.verb
		if plan.dryRun && verb == "remove" {
			verb = "would remove"
		}
		line := fmt.Sprintf("  %-14s%s", verb, action.path)
		if action.reason != "" {
			line += "  (" + action.reason + ")"
		}
		if _, err := fmt.Fprintln(stdout, line); err != nil {
			return err
		}
	}
	if plan.unwire == "" {
		// The application has the routes seam: the generator's block is
		// taken out of it (or was not there).
		if unwire {
			verb := "unwire"
			if plan.dryRun {
				verb = "would unwire"
			}
			if _, err := fmt.Fprintf(stdout, "  %-14s%s\n", verb, scaffold.RoutesFile); err != nil {
				return err
			}
		}
		_, err = fmt.Fprintln(stdout, "\nThen: forge test")
		return err
	}
	_, err = fmt.Fprintf(stdout, "\nForge does not edit app/bootstrap/bootstrap.go. Remove %s\n\n  and any import or helper only it used. Then: forge test\n", strings.TrimRight(plan.unwire, "\n"))
	return err
}

// removeEmptyParents removes the directories the removed files were in, and
// their parents, while they are empty and not part of the application's
// layout.
func removeEmptyParents(root *os.Root, removed []string) {
	var directories []string
	for _, path := range removed {
		for dir := filepath.Dir(path); !slices.Contains(directoriesKept, dir); dir = filepath.Dir(dir) {
			if !slices.Contains(directories, dir) {
				directories = append(directories, dir)
			}
		}
	}
	// Deepest first, so a directory is empty once its children are gone.
	slices.SortFunc(directories, func(a, b string) int { return len(b) - len(a) })
	for _, dir := range directories {
		if entries, err := fs.ReadDir(root.FS(), dir); err == nil && len(entries) == 0 {
			_ = root.Remove(dir)
		}
	}
}
