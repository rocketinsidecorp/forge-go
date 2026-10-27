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
// rollback, seed and setup subcommands over the schemas it composes), which
// cmd/NAME/main.go passes to forge.Main. `forge new` writes it, and `forge
// generate authentication` and `forge generate tenancy` rewrite it whole; a
// checksum line records what a generator last wrote, so a hand edit is
// refused rather than overwritten, as with routes_gen.go.
var AppFile = filepath.Join("app", "bootstrap", "app_gen.go")

// AppFeatures is what app_gen.go says.
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
}

// ErrAppEdited reports an app_gen.go whose checksum does not match.
var ErrAppEdited = errors.New("app/bootstrap/app_gen.go was edited by hand since a generator last wrote it (its forge:checksum no longer matches), so nothing was written. " +
	"Generators own that file: add your own commands in cmd/<name>/main.go (forge.Main(bootstrap.Configure, append(bootstrap.Commands(), yours...)...)), restore the file with `git checkout app/bootstrap/app_gen.go` and run the command again")

const appHeader = `// Features of this application, as the generators set them. ` + "`forge new`" + ` writes
// this file; ` + "`forge generate authentication` and `forge generate tenancy`" + `
// rewrite it. Do not edit it: they refuse to change it once its checksum no
// longer matches. Add your own commands in cmd/<name>/main.go instead.
//
`

// RenderApp renders app_gen.go for features, formatted and with its checksum.
func RenderApp(features AppFeatures) ([]byte, error) {
	switch features.Adapter {
	case "postgres", "mysql", "sqlite":
	default:
		return nil, fmt.Errorf("app_gen.go needs a database adapter, not %q", features.Adapter)
	}
	if features.Organizations && !features.Identity {
		return nil, errors.New("an application with organizations needs Forge identity")
	}
	adapter := features.Adapter
	imports := []string{`"io/fs"`, "", `forge "` + FrameworkModule + `"`}
	sets := []string{}
	if features.Identity {
		imports = append(imports, "auth"+adapter+` "`+FrameworkModule+"/auth/"+adapter+`"`)
		sets = append(sets, "auth"+adapter+".Migrations()")
	}
	if features.Jobs {
		imports = append(imports, "jobs"+adapter+` "`+FrameworkModule+"/jobs/"+adapter+`"`)
		sets = append(sets, "jobs"+adapter+".Migrations()")
	}
	imports = append(imports, strconv.Quote(FrameworkModule+"/"+adapter), "", strconv.Quote(features.Module+"/db"))
	sets = append(sets, "db.Migrations()")

	var source bytes.Buffer
	source.WriteString(appHeader + checksumPrefix + checksumPlaceholder + "\n\npackage bootstrap\n\nimport (\n")
	for _, line := range imports {
		if line != "" {
			line = "\t" + line
		}
		source.WriteString(line + "\n")
	}
	source.WriteString(")\n\n")
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
	fmt.Fprintf(&source, "\n// MigrationSets are what migrate applies, in order: %s.\n// The generated tests migrate the same.\nfunc MigrationSets() []fs.FS {\n\treturn []fs.FS{%s}\n}\n", describeSets(features), strings.Join(sets, ", "))
	source.WriteString("\n// Commands are the subcommands of the binary besides serving: migrate and\n// rollback over MigrationSets, seed")
	if features.Identity {
		source.WriteString(", and setup, which creates the owner")
	}
	source.WriteString(".\nfunc Commands() []forge.Command {\n")
	fmt.Fprintf(&source, "\tcommands := %s.CommandsFromSets(MigrationSets()...)\n", adapter)
	if features.Identity {
		fmt.Fprintf(&source, "\tcommands = append(commands, auth%[1]s.SetupCommand(auth%[1]s.SetupOrganizations(Organizations)))\n", adapter)
	}
	fmt.Fprintf(&source, "\treturn append(commands, %s.SeedCommand(db.Seed))\n}\n", adapter)
	formatted, err := format.Source(source.Bytes())
	if err != nil {
		return nil, fmt.Errorf("format %s: %w\n%s", AppFile, err, source.String())
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
	sets = append(sets, "db/migrations")
	if len(sets) == 1 {
		return sets[0]
	}
	return strings.Join(sets[:len(sets)-1], ", ") + " and " + sets[len(sets)-1]
}

// ParseApp verifies the checksum of app_gen.go and returns what it says
// about Identity, Jobs and Organizations (Module and Adapter stay empty). It returns
// ErrAppEdited when the file is not exactly what a generator wrote.
func ParseApp(source []byte) (AppFeatures, error) {
	text := string(source)
	start := strings.Index(text, "\n"+checksumPrefix)
	if start < 0 {
		return AppFeatures{}, ErrAppEdited
	}
	start += 1 + len(checksumPrefix)
	end := strings.IndexByte(text[start:], '\n')
	if end < 0 {
		return AppFeatures{}, ErrAppEdited
	}
	sum := sha256.Sum256([]byte(text[:start] + checksumPlaceholder + text[start+end:]))
	if text[start:start+end] != hex.EncodeToString(sum[:]) {
		return AppFeatures{}, ErrAppEdited
	}
	return AppFeatures{
		Identity:      strings.Contains(text, `"`+FrameworkModule+"/auth/"),
		Jobs:          strings.Contains(text, `"`+FrameworkModule+"/jobs/"),
		Organizations: strings.Contains(text, "\nconst Organizations = true\n"),
	}, nil
}
