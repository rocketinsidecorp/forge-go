package deploy

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ConfigTemplate is written by `forge deploy init`. SERVICE is replaced
// with the application name. It must stay a valid configuration
// (TestInitTemplateIsValid).
const ConfigTemplate = `# forge deploy configuration. Reference: docs/DEPLOYMENT.md#forge-deploy
#
#   forge deploy setup     # once per host: docker check, network, proxy, accessories
#   forge deploy           # build, push, migrate, switch with zero downtime
#   forge deploy rollback VERSION
#
# Every command accepts --dry-run, which prints the remote commands
# (secrets redacted) instead of running them.

# Container names, labels and remote state (~/.forge/SERVICE) use this name.
service: SERVICE

# Repository without tag: the deploy version (git commit, or --version) is
# the tag.
image: registry.example.com/acme/SERVICE

registry:
  server: registry.example.com
  username: acme
  # Name of a secret in .forge/secrets, never the password itself.
  password: REGISTRY_PASSWORD

# Roles and their hosts. "web" is routed through forge-proxy; other roles
# (jobs workers, ...) run the same image with their own cmd.
servers:
  web:
    hosts:
      - 192.0.2.10
  # jobs:
  #   hosts:
  #     - 192.0.2.11
  #   cmd: ["jobs"]
  #   proxy: false

env:
  clear:
    FORGE_ENV: production
  # Names of secrets from .forge/secrets. They reach containers through an
  # env file written 0600 on the host, never through the command line.
  secret:
    - FORGE_DATABASE_URL

# SQLite applications keep their database on a volume:
# volumes:
#   - SERVICE-storage:/app/storage

proxy:
  # Build and push it once from the Forge repository:
  #   docker build -f cmd/forge-proxy/Dockerfile -t registry.example.com/acme/forge-proxy:0.1.0 .
  # then pin the pushed digest: registry.example.com/acme/forge-proxy@sha256:...
  image: registry.example.com/acme/forge-proxy:0.1.0
  hosts:
    - app.example.com
  # Let's Encrypt certificates (accepts the Let's Encrypt terms). Point the
  # DNS records at the web hosts first.
  ssl: true
  # acme_email: ops@example.com
  app_port: 8080
  healthcheck:
    path: /health/ready
    interval: 1s
    timeout: 2s
    healthy_count: 3
    deploy_timeout: 60s
  drain_timeout: 30s

# Runs "<image> migrate" once, on the first web host, before any switch.
migrate:
  enabled: true

# accessories:
#   db:
#     image: postgres:18-alpine
#     host: 192.0.2.20
#     port: 5432            # published on 127.0.0.1 only
#     volumes:
#       - SERVICE-postgres:/var/lib/postgresql
#     env:
#       secret:
#         - POSTGRES_PASSWORD

builder:
  context: .
  dockerfile: Dockerfile
  # platform: linux/amd64

retain: 5            # stopped versions kept per role for rollback
stop_timeout: 35s    # SIGTERM grace; above FORGE_HTTP_SHUTDOWN_TIMEOUT

ssh:
  user: deploy
  # port: 22
  # proxy_jump: bastion.example.com
`

// SecretsTemplate is written to .forge/secrets with mode 0600. It holds
// only $VARIABLE references.
//
// #nosec G101 -- no credentials: the values are environment references.
const SecretsTemplate = `# Secrets for forge deploy. This file is listed in .gitignore and
# .dockerignore; never commit it.
#
#   NAME=value      literal value (quotes are removed, nothing is expanded)
#   NAME=$VARIABLE  copied from your local environment at deploy time, so the
#                   value can come from a password manager or CI secret store
#
# Command substitution is not supported.
REGISTRY_PASSWORD=$REGISTRY_PASSWORD
FORGE_DATABASE_URL=$FORGE_DATABASE_URL
`

// Init writes config/deploy.yml and .forge/secrets under root and adds
// .forge/secrets to .gitignore and .forge to .dockerignore. Existing files
// are never overwritten. It returns the paths it created or changed.
func Init(root, service string) ([]string, error) {
	if !serviceNamePattern.MatchString(service) {
		return nil, fmt.Errorf("service name %q must match %s (pass --service)", service, serviceNamePattern)
	}
	configPath := filepath.Join(root, DefaultConfigPath)
	secretsPath := filepath.Join(root, DefaultSecretsPath)
	for _, path := range []string{configPath, secretsPath} {
		if _, err := os.Stat(path); err == nil {
			return nil, fmt.Errorf("%s already exists; not overwriting", path)
		}
	}
	var changed []string
	if err := os.MkdirAll(filepath.Dir(configPath), 0o750); err != nil {
		return nil, err
	}
	if err := writeNew(configPath, strings.ReplaceAll(ConfigTemplate, "SERVICE", service)); err != nil {
		return nil, err
	}
	changed = append(changed, DefaultConfigPath)
	if err := os.MkdirAll(filepath.Dir(secretsPath), 0o700); err != nil {
		return changed, err
	}
	if err := writeNew(secretsPath, SecretsTemplate); err != nil {
		return changed, err
	}
	changed = append(changed, DefaultSecretsPath)
	for _, entry := range []struct{ file, line string }{{".gitignore", ".forge/secrets"}, {".dockerignore", ".forge"}} {
		added, err := ensureLine(filepath.Join(root, entry.file), entry.line)
		if err != nil {
			return changed, err
		}
		if added {
			changed = append(changed, entry.file)
		}
	}
	return changed, nil
}

func writeNew(path, content string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- fixed names under the project root.
	if err != nil {
		return err
	}
	if _, err := file.WriteString(content); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// ensureLine appends line to the ignore file at path unless present.
func ensureLine(path, line string) (bool, error) {
	content, err := os.ReadFile(path) // #nosec G304 -- fixed names under the project root.
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	for existing := range strings.Lines(string(content)) {
		trimmed := strings.TrimSpace(existing)
		if trimmed == line || trimmed == "/"+line || trimmed == line+"/" {
			return false, nil
		}
	}
	text := string(content)
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	text += "# forge deploy\n" + line + "\n"
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil { // #nosec G306,G703 -- fixed ignore-file names under the project root; 0600 matches generated files.
		return false, err
	}
	return true, nil
}
