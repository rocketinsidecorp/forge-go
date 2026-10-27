package scaffold

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"go/format"
	"path/filepath"
	"strconv"
	"strings"
)

// AppFile is app/bootstrap/app_gen.go: the application's features, as the
// generators set them. It declares Organizations (whether the application
// has organizations, decision D33) and Commands (the binary's migrate,
// rollback, seed and setup subcommands over db.MigrationSets), which
// cmd/NAME/main.go passes to forge.Main. `forge new` writes it, and `forge
// generate authentication` and `forge generate tenancy` rewrite it whole; a
// checksum line records what a generator last wrote, so a hand edit is
// refused rather than overwritten, as with routes_gen.go.
var AppFile = filepath.Join("app", "bootstrap", "app_gen.go")

// SetsFile is db/migration_sets_gen.go: MigrationSets, the migration sets
// the application composes (Forge identity, the job schedules,
// db/migrations). It lives in package db, not app/bootstrap, so that the
// generated persistence tests migrate exactly what the application migrates
// without importing the composition root (the architecture test forbids
// app/adapters to import app/bootstrap). `forge new` writes it and `forge
// generate authentication` rewrites it, under the same checksum rule.
var SetsFile = filepath.Join("db", "migration_sets_gen.go")

// AppFeatures is what app_gen.go and migration_sets_gen.go say.
type AppFeatures struct {
	Module string
	// Adapter is postgres, mysql or sqlite.
	Adapter string
	// Jobs composes the job schedule migrations of the adapter (PostgreSQL
	// and MySQL).
	Jobs bool
	// Identity composes Forge identity: its migrations and the setup
	// command (forge new --auth, forge generate authentication).
	Identity bool
	// Organizations is a multi-tenant application (forge new --tenants,
	// forge generate tenancy).
	Organizations bool
	// Queue composes the persistent job queue: its migrations and the
	// binary's jobs command (forge new with a database). An application
	// generated before it keeps running without it.
	Queue bool
}

// ErrAppEdited reports an app_gen.go whose checksum does not match.
var ErrAppEdited = errors.New("app/bootstrap/app_gen.go was edited by hand since a generator last wrote it (its forge:checksum no longer matches), so nothing was written. " +
	"Generators own that file: add your own commands in cmd/<name>/main.go (forge.Main(bootstrap.Configure, append(bootstrap.Commands(), yours...)...)), restore the file with `git checkout app/bootstrap/app_gen.go` and run the command again")

// ErrSetsEdited reports a migration_sets_gen.go whose checksum does not match.
var ErrSetsEdited = errors.New("db/migration_sets_gen.go was edited by hand since a generator last wrote it (its forge:checksum no longer matches), so nothing was written. " +
	"Generators own that file: restore it with `git checkout db/migration_sets_gen.go` and run the command again")

const appHeader = `// Features of this application, as the generators set them. ` + "`forge new`" + ` writes
// this file; ` + "`forge generate authentication` and `forge generate tenancy`" + `
// rewrite it. Do not edit it: they refuse to change it once its checksum no
// longer matches. Add your own commands in cmd/<name>/main.go instead.
//
`

const setsHeader = `// The migration sets of this application, as the generators set them.
// ` + "`forge new`" + ` writes this file; ` + "`forge generate authentication`" + ` rewrites it.
// Do not edit it: it refuses to change it once its checksum no longer matches.
//
`

func validFeatures(features AppFeatures) error {
	switch features.Adapter {
	case "postgres", "mysql", "sqlite":
	default:
		return fmt.Errorf("app_gen.go needs a database adapter, not %q", features.Adapter)
	}
	if features.Organizations && !features.Identity {
		return errors.New("an application with organizations needs Forge identity")
	}
	return nil
}

// RenderApp renders app_gen.go for features, formatted and with its checksum.
func RenderApp(features AppFeatures) ([]byte, error) {
	if err := validFeatures(features); err != nil {
		return nil, err
	}
	adapter := features.Adapter
	imports := []string{`forge "` + FrameworkModule + `"`}
	if features.Identity {
		imports = append(imports, "auth"+adapter+` "`+FrameworkModule+"/auth/"+adapter+`"`)
	}
	if features.Queue {
		imports = append(imports, "queue"+adapter+` "`+FrameworkModule+"/queue/"+adapter+`"`)
	}
	imports = append(imports, strconv.Quote(FrameworkModule+"/"+adapter), "", strconv.Quote(features.Module+"/db"))

	var source bytes.Buffer
	source.WriteString(appHeader + checksumPrefix + checksumPlaceholder + "\n\npackage bootstrap\n\n")
	writeImports(&source, imports)
	if features.Organizations {
		source.WriteString(`// Organizations is true: users belong to organizations (forge new
// --tenants, or forge generate tenancy). Sign-in asks for the organization,
// setup for --org, and generated tables need a scope: --org-scoped (an org_id
// column every query is filtered by) or --global.
const Organizations = true
`)
	} else {
		source.WriteString(`// Organizations is false: a single-organization application. Generated
// tables are plain tables; sign-in asks for the email and password only, and
// setup creates the one hidden organization (auth.DefaultTenantSlug) every
// user belongs to. ` + "`forge generate tenancy`" + ` converts the application.
const Organizations = false
`)
	}
	source.WriteString("\n// Commands are the subcommands of the binary besides serving: migrate and\n// rollback over db.MigrationSets, seed")
	if features.Queue {
		source.WriteString(", jobs (inspects and repairs the job queue)")
	}
	if features.Identity {
		source.WriteString(", and setup, which creates the owner")
	}
	source.WriteString(".\nfunc Commands() []forge.Command {\n")
	fmt.Fprintf(&source, "\tcommands := %s.CommandsFromSets(db.MigrationSets()...)\n", adapter)
	if features.Identity {
		fmt.Fprintf(&source, "\tcommands = append(commands, auth%[1]s.SetupCommand(auth%[1]s.SetupOrganizations(Organizations)))\n", adapter)
	}
	if features.Queue {
		fmt.Fprintf(&source, "\tcommands = append(commands, queue%s.Commands()...)\n", adapter)
	}
	fmt.Fprintf(&source, "\treturn append(commands, %s.SeedCommand(db.Seed))\n}\n", adapter)
	return checksummed(AppFile, source.Bytes())
}

