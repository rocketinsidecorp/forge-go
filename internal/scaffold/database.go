package scaffold

import (
	"fmt"
	"hash/fnv"
	"strings"
)

// Databases lists the values Options.Database accepts, in the order the CLI
// documents them. The empty string means no database.
var Databases = []string{"postgresql", "mysql", "mariadb", "sqlite"}

var databaseAliases = map[string]string{
	"postgresql": "postgresql", "postgres": "postgresql", "pg": "postgresql",
	"mysql":   "mysql",
	"mariadb": "mariadb",
	"sqlite":  "sqlite", "sqlite3": "sqlite",
	"none": "", "": "",
}

// normalizeDatabase returns the canonical database name.
func normalizeDatabase(name string) (string, error) {
	canonical, ok := databaseAliases[strings.ToLower(name)]
	if !ok {
		return "", fmt.Errorf("unsupported database %q: use %s or none", name, strings.Join(Databases, ", "))
	}
	return canonical, nil
}

// Credentials in the profiles below ("development", "forge") belong to
// throwaway containers: local compose services published on 127.0.0.1 only
// and CI service containers. They are not secrets, and the generated
// .env.development says real secrets never go there.

// databaseProfile is everything the templates need to know about one
// database. Server databases get a compose service and a CI service
// container; SQLite lives in a file on a volume.
type databaseProfile struct {
	Name        string // canonical name, as passed to --database
	Title       string // human-readable name and version
	Adapter     string // Forge adapter package: postgres, mysql or sqlite
	TestPackage string // its test helper package
	Server      bool
	Identity    bool // generated Forge identity wiring is available
	Jobs        bool // generated Forge jobs wiring is available

	// Server databases only.
	Service       string      // compose service and host name
	Image         string      // shared by compose and CI; pinned by digest, the framework CI's pin
	ContainerPort int         // port inside the container
	VolumePath    string      // data directory inside the container
	Env           [][2]string // compose environment of the server
	Healthcheck   string      // compose healthcheck test, as a YAML flow sequence
	CIEnv         [][2]string // CI service environment
	CIHealthCmd   string      // CI service --health-cmd
	CITestURL     string      // FORGE_TEST_DATABASE_URL in CI
	// ProductionTLS is the FORGE_DATABASE_URL setting production requires.
	ProductionTLS string
	// MigrationLock describes how concurrent migrate runs are serialized.
	MigrationLock string
}

func profileFor(name, user, database string) *databaseProfile {
	switch name {
	case "postgresql":
		return &databaseProfile{ // #nosec G101 -- throwaway container credentials, see above
			Name: name, Title: "PostgreSQL 18", Adapter: "postgres", TestPackage: "postgrestest", Server: true, Identity: true, Jobs: true,
			ProductionTLS: "`sslmode=verify-full`, or `verify-ca` with `sslrootcert`",
			MigrationLock: "a PostgreSQL advisory lock",
			Service:       "postgres", Image: "postgres:18-alpine@sha256:77f585114c32fbca283dc835b0596f4e52b51b4c6662d7810b2f4084f60a1873", ContainerPort: 5432,
			// The PostgreSQL 18 image keeps its data in a versioned
			// subdirectory of /var/lib/postgresql.
			VolumePath: "/var/lib/postgresql",
			// The image's superuser creates the database and the
			// app_migrator and app_runtime roles (docker/postgres/init.sql).
			Env: [][2]string{
				{"POSTGRES_USER", "postgres"}, {"POSTGRES_PASSWORD", "development"}, {"POSTGRES_DB", "postgres"},
			},
			// TCP: while init.sql runs, the server listens on its socket
			// only, so the check passes once the roles and database exist.
			Healthcheck: fmt.Sprintf(`["CMD-SHELL", "pg_isready -h 127.0.0.1 -U postgres -d %s"]`, database),
			CIEnv:       [][2]string{{"POSTGRES_USER", "forge"}, {"POSTGRES_PASSWORD", "forge"}},
			CIHealthCmd: "pg_isready -U forge",
			CITestURL:   "postgres://forge:forge@127.0.0.1:5432/postgres?sslmode=disable",
		}
	case "mysql":
		return &databaseProfile{ // #nosec G101 -- throwaway container credentials, see above
			Name: name, Title: "MySQL 8.4", Adapter: "mysql", TestPackage: "mysqltest", Server: true, Jobs: true,
			ProductionTLS: "`tls=true`", MigrationLock: "a MySQL named lock (GET_LOCK)",
			Service: "mysql", Image: "mysql:8.4@sha256:0f51e63e537a1f176423fa03ef559aabe6fa4372a53160a44bc7e75da7d15364", ContainerPort: 3306, VolumePath: "/var/lib/mysql",
			Env: [][2]string{
				{"MYSQL_ROOT_PASSWORD", "development"}, {"MYSQL_DATABASE", database},
				{"MYSQL_USER", user}, {"MYSQL_PASSWORD", "development"},
			},
			// During initialization the server listens on a socket only, so a
			// TCP ping succeeds only once it is really ready.
			Healthcheck: `["CMD", "mysqladmin", "ping", "-h", "127.0.0.1", "-uroot", "-pdevelopment"]`,
			CIEnv:       [][2]string{{"MYSQL_ROOT_PASSWORD", "forge"}},
			CIHealthCmd: "mysqladmin ping -h 127.0.0.1 -uroot -pforge",
			CITestURL:   "mysql://root:forge@127.0.0.1:3306/mysql",
		}
	case "mariadb":
		return &databaseProfile{ // #nosec G101 -- throwaway container credentials, see above
			Name: name, Title: "MariaDB 11.8", Adapter: "mysql", TestPackage: "mysqltest", Server: true, Jobs: true,
			ProductionTLS: "`tls=true`", MigrationLock: "a MariaDB named lock (GET_LOCK)",
			Service: "mariadb", Image: "mariadb:11.8@sha256:f16d7a344413e519dff518f27bdb221388f84b0d274c97c4fb5f2cb0dc1b34fa", ContainerPort: 3306, VolumePath: "/var/lib/mysql",
			Env: [][2]string{
				{"MARIADB_ROOT_PASSWORD", "development"}, {"MARIADB_DATABASE", database},
				{"MARIADB_USER", user}, {"MARIADB_PASSWORD", "development"},
			},
			Healthcheck: `["CMD", "healthcheck.sh", "--connect", "--innodb_initialized"]`,
			CIEnv:       [][2]string{{"MARIADB_ROOT_PASSWORD", "forge"}},
			CIHealthCmd: "healthcheck.sh --connect --innodb_initialized",
			CITestURL:   "mysql://root:forge@127.0.0.1:3306/mysql",
		}
	case "sqlite":
		return &databaseProfile{
			Name: name, Title: "SQLite", Adapter: "sqlite", TestPackage: "sqlitetest",
			MigrationLock: "an operating-system lock on a sidecar file (`<database>-migrate.lock`) on the same host",
		}
	default:
		return nil
	}
}

