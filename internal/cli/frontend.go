package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/rocketinsidecorp/forge-go/internal/deploy"
	"github.com/rocketinsidecorp/forge-go/internal/frontend"
)

// tscInstall is the note `forge check` prints when tsc is missing: the check
// is skipped, not failed, because the application builds and runs without it
// (esbuild strips types; only the type check needs tsc).
const tscInstall = "note    tsc not found: app/javascript is compiled but its types were not checked. Install the checker with `npm install --global --ignore-scripts typescript@" + frontend.TypeScriptVersion + "` (Node is only needed for this check)."

// compileFrontend compiles the TypeScript front end of the application at
// root into app/javascript/dist, the files its binary embeds (ADR 0049), and
// returns how many files it wrote or removed. It does nothing for an
// application whose front end is JavaScript (`forge new --js`) or that has
// none. The notice goes to notices (stderr): stdout belongs to the command.
func compileFrontend(root string, notices io.Writer) (int, error) {
	changed, err := frontend.Sync(root)
	if err != nil {
		return 0, fmt.Errorf("compile %s: %w", frontend.SourceDir, err)
	}
	if len(changed) == 0 {
		return 0, nil
	}
	_, err = fmt.Fprintf(notices, "  compile %s/*.ts -> %s/%s (%d file(s) updated, esbuild %s)\n", frontend.SourceDir, frontend.SourceDir, frontend.DistDir, len(changed), frontend.EsbuildVersion)
	return len(changed), err
}

// runDeploy is `forge deploy`: when it builds the image from the working tree
// the TypeScript is compiled first, so an edited .ts file does not deploy as
// the old JavaScript. forge deploy refuses a working tree with uncommitted
// changes, so a recompiled dist says what to do about it.
func runDeploy(args []string, stdout, stderr io.Writer) error {
	changed := 0
	if deployBuilds(args) {
		var err error
		if changed, err = compileFrontend(".", stderr); err != nil {
			return err
		}
	}
	err := deploy.RunDeploy(args, stdout, stderr)
	if err != nil && changed > 0 {
		err = fmt.Errorf("%w\n(forge recompiled %s/%s before deploying: %d file(s) changed because the TypeScript changed since dist was compiled; commit the compiled files with their sources, then deploy again)", err, frontend.SourceDir, frontend.DistDir, changed)
	}
	return err
}

// typeCheckFrontend runs `tsc --noEmit` on the application's TypeScript, with
// the tsconfig.json `forge new` generated. esbuild strips types without
// checking them, so this is what finds a type error. A missing tsc is a note,
// not a failure.
func typeCheckFrontend(ctx context.Context, root string, stdout, stderr io.Writer) error {
	project, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer func() { _ = project.Close() }()
	if !frontend.IsTypeScript(project.FS()) {
		return nil
	}
	if _, err := project.Stat("tsconfig.json"); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			_, err = fmt.Fprintln(stdout, "  note    tsconfig.json is missing, so app/javascript was not type-checked")
		}
		return err
	}
	tsc, err := exec.LookPath("tsc")
	if err != nil {
		_, err = fmt.Fprintln(stdout, "  "+tscInstall)
		return err
	}
	fmt.Fprintln(stdout, "  run     tsc --noEmit")
	command := exec.CommandContext(ctx, tsc, "--noEmit", "--project", "tsconfig.json") // #nosec G204 -- the tsc found on PATH, fixed arguments, no shell
	command.Dir = filepath.Clean(root)
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		return toolError("tsc", err)
	}
	return nil
}

// deployBuilds reports whether `forge deploy ARGS` builds the image from the
// working tree: the bare command, without --skip-build or --dry-run. The
// image embeds app/javascript/dist, so a TypeScript application is compiled
// first, or an edited .ts file would deploy as the old JavaScript.
func deployBuilds(args []string) bool {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return false // a subcommand: setup, rollback, prune, details, lock, maintenance, init
	}
	return !slices.ContainsFunc(args, func(arg string) bool {
		name, _, _ := strings.Cut(arg, "=")
		return name == "--skip-build" || name == "--dry-run" || name == "-h" || name == "--help"
	})
}
