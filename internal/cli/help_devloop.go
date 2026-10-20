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
