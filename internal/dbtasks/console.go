package dbtasks

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/rocketinsidecorp/forge-go/config"
)

// Console is a prepared database client. Credentials are in Env or in the
// option file Cleanup removes, never in Args.
type Console struct {
	Path string
	Args []string
	Env  []string
	// Cleanup removes temporary credential files; call it when the client exits.
	Cleanup func()
}

// Command returns the client as an exec.Cmd bound to ctx.
func (c *Console) Command(ctx context.Context) *exec.Cmd {
	command := exec.CommandContext(ctx, c.Path, c.Args...) // #nosec G204 -- a vendor client found on PATH, no shell
	command.Env = c.Env
	return command
}

// PrepareConsole builds the command line of the database's own interactive
// client (psql, mysql, sqlite3). extra arguments follow the connection
// options, for example -c "select 1".
func (t *Target) PrepareConsole(extra []string) (*Console, error) {
	if err := t.checkMutable(); err != nil {
		return nil, err
	}
	nothing := func() {}
	switch t.Adapter {
	case config.AdapterPostgres:
		path, err := t.lookPath("psql")
		if err != nil {
			return nil, missingTool("psql", t.Adapter)
		}
		return &Console{Path: path, Args: append([]string{"-X"}, extra...), Env: t.pgEnvironment(environ()), Cleanup: nothing}, nil
	case config.AdapterMySQL:
		path, err := t.lookPath("mysql")
		if err != nil {
			return nil, missingTool("mysql", t.Adapter)
		}
		file, cleanup, err := t.mysqlOptions("mysql", t.Name)
		if err != nil {
			return nil, err
		}
		// The option file must be the first argument.
		return &Console{Path: path, Args: append([]string{mysqlDefaults(file)}, extra...), Env: mysqlEnvironment(environ(), file), Cleanup: cleanup}, nil
	default:
		path, err := t.lookPath("sqlite3")
		if err != nil {
			return nil, missingTool("sqlite3", t.Adapter)
		}
		return &Console{Path: path, Args: append([]string{t.Name}, extra...), Env: environ(), Cleanup: nothing}, nil
	}
}

func missingTool(name string, adapter config.Adapter) error {
	return fmt.Errorf("%s was not found on PATH: install the %s client tools", name, adapter)
}

// mysqlOptions writes the option file (mode 0600, in a new 0700 directory
// that cleanup removes) for the named client tool. Tools read it through
// mysqlDefaults and mysqlEnvironment, so it is the only option file they read.
func (t *Target) mysqlOptions(toolName, database string) (name string, cleanup func(), err error) {
	return t.mysqlOptionsFor(t.mysqlFlavor(toolName), database)
}

// mysqlOptionsFor is mysqlOptions for a client whose flavor is known.
func (t *Target) mysqlOptionsFor(flavor, database string) (name string, cleanup func(), err error) {
	content, err := mysqlOptionFile(credentialsOf(t.priv), database, flavor)
	if err != nil {
		return "", nil, err
	}
	dir, err := os.MkdirTemp("", "forge-mysql-*") // 0700
	if err != nil {
		return "", nil, fmt.Errorf("create option file: %w", err)
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	name = filepath.Join(dir, "client.cnf")
	if err := writeFile(name, []byte(content)); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("write option file: %w", err)
	}
	return name, cleanup, nil
}

// mysqlDefaults is the first argument of every mysql client tool:
// --defaults-file makes the option file the only one read, so a user's
// ~/.my.cnf or the system's /etc/my.cnf cannot change the host, port, user
// or protocol (--defaults-extra-file would let ~/.my.cnf override them).
func mysqlDefaults(optionFile string) string { return "--defaults-file=" + optionFile }

// mysqlEnvironment is the process environment for a client given
// mysqlDefaults(optionFile). MySQL's clients read the login-path file
// (~/.mylogin.cnf) even with --defaults-file, after it; pointing
// MYSQL_TEST_LOGIN_FILE at a name in the option file's private directory,
// which never exists, keeps it out. MariaDB's clients ignore both. An
// inherited MYSQL_PWD (used when the URL has no password) and
// LIBMYSQL_ENABLE_CLEARTEXT_PLUGIN (which lets the client send the password
// unencrypted) are dropped.
func mysqlEnvironment(base []string, optionFile string) []string {
	env := base
	for _, variable := range []string{"MYSQL_TEST_LOGIN_FILE=", "MYSQL_PWD=", "LIBMYSQL_ENABLE_CLEARTEXT_PLUGIN="} {
		env = withoutPrefix(env, variable)
	}
	return append(env, "MYSQL_TEST_LOGIN_FILE="+filepath.Join(filepath.Dir(optionFile), "no-login-path.cnf"))
}

// mysqlFlavor tells MariaDB's client tools from MySQL's by their version
// banner; their TLS options differ. The tools' own name is not a reliable
// sign (mysql is often MariaDB's).
func (t *Target) mysqlFlavor(toolName string) string {
	path, err := t.lookPath(toolName)
	if err != nil {
		return "mysql"
	}
	out, err := exec.Command(path, "--version").Output() // #nosec G204 -- a vendor tool found on PATH
	if err == nil && strings.Contains(strings.ToLower(string(out)), "mariadb") {
		return "mariadb"
	}
	return "mysql"
}
