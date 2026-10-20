package cli

// devCommandDetail is the catalog entry of `forge dev` (ADR 0035).
func devCommandDetail() commandDetail {
	environment := runtimeEnvironment()
	environment.Notes = "Loads .env.development and .env.local (process environment wins) and sets FORGE_ENV=development when unset. With FORGE_ENV=development and FORGE_HTTP_TRANSPORT=plain, forge itself listens on FORGE_HTTP_ADDR as a development-only reverse proxy and runs the application on a private 127.0.0.1 port; the proxy injects a reload script into text/html responses, serves it and an event stream under /__forge/, and adds a per-response nonce to the page's Content-Security-Policy only if the policy would block that one script. Nothing is compiled into the application. The watcher polls Go sources, templates, assets, migrations and the two env files; env-file changes restart without rebuilding. In any other environment or transport the application restarts on change without the proxy."
	return commandDetail{
		Name: "dev", Usage: "forge dev [NAME] [--no-reload] [--poll-interval D] [--debounce D]",
		Summary:     "Run the application in development mode: rebuild, gracefully restart and reload the browser when source, templates, assets, migrations or env files change.",
		SideEffects: []string{"subprocess", "filesystem_read", "network_listen", "application_defined"},
		Examples:    []string{"forge dev", "forge dev api", "forge dev --poll-interval 1s", "forge dev --no-reload"},
		Environment: environment,
		Flags: []flagDetail{
			{Name: "--no-reload", Description: "Build and run once, as before: no file watching, restarts or browser reload."},
			{Name: "--poll-interval", Value: "DURATION", Default: "500ms", Description: "How often the source tree is scanned for changes (50ms to 1m)."},
			{Name: "--debounce", Value: "DURATION", Default: "300ms", Description: "Quiet time after a change before rebuilding, so one save causes one rebuild."},
		},
	}
}

// dbCommandDetail is the catalog entry of `forge db` (ADR 0035).
func dbCommandDetail() commandDetail {
	environment := runtimeEnvironment()
	environment.Variables = append(environment.Variables, "FORGE_MIGRATION_DATABASE_URL", "FORGE_ALLOW_DESTRUCTIVE")
	environment.Notes = "Loads .env.development and .env.local (process environment wins). create/drop connect to the server's maintenance database with FORGE_MIGRATION_DATABASE_URL when set (a privileged role; its database is ignored) or FORGE_DATABASE_URL; the target database is always the one in FORGE_DATABASE_URL. migrate, status, rollback and seed build the application and run its own commands. schema:dump/schema:load and console run pg_dump/psql, mysqldump/mysql or sqlite3 from PATH with credentials in PG* environment variables or a 0600 option file, never in arguments. Names reserved by the server (postgres, template0, template1, mysql, sys, ...) are never created, dropped, loaded into or opened in the console. drop, reset, schema:load and rollback are refused unless the database is local (loopback, localhost, a Unix socket or a SQLite file) and FORGE_ENV is development or test: production, staging and any other server need --force-production plus either a person typing the database name at a terminal, or --yes with FORGE_ALLOW_DESTRUCTIVE=true in the process environment (an env file cannot grant it)."
	return commandDetail{
		Name: "db", Usage: "forge db <create|drop|reset|setup|prepare|migrate|rollback|status|seed|schema:dump|schema:load|console> [flags]",
		Summary:     "Database lifecycle like Rails' db:*: create, drop, reset, setup (create + migrate + seed), prepare (setup if missing, else migrate), migrate, rollback, status, seed, schema dump/load (db/schema.sql) and the database's own console. drop, reset, schema:load and rollback destroy or change data.",
		SideEffects: []string{"database_write", "database_destructive", "filesystem_write", "subprocess", "interactive_input", "network_optional"},
		Examples: []string{
			"forge db setup", "forge db prepare", "forge db migrate", "forge db status", "forge db rollback -steps 2",
			"forge db reset", "forge db schema:dump", "forge db schema:load", "forge db console", "forge db console -- -c 'select 1'",
			"forge db drop --force-production --yes",
		},
		Environment: environment,
		Flags: []flagDetail{
			{Name: "--force-production", Description: "drop, reset, schema:load, rollback: allow the command on a protected database (production, staging or a non-local server), after confirmation."},
			{Name: "--yes", Description: "With --force-production: confirm without a prompt; requires FORGE_ALLOW_DESTRUCTIVE=true in the process environment."},
			{Name: "-steps", Value: "N", Default: "1", Description: "rollback: number of application migrations to revert; Forge framework migrations are never reverted."},
			{Name: "--", Description: "console: arguments after it go to the database client."},
		},
	}
}
