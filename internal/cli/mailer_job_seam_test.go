package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// `forge generate mailer` and `forge generate job --queue` write their wiring
// into the routes seam (app/bootstrap/routes_gen.go) like resource, scaffold
// and controller do, and `forge destroy` takes it out again, so nothing is
// pasted by hand (audit A-6 and D-10).

func withoutDatabaseEnv() []string {
	var env []string
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "FORGE_DATABASE_URL=") {
			env = append(env, entry)
		}
	}
	return env
}

func seamApp(t *testing.T, args ...string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "shop")
	var stdout, stderr bytes.Buffer
	all := append([]string{"new", "shop", "--module", "example.com/shop", "--dir", dir, "--skip-deps"}, args...)
	if code := Run(all, &stdout, &stderr); code != 0 {
		t.Fatalf("forge %v: %d %s %s", all, code, stdout.String(), stderr.String())
	}
	return dir
}

func TestMailerAndJobWireThemselvesIntoTheRoutesSeamAndDestroyUnwiresThem(t *testing.T) {
	t.Parallel()
	for _, database := range []string{"sqlite", "postgresql", "none"} {
		t.Run(database, func(t *testing.T) {
			t.Parallel()
			dir := seamApp(t, "-d", database)
			seamPath := filepath.Join(dir, "app", "bootstrap", "routes_gen.go")
			empty := readFile(t, seamPath)
			env := withoutDatabaseEnv()
			now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
			generate := func(args ...string) string {
				t.Helper()
				var out bytes.Buffer
				if err := runGenerateEnv(args, &out, dir, env, now); err != nil {
					t.Fatalf("generate %v: %v\n%s", args, err, out.String())
				}
				return out.String()
			}
			destroy := func(args ...string) {
				t.Helper()
				var out bytes.Buffer
				if err := runDestroyEnv(args, &out, dir, env, now); err != nil {
					t.Fatalf("destroy %v: %v\n%s", args, err, out.String())
				}
			}

			output := generate("mailer", "user", "welcome:name")
			seam := readFile(t, seamPath)
			for _, want := range []string{"// forge:mail mailSender", "mail.FromConfig(app.Config()", "// forge:mailer user", "mailers.NewUserMailer(mailSender, mailFrom)", `"GET /_mail/user/"`} {
				if !strings.Contains(seam, want) {
					t.Errorf("the seam lacks %q after generate mailer:\n%s", want, seam)
				}
			}
			if !strings.Contains(output, "wire  "+filepath.Join("app", "bootstrap", "routes_gen.go")) || strings.Contains(output, "Forge did not edit") {
				t.Errorf("generate mailer still prints wiring to paste:\n%s", output)
			}
			// A second mailer shares the sender.
			generate("mailer", "order_notice", "shipped:order_id")
			if got := readFile(t, seamPath); strings.Count(got, "// forge:mail mailSender") != 1 || !strings.Contains(got, "// forge:mailer order_notice") {
				t.Errorf("the second mailer:\n%s", got)
			}

			if database != "none" {
				output = generate("job", "send_welcome", "user_id:uuid", "--queue")
				seam = readFile(t, seamPath)
				if !strings.Contains(seam, "// forge:job send_welcome") || !strings.Contains(seam, "jobs.RegisterSendWelcome(registry)") || !strings.Contains(seam, "registry *queue.Registry") {
					t.Errorf("the seam does not register the job:\n%s", seam)
				}
				if strings.Contains(output, "Forge did not edit") {
					t.Errorf("generate job --queue still prints wiring to paste:\n%s", output)
				}
				destroy("job", "send_welcome", "user_id:uuid", "--queue")
				if got := readFile(t, seamPath); strings.Contains(got, "send_welcome") {
					t.Errorf("destroy job left its registration:\n%s", got)
				}
			}

			destroy("mailer", "user", "welcome:name")
			if got := readFile(t, seamPath); strings.Contains(got, "forge:mailer user") || !strings.Contains(got, "// forge:mail mailSender") {
				t.Errorf("destroying one mailer must keep the sender the other uses:\n%s", got)
			}
			destroy("mailer", "order_notice", "shipped:order_id")
			if got := readFile(t, seamPath); got != empty {
				t.Errorf("destroying every mailer and job did not restore the seam:\n%s", got)
			}
		})
	}
}

// An application created before forge new passed the job registry to the seam
// keeps the printed registration: nothing is wired behind its back.
func TestQueueJobPrintsItsRegistrationWithoutTheRegistryInTheSeamCall(t *testing.T) {
	t.Parallel()
	dir := seamApp(t, "-d", "sqlite")
	bootstrap := filepath.Join(dir, "app", "bootstrap", "bootstrap.go")
	source := readFile(t, bootstrap)
	source = strings.Replace(source, "generatedRoutes(ctx, app, database, registry, translator)", "generatedRoutes(ctx, app, database, translator)", 1)
	if source == readFile(t, bootstrap) {
		t.Fatal("the generated bootstrap.go does not pass the registry to the seam")
	}
	writeFile(t, bootstrap, source)
	seamPath := filepath.Join(dir, "app", "bootstrap", "routes_gen.go")
	before := readFile(t, seamPath)
	var out bytes.Buffer
	if err := runGenerateEnv([]string{"job", "send_welcome", "--queue"}, &out, dir, withoutDatabaseEnv(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Forge did not edit app/bootstrap/bootstrap.go. Register the handler") || readFile(t, seamPath) != before {
		t.Errorf("an application without the registry in its seam call:\n%s", out.String())
	}
}

// A seam edited by hand is refused before anything is written, as for the
// other generators.
func TestMailerRefusesAnEditedSeamAndWritesNothing(t *testing.T) {
	t.Parallel()
	dir := seamApp(t, "-d", "sqlite")
	seamPath := filepath.Join(dir, "app", "bootstrap", "routes_gen.go")
	writeFile(t, seamPath, readFile(t, seamPath)+"\n// edited\n")
	var out bytes.Buffer
	if err := runGenerateEnv([]string{"mailer", "user", "welcome:name"}, &out, dir, withoutDatabaseEnv(), time.Now()); err == nil || !strings.Contains(err.Error(), "edited by hand") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "app", "mailers")); !os.IsNotExist(err) {
		t.Errorf("a refused seam still wrote the mailer: %v", err)
	}
}