// RenderSets renders migration_sets_gen.go for features, formatted and with
// its checksum.
func RenderSets(features AppFeatures) ([]byte, error) {
	if err := validFeatures(features); err != nil {
		return nil, err
	}
	adapter := features.Adapter
	imports := []string{`"io/fs"`}
	var sets []string
	if features.Identity || features.Jobs || features.Queue {
		imports = append(imports, "")
	}
	if features.Identity {
		imports = append(imports, "auth"+adapter+` "`+FrameworkModule+"/auth/"+adapter+`"`)
		sets = append(sets, "auth"+adapter+".Migrations()")
	}
	if features.Jobs {
		imports = append(imports, "jobs"+adapter+` "`+FrameworkModule+"/jobs/"+adapter+`"`)
		sets = append(sets, "jobs"+adapter+".Migrations()")
	}
	if features.Queue {
		imports = append(imports, "queue"+adapter+` "`+FrameworkModule+"/queue/"+adapter+`"`)
		sets = append(sets, "queue"+adapter+".Migrations()")
	}
	sets = append(sets, "Migrations()")
	var source bytes.Buffer
	source.WriteString(setsHeader + checksumPrefix + checksumPlaceholder + "\n\npackage db\n\n")
	writeImports(&source, imports)
	fmt.Fprintf(&source, "// MigrationSets are what migrate applies, in order: %s. The\n// generated tests migrate the same.\nfunc MigrationSets() []fs.FS {\n\treturn []fs.FS{%s}\n}\n", describeSets(features), strings.Join(sets, ", "))
	return checksummed(SetsFile, source.Bytes())
}

func writeImports(source *bytes.Buffer, imports []string) {
	source.WriteString("import (\n")
	for _, line := range imports {
		if line != "" {
			line = "\t" + line
		}
		source.WriteString(line + "\n")
	}
	source.WriteString(")\n\n")
}

// checksummed formats source and fills in its checksum line.
func checksummed(path string, source []byte) ([]byte, error) {
	formatted, err := format.Source(source)
	if err != nil {
		return nil, fmt.Errorf("format %s: %w\n%s", path, err, source)
	}
	sum := sha256.Sum256(formatted)
	return bytes.Replace(formatted, []byte(checksumPrefix+checksumPlaceholder), []byte(checksumPrefix+hex.EncodeToString(sum[:])), 1), nil
}

func describeSets(features AppFeatures) string {
	var sets []string
	if features.Identity {
		sets = append(sets, "Forge identity")
	}
	if features.Jobs {
		sets = append(sets, "the job schedules")
	}
	if features.Queue {
		sets = append(sets, "the job queue")
	}
	sets = append(sets, "db/migrations")
	if len(sets) == 1 {
		return sets[0]
	}
	return strings.Join(sets[:len(sets)-1], ", ") + " and " + sets[len(sets)-1]
}

// ParseApp verifies the checksum of app_gen.go and returns what it says
// about Identity and Organizations (the other fields stay empty). It returns
// ErrAppEdited when the file is not exactly what a generator wrote.
func ParseApp(source []byte) (AppFeatures, error) {
	if !checksumMatches(source) {
		return AppFeatures{}, ErrAppEdited
	}
	text := string(source)
	return AppFeatures{
		Identity:      strings.Contains(text, `"`+FrameworkModule+"/auth/"),
		Organizations: strings.Contains(text, "\nconst Organizations = true\n"),
		Queue:         strings.Contains(text, `"`+FrameworkModule+"/queue/"),
	}, nil
}

// ParseSets verifies the checksum of migration_sets_gen.go and returns what
// it says about Identity, Jobs and Queue. It returns ErrSetsEdited when the file is
// not exactly what a generator wrote.
func ParseSets(source []byte) (AppFeatures, error) {
	if !checksumMatches(source) {
		return AppFeatures{}, ErrSetsEdited
	}
	text := string(source)
	return AppFeatures{
		Identity: strings.Contains(text, `"`+FrameworkModule+"/auth/"),
		Jobs:     strings.Contains(text, `"`+FrameworkModule+"/jobs/"),
		Queue:    strings.Contains(text, `"`+FrameworkModule+"/queue/"),
	}, nil
}

// checksumMatches reports whether source's forge:checksum line is the
// SHA-256 of source with that line's value replaced by the placeholder.
func checksumMatches(source []byte) bool {
	text := string(source)
	start := strings.Index(text, "\n"+checksumPrefix)
	if start < 0 {
		return false
	}
	start += 1 + len(checksumPrefix)
	end := strings.IndexByte(text[start:], '\n')
	if end < 0 {
		return false
	}
	sum := sha256.Sum256([]byte(text[:start] + checksumPlaceholder + text[start+end:]))
	return text[start:start+end] == hex.EncodeToString(sum[:])
}
