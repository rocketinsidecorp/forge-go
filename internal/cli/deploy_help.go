package cli

import "github.com/rocketinsidecorp/forge-go/internal/deploy"

// deployExitCodes are the exit codes forge deploy, logs and app add to 0
// and 1 (ADR 0030).
var deployExitCodes = []exitCodeDetail{
	{Code: deploy.ExitLockHeld, Meaning: "Another operation holds the deploy lock; nothing was changed."},
	{Code: deploy.ExitUnhealthy, Meaning: "A new container failed its health check; every host serves its previous version."},
	{Code: deploy.ExitMigrationFailed, Meaning: "The migration failed before any container was switched."},
	{Code: deploy.ExitHookFailed, Meaning: "A .forge/hooks executable exited non-zero; the following step did not run."},
	{Code: deploy.ExitInconsistent, Meaning: "A failure could not be fully reverted; hosts may run different versions (see forge deploy details)."},
}

func deployEnvironment() environmentInfo {
	return environmentInfo{
		Files:     []string{deploy.DefaultConfigPath, deploy.DefaultSecretsPath, ".forge/hooks/*", "~/.ssh/config"},
		Variables: []string{"variables referenced as NAME=$VARIABLE in .forge/secrets", "SSH_AUTH_SOCK"},
		Notes:     "Uses the system ssh client (agent, known_hosts and ~/.ssh/config apply; host keys are always verified) and, for builds, the local docker CLI. Secret values are never printed: --dry-run and logs redact them, and containers receive them through an env file written 0600 on each host.",
	}
}

func deployCommonFlags() []flagDetail {
	return []flagDetail{
		{Name: "--config", Value: "PATH", Default: deploy.DefaultConfigPath, Description: "Deploy configuration (-c)."},
		{Name: "--secrets", Value: "PATH", Default: deploy.DefaultSecretsPath, Description: "Secrets file; must be mode 0600."},
		{Name: "--dry-run", Description: "Print the remote and local commands with secrets redacted; read-only queries (docker ps, lock status) still run."},
	}
}

func deployCommandDetails() []commandDetail {
	return []commandDetail{
		{
			Name: "deploy", Usage: "forge deploy [init|setup|rollback VERSION|prune|details|lock status|acquire|release|maintenance on|off] [flags]",
			Summary:     "Build, push and deploy the application image to hosts over SSH (a TypeScript front end is compiled into app/javascript/dist first); health-gated zero-downtime switch through forge-proxy, migrations before the switch, all-or-nothing revert, deploy lock, retained versions for rollback.",
			SideEffects: []string{"remote_write", "subprocess", "network", "database_write", "filesystem_write"},
			Examples: []string{
				"forge deploy init", "forge deploy setup", "forge deploy --dry-run", "forge deploy",
				"forge deploy --skip-build --version 3f2a9c1d07be", "forge deploy rollback 3f2a9c1d07be",
				"forge deploy prune", "forge deploy details --json", "forge deploy lock status", "forge deploy maintenance on",
			},
			Environment: deployEnvironment(),
			ExitCodes:   deployExitCodes,
			Flags: append(deployCommonFlags(),
				flagDetail{Name: "--json", Description: "Print a schema-versioned report on stdout (also on failure); progress goes to stderr."},
				flagDetail{Name: "--version", Value: "TAG", Description: "Image tag to deploy; default is the abbreviated git commit of a clean tree."},
				flagDetail{Name: "--skip-build", Description: "Deploy a tag already in the registry instead of building and pushing."},
				flagDetail{Name: "--message", Value: "TEXT", Description: "lock acquire: reason recorded with the lock."},
				flagDetail{Name: "--force", Description: "lock release: release a lock held by someone else."},
				flagDetail{Name: "--service", Value: "NAME", Description: "init: service name (default: from go.mod)."},
			),
		},
		{
			Name: "logs", Usage: "forge logs [-f] [--role R] [--host H] [--since 1h] [--grep REGEX] [--lines N]",
			Summary:     "Stream docker logs of the current role container from every matching host, each line prefixed with host|; Ctrl-C stops the remote streams.",
			SideEffects: []string{"remote_read", "network"},
			Examples:    []string{"forge logs", "forge logs -f --role jobs", "forge logs --since 1h --grep ERROR --host 192.0.2.10"},
			Environment: deployEnvironment(),
			Flags: append(deployCommonFlags(),
				flagDetail{Name: "--follow", Description: "Keep streaming (-f)."},
				flagDetail{Name: "--role", Value: "ROLE", Description: "Role (default: the proxied role, else web)."},
				flagDetail{Name: "--host", Value: "HOST", Description: "Only this host."},
				flagDetail{Name: "--since", Value: "DURATION", Description: "Only lines since 1h, 30m or a timestamp."},
				flagDetail{Name: "--grep", Value: "REGEX", Description: "Only lines matching this RE2 expression (filtered locally)."},
				flagDetail{Name: "--lines", Value: "N", Default: "100", Description: "Lines per host to start with (10 with --follow)."},
			),
		},
		{
			Name: "app", Usage: "forge app exec [--role R] [--host H] [--interactive] [--reuse] CMD... | forge app shell [--image IMAGE]",
			Summary:     "Run a command in a one-off container of the deployed image (arguments go to the image entrypoint, e.g. migrate status), inside the running container with --reuse, or open a shell; distroless images need shell --image busybox (a sidecar without the app's environment).",
			SideEffects: []string{"remote_write", "network", "application_defined"},
			Examples:    []string{"forge app exec migrate status", "forge app exec --reuse /app/server healthcheck", "forge app exec --interactive --host 192.0.2.10 migrate", "forge app shell --image busybox:1.37"},
			Environment: deployEnvironment(),
			Flags: append(deployCommonFlags(),
				flagDetail{Name: "--role", Value: "ROLE", Description: "Role (default: the proxied role, else web)."},
				flagDetail{Name: "--host", Value: "HOST", Description: "Host (default: the first host of the role)."},
				flagDetail{Name: "--interactive", Description: "exec: allocate a terminal (-i)."},
				flagDetail{Name: "--reuse", Description: "exec: run inside the running container."},
				flagDetail{Name: "--image", Value: "IMAGE", Description: "shell: debug image run as a sidecar sharing network and PID namespaces."},
			),
		},
	}
}
