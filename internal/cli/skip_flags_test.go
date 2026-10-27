package cli

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// `forge new --skip-docker` and `--skip-ci` (forge-go#87), with Forge-Rust's
// names and meaning: --skip-docker writes no Dockerfile, .dockerignore,
// compose.yaml, compose.observability.yaml, docker/ or observability/ (and,
// because it builds the image, no image job in CI and no release workflow);
// --skip-ci writes no .github/. The README, AGENTS.md and the next steps
// change with them. The generated applications are compiled and tested in
// TestGeneratedApplication ("skip docker", "skip ci", "skip docker and ci").

var dockerFiles = []string{"Dockerfile", ".dockerignore", "compose.yaml", "compose.observability.yaml"}

// newSkipApp is `forge new shop ARGS --skip-deps` and its file tree.
func newSkipApp(t *testing.T, stdout *bytes.Buffer, args ...string) map[string]string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "shop")
	var stderr bytes.Buffer
	all := append([]string{"new", "shop", "--module", "example.com/shop", "--dir", dir, "--skip-deps"}, args...)
	if code := Run(all, stdout, &stderr); code != 0 {
		t.Fatalf("forge %v: code=%d %s %s", all, code, stdout.String(), stderr.String())
	}
	return fileTree(t, dir)
}

func hasPrefixIn(tree map[string]string, prefix string) bool {
	for path := range tree {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

func TestNewSkipDockerAndSkipCIOmitTheirFiles(t *testing.T) {
	t.Parallel()
	for _, database := range []string{"sqlite", "postgresql", "mysql", "none"} {
		for _, api := range []bool{false, true} {
			for _, flags := range [][]string{{"--skip-docker"}, {"--skip-ci"}, {"--skip-docker", "--skip-ci"}, nil} {
				name := database + " " + strings.Join(flags, " ")
				if api {
					name += " --api"
				}
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					args := append([]string{"-d", database}, flags...)
					if api {
						args = append(args, "--api")
					}
					var stdout bytes.Buffer
					tree := newSkipApp(t, &stdout, args...)
					skipDocker, skipCI := containsFlag(flags, "--skip-docker"), containsFlag(flags, "--skip-ci")
					for _, path := range dockerFiles {
						if _, has := tree[path]; has == skipDocker {
							t.Errorf("%s present = %v with --skip-docker = %v", path, has, skipDocker)
						}
					}
					if has := hasPrefixIn(tree, "observability/"); has == skipDocker {
						t.Errorf("observability/ present = %v with --skip-docker = %v", has, skipDocker)
					}
					if has := hasPrefixIn(tree, "docker/"); has && skipDocker {
						t.Error("docker/ (the PostgreSQL init script) is generated with --skip-docker")
					}
					if has := hasPrefixIn(tree, ".github/"); has == skipCI {
						t.Errorf(".github/ present = %v with --skip-ci = %v", has, skipCI)
					}
					// The release workflow publishes the container image, so it
					// needs both the Dockerfile and CI.
					if _, has := tree[".github/workflows/release.yml"]; has != (!skipDocker && !skipCI) {
						t.Errorf("release.yml present = %v with --skip-docker = %v, --skip-ci = %v", has, skipDocker, skipCI)
					}
					if skipCI {
						return
					}
					ci := tree[".github/workflows/ci.yml"]
					if hasJob := strings.Contains(ci, "\n  docker:\n"); hasJob == skipDocker {
						t.Errorf("the ci.yml docker job present = %v with --skip-docker = %v", hasJob, skipDocker)
					}
					if skipDocker && (strings.Contains(ci, "compose") || strings.Contains(ci, "Dockerfile") || strings.Contains(ci, "docker/") || strings.Contains(tree[".github/dependabot.yml"], "docker")) {
						t.Errorf("CI still refers to the container assets:\n%s\n%s", ci, tree[".github/dependabot.yml"])
					}
				})
			}
		}
	}
}

func containsFlag(flags []string, flag string) bool {
	for _, f := range flags {
		if f == flag {
			return true
		}
	}
	return false
}