// databaseURLs returns the connection URLs for inside compose, for the host
// (.env.development) and for tests on the host.
func databaseURLs(profile *databaseProfile, user, database string, hostPort int) (app, development, test string) {
	switch profile.Adapter {
	case "postgres":
		// The application serves as the runtime role. Tests connect as the
		// image's superuser, which can create their databases and roles.
		app = postgresURL(runtimeRole, "postgres:5432", database)
		development = postgresURL(runtimeRole, fmt.Sprintf("127.0.0.1:%d", hostPort), database)
		return app, development, postgresURL("postgres", fmt.Sprintf("127.0.0.1:%d", hostPort), "postgres")
	case "mysql":
		app = fmt.Sprintf("mysql://%s:development@%s:3306/%s", user, profile.Service, database)
		development = fmt.Sprintf("mysql://%s:development@127.0.0.1:%d/%s", user, hostPort, database)
		// Only root may create the throwaway test databases.
		test = fmt.Sprintf("mysql://root:development@127.0.0.1:%d/%s", hostPort, database)
		return app, development, test
	default: // sqlite: tests use temporary files and need no URL.
		return "sqlite:///app/storage/development.db", "sqlite:storage/development.db", ""
	}
}

// The development PostgreSQL roles docker/postgres/init.sql creates, with
// their names as passwords, as Forge-Rust's: the migration role owns the
// schema, the runtime role is what the application serves with.
const (
	migratorRole = "app_migrator"
	runtimeRole  = "app_runtime"
)

// postgresURL is a development PostgreSQL URL. The image's superuser has
// the password "development", the roles of init.sql their own names.
func postgresURL(role, address, database string) string {
	password := role
	if role == "postgres" {
		password = "development"
	}
	return fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable", role, password, address, database)
}

// migrationURLs returns FORGE_MIGRATION_DATABASE_URL inside compose and on
// the host: the migration role for PostgreSQL, and none for the other
// databases, which serve and migrate as one user.
func migrationURLs(profile *databaseProfile, database string, hostPort int) (app, development string) {
	if profile.Adapter != "postgres" {
		return "", ""
	}
	return postgresURL(migratorRole, "postgres:5432", database), postgresURL(migratorRole, fmt.Sprintf("127.0.0.1:%d", hostPort), database)
}

// databaseIdentifiers derives the database user and name from the
// application name, within MySQL's 32-character user and PostgreSQL's
// 63-byte identifier limits.
func databaseIdentifiers(name string) (user, database string) {
	identifier := strings.ReplaceAll(name, "-", "_")
	user = identifier[:min(len(identifier), 32)]
	// The MySQL and MariaDB images refuse to create a second "root" user.
	if user == "root" {
		user = "root_app"
	}
	return user, identifier[:min(len(identifier), 51)] + "_development"
}

// dbHostPort maps name into 20000-29999, so the published database port
// neither clashes with a locally installed server on its default port nor,
// usually, with other Forge applications.
func dbHostPort(name string) int {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(name)) // hash.Hash writes never fail
	return 20000 + int(hash.Sum32()%10000)
}
