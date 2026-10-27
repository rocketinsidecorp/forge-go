package authsetup_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	forge "github.com/rocketinsidecorp/forge-go"
	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/auth/authsetup"
)

const password = "correct horse battery staple"

type recorder struct {
	accounts []authsetup.Account
	err      error
}

func (r *recorder) provision(_ context.Context, _ forge.CommandEnv, account authsetup.Account) (authsetup.Provisioned, error) {
	r.accounts = append(r.accounts, account)
	return authsetup.Provisioned{Email: account.Email, OrgSlug: account.OrgSlug, Permissions: 4}, r.err
}

func run(t *testing.T, organizations bool, stdin string, env map[string]string, args ...string) (*recorder, string, error) {
	t.Helper()
	r := &recorder{}
	var stdout, stderr bytes.Buffer
	command := authsetup.Command(authsetup.Config{Organizations: organizations, Provision: r.provision})
	err := command.Run(context.Background(), forge.CommandEnv{
		Stdin: strings.NewReader(stdin), Stdout: &stdout, Stderr: &stderr,
		LookupEnv: func(key string) (string, bool) { value, ok := env[key]; return value, ok },
	}, args)
	if strings.Contains(stdout.String()+stderr.String(), password) {
		t.Fatal("the password was printed")
	}
	return r, stdout.String(), err
}

func TestSingleTenantSetupTakesTheEmailOnly(t *testing.T) {
	t.Parallel()
	r, stdout, err := run(t, false, password+"\n", nil, "--email", "Owner@Example.com", "--password-stdin")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.accounts) != 1 || r.accounts[0].OrgSlug != auth.DefaultTenantSlug || r.accounts[0].OrganizationName != authsetup.DefaultOrganizationName || r.accounts[0].Password != password {
		t.Fatalf("accounts = %+v", r.accounts)
	}
	if strings.Contains(stdout, "organization") || !strings.Contains(stdout, "Sign in at /session/new.") || !strings.Contains(stdout, "(4 permissions)") {
		t.Fatalf("stdout = %q", stdout)
	}
	for _, args := range [][]string{
		{"--org", "acme", "--email", "owner@example.com", "--password-stdin"},
		{"--tenant", "acme", "--email", "owner@example.com", "--password-stdin"},
		{"--org-name", "Acme", "--email", "owner@example.com", "--password-stdin"},
	} {
		if _, _, err := run(t, false, password+"\n", nil, args...); !errors.Is(err, forge.ErrUsage) || !strings.Contains(err.Error(), "--email only") {
			t.Fatalf("%v: %v", args, err)
		}
	}
	if !strings.Contains(authsetup.Command(authsetup.Config{}).Summary, "owner account") {
		t.Fatal("summary")
	}
}

func TestMultiTenantSetupRequiresTheOrganization(t *testing.T) {
	t.Parallel()
	r, stdout, err := run(t, true, "", map[string]string{authsetup.PasswordVariable: password}, "--org", "Acme", "--email", "owner@example.com", "--org-name", "  Acme Inc ")
	if err != nil {
		t.Fatal(err)
	}
	if r.accounts[0].OrgSlug != "acme" || r.accounts[0].OrganizationName != "Acme Inc" {
		t.Fatalf("account = %+v", r.accounts[0])
	}
	if !strings.Contains(stdout, `sign-in slug "acme"`) || !strings.Contains(stdout, `with organization "acme"`) {
		t.Fatalf("stdout = %q", stdout)
	}
	r, _, err = run(t, true, password+"\n", nil, "--tenant", "globex", "--email", "owner@example.com", "--password-stdin")
	if err != nil || r.accounts[0].OrganizationName != "globex" {
		t.Fatalf("--tenant alias: %+v %v", r.accounts, err)
	}
	if !strings.Contains(authsetup.Command(authsetup.Config{Organizations: true}).Summary, "organization") {
		t.Fatal("summary")
	}
	for name, args := range map[string][]string{
		"no org":       {"--email", "owner@example.com", "--password-stdin"},
		"no email":     {"--org", "acme", "--password-stdin"},
		"bad slug":     {"--org", "A B", "--email", "owner@example.com", "--password-stdin"},
		"bad email":    {"--org", "acme", "--email", "owner@İcloud.com", "--password-stdin"},
		"bad name":     {"--org", "acme", "--email", "owner@example.com", "--org-name", "bad\x07name", "--password-stdin"},
		"unknown flag": {"--org", "acme", "--email", "owner@example.com", "--bogus"},
		"positional":   {"--org", "acme", "--email", "owner@example.com", "extra"},
	} {
		if _, _, err := run(t, true, password+"\n", nil, args...); !errors.Is(err, forge.ErrUsage) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestSetupReadsThePasswordSafely(t *testing.T) {
	t.Parallel()
	valid := []string{"--email", "owner@example.com"}
	if _, _, err := run(t, false, password+"\n", map[string]string{authsetup.PasswordVariable: password}, append(valid, "--password-stdin")...); !errors.Is(err, forge.ErrUsage) {
		t.Fatalf("both sources: %v", err)
	}
	if _, _, err := run(t, false, "too short\n", nil, append(valid, "--password-stdin")...); err == nil || errors.Is(err, forge.ErrUsage) {
		t.Fatalf("short password: %v", err)
	}
	if _, _, err := run(t, false, "", nil, valid...); !errors.Is(err, forge.ErrUsage) {
		t.Fatalf("no terminal: %v", err)
	}
	command := authsetup.Command(authsetup.Config{Provision: (&recorder{}).provision})
	if err := command.Run(context.Background(), forge.CommandEnv{Stdout: os.Stdout, Stderr: os.Stderr}, append(valid, "--password-stdin")); err == nil {
		t.Fatal("--password-stdin without standard input succeeded")
	}
	if err := command.Run(context.Background(), forge.CommandEnv{Stdin: errReader{}, Stdout: os.Stdout, Stderr: os.Stderr}, append(valid, "--password-stdin")); err == nil {
		t.Fatal("an unreadable standard input succeeded")
	}
	if err := authsetup.Command(authsetup.Config{}).Run(context.Background(), forge.CommandEnv{}, valid); err == nil {
		t.Fatal("a command without a store succeeded")
	}
	failing := &recorder{err: errors.New("database down")}
	err := authsetup.Command(authsetup.Config{Provision: failing.provision}).Run(context.Background(), forge.CommandEnv{
		Stdin: strings.NewReader(password + "\n"), Stdout: os.Stdout, Stderr: os.Stderr,
	}, append(valid, "--password-stdin"))
	if err == nil || err.Error() != "database down" {
		t.Fatalf("provision failure: %v", err)
	}
	if authsetup.Usage(true) == authsetup.Usage(false) {
		t.Fatal("usage does not depend on organizations")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("broken pipe") }
