package dbtasks

import (
	"context"
	"fmt"
	"os"
	"os/exec"
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
		return &Console{Path: path, Args: append([]string{"--defaults-extra-file=" + file}, extra...), Env: environ(), Cleanup: cleanup}, nil
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

// mysqlOptions writes the option file (mode 0600, removed by cleanup) for
// the named client tool.
func (t *Target) mysqlOptions(toolName, database string) (name string, cleanup func(), err error) {
	flavor := t.mysqlFlavor(toolName)
	content, err := mysqlOptionFile(credentialsOf(t.priv), database, flavor)
	if err != nil {
		return "", nil, err
	}
	file, err := os.CreateTemp("", "forge-mysql-*.cnf") // 0600
	if err != nil {
		return "", nil, fmt.Errorf("create option file: %w", err)
	}
	cleanup = func() { _ = os.Remove(file.Name()) }
	if _, err := file.WriteString(content); err != nil {
		_ = file.Close()
		cleanup()
		return "", nil, fmt.Errorf("write option file: %w", err)
	}
	if err := file.Close(); err != nil {
		cleanup()
		return "", nil, err
	}
	return file.Name(), cleanup, nil
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
