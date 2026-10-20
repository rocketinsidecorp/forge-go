package dbtasks

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/mysql"
	"github.com/rocketinsidecorp/forge-go/sqldb"
)

// mysqlConnect opens the server with the administration credentials on
// database name, through the adapter, so the connection gets Forge's TLS and
// session rules. information_schema exists on every server and any account
// may use it, so it is the maintenance database.
func (t *Target) mysqlConnect(ctx context.Context, name string) (*sqldb.DB, error) {
	u := *t.priv
	u.Path, u.RawPath = "/"+name, ""
	cfg := t.cfg
	cfg.URL = config.NewSecret(u.String())
	db, err := mysql.Open(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect to the MySQL server: %w", err)
	}
	return db, nil
}

func (t *Target) mysqlExists(ctx context.Context) (bool, error) {
	db, err := t.mysqlConnect(ctx, "information_schema")
	if err != nil {
		return false, err
	}
	defer func() { _ = db.Close() }()
	return mysqlSchemaExists(ctx, db, t.Name)
}

func mysqlSchemaExists(ctx context.Context, db *sqldb.DB, name string) (bool, error) {
	var count int
	err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = ?", name).Scan(&count)
	return count > 0, err
}

// quoteMySQL quotes an identifier; names are also restricted by NewTarget.
func quoteMySQL(name string) string { return "`" + strings.ReplaceAll(name, "`", "``") + "`" }

func (t *Target) mysqlCreate(ctx context.Context) (bool, error) {
	db, err := t.mysqlConnect(ctx, "information_schema")
	if err != nil {
		return false, err
	}
	defer func() { _ = db.Close() }()
	exists, err := mysqlSchemaExists(ctx, db, t.Name)
	if err != nil || exists {
		return false, err
	}
	if _, err := db.ExecContext(ctx, "CREATE DATABASE "+quoteMySQL(t.Name)+" CHARACTER SET utf8mb4"); err != nil {
		return false, fmt.Errorf("create database: %w", err)
	}
	return true, nil
}

func (t *Target) mysqlDrop(ctx context.Context) (bool, error) {
	db, err := t.mysqlConnect(ctx, "information_schema")
	if err != nil {
		return false, err
	}
	defer func() { _ = db.Close() }()
	exists, err := mysqlSchemaExists(ctx, db, t.Name)
	if err != nil || !exists {
		return false, err
	}
	if _, err := db.ExecContext(ctx, "DROP DATABASE IF EXISTS "+quoteMySQL(t.Name)); err != nil {
		return false, fmt.Errorf("drop database: %w", err)
	}
	return true, nil
}

func (t *Target) mysqlAppliedMigrations(ctx context.Context) ([]appliedMigration, error) {
	db, err := t.mysqlConnect(ctx, t.Name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	var tables int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?", migrationsTable).Scan(&tables); err != nil || tables == 0 {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, "SELECT version, name, checksum FROM "+migrationsTable+" ORDER BY version")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var applied []appliedMigration
	for rows.Next() {
		var m appliedMigration
		if err := rows.Scan(&m.version, &m.name, &m.checksum); err != nil {
			return nil, err
		}
		applied = append(applied, m)
	}
	return applied, rows.Err()
}

// mysqlOptionFile renders the [client] option file the mysql tools read the
// connection from, so the password never appears in an argument. flavor is
// "mariadb" or "mysql": they spell the TLS options differently.
func mysqlOptionFile(c credentials, database, flavor string) (string, error) {
	var b strings.Builder
	b.WriteString("[client]\n")
	write := func(key, value string) error {
		if strings.ContainsAny(value, "\n\r\x00") {
			return errors.New("the database URL holds a control character the MySQL option file cannot carry")
		}
		escaped := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value)
		b.WriteString(key + "=\"" + escaped + "\"\n")
		return nil
	}
	for _, kv := range [][2]string{{"user", c.user}, {"password", c.password}, {"host", c.host}, {"port", portOr(c.port, "3306")}, {"database", database}} {
		if kv[1] == "" {
			continue
		}
		if err := write(kv[0], kv[1]); err != nil {
			return "", err
		}
	}
	// protocol=tcp: never a socket, which would ignore host and port.
	b.WriteString("protocol=tcp\n")
	b.WriteString(mysqlTLSOptions(c.params.Get("tls"), flavor))
	return b.String(), nil
}

func portOr(port, fallback string) string {
	if port == "" {
		return fallback
	}
	return port
}

func mysqlTLSOptions(mode, flavor string) string {
	if flavor == "mariadb" {
		switch mode {
		case "true":
			return "ssl=1\nssl-verify-server-cert=1\n"
		case "skip-verify":
			return "ssl=1\n"
		case "false", "":
			return "ssl=0\n"
		}
		return ""
	}
	switch mode {
	case "true":
		return "ssl-mode=VERIFY_IDENTITY\n"
	case "skip-verify":
		return "ssl-mode=REQUIRED\n"
	case "preferred":
		return "ssl-mode=PREFERRED\n"
	}
	return "ssl-mode=DISABLED\n"
}