// A README, AGENTS.md and generated source that name a file the flag omitted
// would send the reader to something that does not exist.
func TestNewSkipFlagsChangeTheDocumentation(t *testing.T) {
	t.Parallel()
	for _, database := range []string{"sqlite", "postgresql", "mysql"} {
		t.Run(database, func(t *testing.T) {
			t.Parallel()
			var stdout bytes.Buffer
			tree := newSkipApp(t, &stdout, "-d", database, "--auth", "--skip-docker", "--skip-ci")
			for _, path := range []string{"README.md", "AGENTS.md", "cmd/shop/main.go", ".env.development", ".env.local", "app/javascript/javascript.go"} {
				// The sentences that say the flag removed something are the point.
				var kept []string
				for _, line := range strings.Split(tree[path], "\n") {
					if !strings.Contains(line, "--skip-docker") && !strings.Contains(line, "no compose.yaml") && !strings.Contains(line, "no `compose.yaml`") && !strings.Contains(line, "Without a `compose.yaml`") {
						kept = append(kept, line)
					}
				}
				text := strings.Join(kept, "\n")
				for _, forbidden := range []string{"docker compose", "docker run", "docker build", "Dockerfile", "compose.yaml", "compose.observability", "HEALTHCHECK", "Docker Compose", "Docker images", "docker/postgres", ".github/", "Grafana", "SBOM"} {
					if strings.Contains(text, forbidden) {
						t.Errorf("%s still says %q with --skip-docker --skip-ci", path, forbidden)
					}
				}
			}
			readme := tree["README.md"]
			if !strings.Contains(readme, "forge new shop -d "+database+" --auth --skip-docker --skip-ci") && (database != "sqlite" || !strings.Contains(readme, "forge new shop --auth --skip-docker --skip-ci")) {
				t.Errorf("the quickstart does not repeat the flags:\n%s", readme)
			}
			if !strings.Contains(readme, "No `Dockerfile` was generated (`forge new --skip-docker`)") {
				t.Errorf("the Deploy section does not say there is no Dockerfile")
			}
			if database != "sqlite" && !strings.Contains(readme, "(no compose.yaml was generated)") {
				t.Errorf("the README does not tell to start the %s server itself", database)
			}
			if !strings.Contains(readme, "bin/shop jobs status") || !strings.Contains(readme, "bin/shop migrate") {
				t.Errorf("the README's commands still run in a container:\n%s", readme)
			}
			if strings.Contains(stdout.String(), "docker compose") || strings.Contains(stdout.String(), "Grafana") {
				t.Errorf("the next steps still name Docker:\n%s", stdout.String())
			}
			if database != "sqlite" && !strings.Contains(stdout.String(), "# start a ") {
				t.Errorf("the next steps do not tell to start the database server:\n%s", stdout.String())
			}
		})
	}
	// Each flag alone leaves the other's documentation in place.
	var stdout bytes.Buffer
	tree := newSkipApp(t, &stdout, "-d", "postgresql", "--skip-ci")
	if readme := tree["README.md"]; !strings.Contains(readme, "docker compose up") || strings.Contains(readme, "release.yml") || strings.Contains(readme, "Supply chain") || !strings.Contains(readme, "Observability stack") {
		t.Errorf("--skip-ci README:\n%s", readme)
	}
	if !strings.Contains(stdout.String(), "docker compose up -d --wait postgres") {
		t.Errorf("--skip-ci next steps:\n%s", stdout.String())
	}
	stdout.Reset()
	tree = newSkipApp(t, &stdout, "--skip-docker")
	for _, forbidden := range []string{"docker compose", "docker run", "docker build", "compose.yaml", "Supply chain"} {
		if strings.Contains(tree["README.md"], forbidden) {
			t.Errorf("the --skip-docker README says %q", forbidden)
		}
	}
	if strings.Contains(tree["AGENTS.md"], "compose.yaml") || !strings.Contains(tree["AGENTS.md"], "what CI also runs") {
		t.Errorf("--skip-docker AGENTS.md:\n%s", tree["AGENTS.md"])
	}
}

// forge new --help and forge help --json list both flags with Forge-Rust's names.
func TestSkipFlagsAreDocumentedInHelp(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"help", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("help --json: %d %s", code, stderr.String())
	}
	var catalog struct {
		Commands []struct {
			Name  string `json:"name"`
			Flags []struct {
				Name string `json:"name"`
			} `json:"flags"`
		} `json:"commands"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, command := range catalog.Commands {
		if command.Name == "new" {
			for _, flag := range command.Flags {
				found[flag.Name] = true
			}
		}
	}
	if !found["--skip-docker"] || !found["--skip-ci"] {
		t.Fatalf("forge help --json lists %v for forge new", found)
	}
	stdout.Reset()
	if code := Run([]string{"new", "--help"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "--skip-docker") || !strings.Contains(stdout.String(), "--skip-ci") {
		t.Fatalf("forge new --help: %d\n%s%s", code, stdout.String(), stderr.String())
	}
}
