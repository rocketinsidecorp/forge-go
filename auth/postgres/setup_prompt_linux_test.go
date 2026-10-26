//go:build linux

package postgres_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/unix"

	forge "github.com/rocketinsidecorp/forge-go"
	"github.com/rocketinsidecorp/forge-go/auth"
	authpostgres "github.com/rocketinsidecorp/forge-go/auth/postgres"
	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/dbtest"
	"github.com/rocketinsidecorp/forge-go/migrate"
	"github.com/rocketinsidecorp/forge-go/postgres"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"
)

// openPTY returns a pseudo-terminal: the master a test types into and the
// slave setup reads the password from. Skipped where /dev/ptmx is missing.
func openPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Skipf("no pseudo-terminal available: %v", err)
	}
	master = os.NewFile(uintptr(fd), "/dev/ptmx")
	t.Cleanup(func() { _ = master.Close() })
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		t.Skipf("cannot unlock the pseudo-terminal: %v", err)
	}
	number, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		t.Skipf("cannot name the pseudo-terminal: %v", err)
	}
	slave, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("cannot open the pseudo-terminal: %v", err)
	}
	t.Cleanup(func() { _ = slave.Close() })
	return master, slave
}

// TestSetupCommandPromptsForThePasswordTwiceAtATerminal drives the
// interactive path of setup through a pseudo-terminal: the password is read
// without echo, must be confirmed, and never reaches the output.
func TestSetupCommandPromptsForThePasswordTwiceAtATerminal(t *testing.T) {
	db, database := postgrestest.NewWithConfig(t)
	postgrestest.RequireUUIDv7(t, db) // the identity migrations use DEFAULT uuidv7()
	dbtest.Migrate(t, authpostgres.Migrations(), func(loaded []migrate.Migration, logger *slog.Logger) (*migrate.Migrator, error) {
		return postgres.NewMigrator(database, loaded, logger)
	})
	cfg := config.Default()
	cfg.Database = database
	command := authpostgres.SetupCommand()
	run := func(stdin *os.File) (stdout, stderr string, err error) {
		var out, errOut bytes.Buffer
		err = command.Run(context.Background(), forge.CommandEnv{
			Config: cfg, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Stdout: &out, Stderr: &errOut, Stdin: stdin,
			LookupEnv: func(string) (string, bool) { return "", false },
		}, []string{"--tenant", "acme", "--email", "owner@example.com"})
		return out.String(), errOut.String(), err
	}

	// Two different entries are refused before the database is touched.
	master, slave := openPTY(t)
	if _, err := master.WriteString(ownerPassword + "\n" + ownerPassword + "x\n"); err != nil {
		t.Fatal(err)
	}
	_, stderr, err := run(slave)
	if err == nil || !strings.Contains(err.Error(), "do not match") {
		t.Fatalf("mismatch: %v", err)
	}
	if !strings.Contains(stderr, "Password for owner@example.com:") || !strings.Contains(stderr, "Confirm password:") || strings.Contains(stderr, ownerPassword) {
		t.Errorf("stderr=%q", stderr)
	}

	// A terminal that is gone is not a terminal: setup asks for another source.
	master, slave = openPTY(t)
	_ = master.Close()
	if _, _, err := run(slave); !errors.Is(err, forge.ErrUsage) {
		t.Fatalf("hang-up before asking: %v", err)
	}

	// A terminal that hangs up between the entry and its confirmation fails
	// the confirmation read; the hang-up is triggered by the prompt itself.
	master, slave = openPTY(t)
	if _, err := master.WriteString(ownerPassword + "\n"); err != nil {
		t.Fatal(err)
	}
	confirming := make(chan struct{})
	var once sync.Once
	var out bytes.Buffer
	prompts := writerFunc(func(p []byte) (int, error) {
		if bytes.Contains(p, []byte("Confirm password:")) {
			once.Do(func() { _ = master.Close(); close(confirming) })
		}
		return out.Write(p)
	})
	err = command.Run(context.Background(), forge.CommandEnv{
		Config: cfg, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Stdout: io.Discard, Stderr: prompts, Stdin: slave,
		LookupEnv: func(string) (string, bool) { return "", false },
	}, []string{"--tenant", "acme", "--email", "owner@example.com"})
	<-confirming
	if err == nil || !strings.Contains(err.Error(), "read the password") {
		t.Fatalf("confirmation hang-up: %v", err)
	}

	// Typing the same password twice creates the owner, who can sign in.
	master, slave = openPTY(t)
	if _, err := master.WriteString(ownerPassword + "\n" + ownerPassword + "\n"); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := run(slave)
	if err != nil {
		t.Fatalf("setup: %v (stderr %q)", err, stderr)
	}
	if strings.Contains(stdout+stderr, ownerPassword) {
		t.Errorf("the password reached the output: %q %q", stdout, stderr)
	}
	repository, err := authpostgres.New(db)
	if err != nil {
		t.Fatal(err)
	}
	login, err := auth.NewLoginService(repository, repository)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := login.Login(context.Background(), auth.LoginInput{Email: "owner@example.com", Password: ownerPassword, TenantSlug: "acme"}); err != nil {
		t.Fatalf("the owner cannot sign in: %v", err)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
