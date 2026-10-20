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
)

const destroyUsage = `usage: forge destroy <model|resource|scaffold|controller|mailer|job|view> ARGUMENTS... [--force] [--migrations] [--dry-run]
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
		return errors.New(destroyUsage)
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
	plan, err := planDestroy(generator, rest, root, env, now)
	if err != nil {
		return err
	}
	return applyDestroy(stdout, root, plan, force, migrations)
}

// destroyPlan is what a generator would create, found where it is.
type destroyPlan struct {
	files  []generatedFile
	dryRun bool
	// unwire says what to take out of app/bootstrap/bootstrap.go, which
	// neither generating nor destroying edits.
	unwire string
}

func planDestroy(generator string, args []string, root string, env []string, now time.Time) (destroyPlan, error) {
	switch generator {
	case "model":
		request, err := parseModelArgs(args)
		if err != nil {
			return destroyPlan{}, err
		}
		request.Destroy = true
		spec, files, err := planResource(root, request, env, now)
		if err != nil {
			return destroyPlan{}, err
		}
		line := fmt.Sprintf("%[1]sService, err := application.New%[2]sService(persistence.New%[2]sRepository(database), time.Now, uuid.New)", lowerCamel(spec.Name), exportedName(spec.Name))
		return destroyPlan{files: files, dryRun: request.DryRun, unwire: "the " + spec.Name + " use cases you built: " + line}, nil
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
		var extend planExtension
		if generator == "scaffold" {
			request.Scaffold = true
			extend = planScaffoldFiles
		}
		spec, files, err := planResourceWith(root, request, env, now, extend)
		if err != nil {
			return destroyPlan{}, err
		}
		return destroyPlan{files: files, dryRun: request.DryRun, unwire: "the " + generator + " wiring:\n" + buildResourceWiring(spec).Usage}, nil
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
		return destroyPlan{files: files, dryRun: request.DryRun, unwire: "the controller wiring:\n" + buildControllerWiring(spec).Usage}, nil
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
	case "migration":
		return destroyPlan{}, errors.New("forge destroy does not remove migrations: one that ran is part of the database's history. Revert it with `forge rollback` and delete db/migrations/<version>_NAME.{up,down}.sql yourself")
	default:
		return destroyPlan{}, fmt.Errorf("unknown generator %q\n%s", generator, destroyUsage)
	}
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
		return destroyPlan{}, errors.New(destroyUsage)
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
		return destroyPlan{}, errors.New(destroyUsage)
	}
	dir, base, err := splitViewName(positional[0])
	if err != nil {
		return destroyPlan{}, err
	}
	projectRoot, _, err := openApplication(root)
	if err != nil {
		return destroyPlan{}, err
	}
	_ = projectRoot.Close()
	path := filepath.Join("app", "views", dir, base+".gohtml")
	return destroyPlan{
		files:  []generatedFile{{path: path, content: generatedView}},
		dryRun: flags["--dry-run"],
		unwire: fmt.Sprintf("the handler that renders %q", dir+"/"+base),
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

	var actions []destroyAction
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
			modified = append(modified, file.path)
			if !force {
				continue
			}
			actions = append(actions, destroyAction{file.path, "remove", "modified; removed because of --force"})
		} else {
			actions = append(actions, destroyAction{file.path, "remove", ""})
		}
		removable = append(removable, file.path)
	}
	if len(modified) > 0 && !force {
		return fmt.Errorf("refusing to destroy: %d file(s) changed since they were generated, so nothing was removed:\n  %s\nReview them, or pass --force to remove them anyway", len(modified), strings.Join(modified, "\n  "))
	}
	if len(removable) == 0 {
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
