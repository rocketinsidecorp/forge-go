package deploy

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const validConfig = `
service: shop
image: registry.example.com/acme/shop
registry:
  server: registry.example.com
  username: acme
  password: REGISTRY_PASSWORD
servers:
  web:
    hosts: [192.0.2.10, 192.0.2.11]
  jobs:
    hosts: [192.0.2.11]
    cmd: [jobs]
    env:
      clear:
        WORKERS: "4"
env:
  clear:
    FORGE_ENV: production
  secret: [FORGE_DATABASE_URL]
volumes: ["shop-storage:/app/storage"]
proxy:
  image: registry.example.com/forge-proxy@sha256:1111111111111111111111111111111111111111111111111111111111111111
  hosts: [Shop.Example.com]
  ssl: true
  healthcheck:
    interval: 500ms
accessories:
  db:
    image: postgres:18-alpine
    host: 192.0.2.20
    port: 5432
    volumes: ["shop-pg:/var/lib/postgresql"]
    env:
      secret: [POSTGRES_PASSWORD]
builder:
  platform: linux/amd64
  args:
    GO_VERSION: "1.27"
retain: 3
ssh:
  user: deploy
  port: 2222
  proxy_jump: ops@bastion.example.com
`

func TestParseConfigAppliesDefaults(t *testing.T) {
	cfg, err := ParseConfig([]byte(validConfig))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PrimaryRole() != "web" || !cfg.Proxied("web") || cfg.Proxied("jobs") {
		t.Fatalf("roles: primary=%s", cfg.PrimaryRole())
	}
	if got := cfg.Roles(); !slices.Equal(got, []string{"web", "jobs"}) {
		t.Fatalf("Roles = %v", got)
	}
	if got := cfg.AppHosts(); !slices.Equal(got, []string{"192.0.2.10", "192.0.2.11"}) {
		t.Fatalf("AppHosts = %v", got)
	}
	if got := cfg.AllHosts(); !slices.Equal(got, []string{"192.0.2.10", "192.0.2.11", "192.0.2.20"}) {
		t.Fatalf("AllHosts = %v", got)
	}
	if got := cfg.RolesOn("192.0.2.11"); !slices.Equal(got, []string{"web", "jobs"}) {
		t.Fatalf("RolesOn = %v", got)
	}
	if got := cfg.SecretNames(); !slices.Equal(got, []string{"FORGE_DATABASE_URL", "POSTGRES_PASSWORD", "REGISTRY_PASSWORD"}) {
		t.Fatalf("SecretNames = %v", got)
	}
	p := cfg.Proxy
	if p.Hosts[0] != "shop.example.com" || p.AppPort != 8080 || p.Healthcheck.Path != "/health/ready" ||
		p.Healthcheck.Interval.D() != 500*time.Millisecond || p.Healthcheck.HealthyCount != 3 || p.DrainTimeout.D() != 30*time.Second {
		t.Fatalf("proxy defaults = %+v", p)
	}
	if cfg.Retain != 3 || cfg.StopTimeout.D() != 35*time.Second || cfg.Parallelism != 4 || cfg.Hooks != ".forge/hooks" ||
		!*cfg.Migrate.Enabled || cfg.Migrate.Role != "web" || !slices.Equal(cfg.Migrate.Cmd, []string{"migrate"}) ||
		cfg.Builder.Context != "." || cfg.Builder.Dockerfile != "Dockerfile" {
		t.Fatalf("defaults = %+v", cfg)
	}
	if cfg.Accessories["db"].Port != "5432" {
		t.Fatalf("integer port not read as string: %q", cfg.Accessories["db"].Port)
	}
}

func TestParseConfigRejectsInvalidConfigurations(t *testing.T) {
	replace := func(old, new string) string {
		if !strings.Contains(validConfig, old) {
			t.Fatalf("fixture lacks %q", old)
		}
		return strings.Replace(validConfig, old, new, 1)
	}
	cases := map[string]struct {
		config string
		want   string
	}{
		"empty":                {"", "empty"},
		"unknown key":          {validConfig + "colour: blue\n", "field colour not found"},
		"unknown nested key":   {replace("  ssl: true", "  ssl: true\n  tls_mode: strict"), "field tls_mode not found"},
		"duplicate key":        {validConfig + "retain: 4\n", "already defined"},
		"two documents":        {validConfig + "---\nservice: other\n", "exactly one YAML document"},
		"bad service":          {replace("service: shop", "service: Shop_App"), "service must match"},
		"image with tag":       {replace("image: registry.example.com/acme/shop", "image: registry.example.com/acme/shop:latest"), "must not include a tag"},
		"image with digest":    {replace("image: registry.example.com/acme/shop", "image: registry.example.com/acme/shop@sha256:abc"), "image:"},
		"password inline":      {replace("password: REGISTRY_PASSWORD", "password: hunter2!"), "must name a secret"},
		"user without pass":    {replace("  password: REGISTRY_PASSWORD\n", ""), "must be set together"},
		"no servers":           {replace("servers:", "unused_servers:"), "field unused_servers not found"},
		"role without hosts":   {replace("    hosts: [192.0.2.11]\n    cmd: [jobs]", "    cmd: [jobs]"), "servers.jobs.hosts must list"},
		"option injection":     {replace("hosts: [192.0.2.10, 192.0.2.11]", "hosts: [-oProxyCommand=evil]"), "not a valid host"},
		"duplicate host":       {replace("hosts: [192.0.2.10, 192.0.2.11]", "hosts: [192.0.2.10, 192.0.2.10]"), "listed twice"},
		"two proxy roles":      {replace("    cmd: [jobs]", "    cmd: [jobs]\n    proxy: true"), "only one role"},
		"bad env key":          {replace("FORGE_ENV: production", "FORGE-ENV: production"), "not a valid variable name"},
		"clear and secret":     {replace("FORGE_ENV: production", "FORGE_DATABASE_URL: plain"), "both clear and secret"},
		"bad volume":           {replace(`volumes: ["shop-storage:/app/storage"]`, `volumes: ["../etc:/etc"]`), "volumes"},
		"proxy without image":  {replace("  image: registry.example.com/forge-proxy@sha256:1111111111111111111111111111111111111111111111111111111111111111\n", ""), "proxy.image is required"},
		"ssl without hosts":    {replace("  hosts: [Shop.Example.com]\n", ""), "proxy.ssl requires"},
		"ip proxy host":        {replace("hosts: [Shop.Example.com]", "hosts: [192.0.2.10]"), "not a DNS name"},
		"numeric duration":     {replace("interval: 500ms", "interval: 5"), "duration must be a string"},
		"bad duration":         {replace("interval: 500ms", "interval: soon"), "invalid duration"},
		"negative retain":      {replace("retain: 3", "retain: -1"), "retain must be"},
		"health path":          {replace("interval: 500ms", "interval: 500ms\n    path: health?x=1"), "healthcheck.path"},
		"migrate role":         {validConfig + "migrate:\n  role: worker\n", "migrate.role"},
		"accessory port":       {replace("port: 5432", "port: 99999"), "not a port number"},
		"accessory bind":       {replace("port: 5432", `port: "public:1:2"`), "bind address"},
		"accessory host":       {replace("host: 192.0.2.20", "host: ''"), "accessories.db.host"},
		"platform":             {replace("platform: linux/amd64", "platform: amd64"), "builder.platform"},
		"ssh user":             {replace("user: deploy", "user: 'root; rm'"), "ssh.user"},
		"proxy jump":           {replace("proxy_jump: ops@bastion.example.com", "proxy_jump: '-oProxyCommand=x'"), "ssh.proxy_jump"},
		"parallelism":          {validConfig + "parallelism: 500\n", "parallelism"},
		"stop timeout":         {validConfig + "stop_timeout: 1h\n", "stop_timeout"},
		"proxy unused":         {replace("  web:\n", "  web:\n    proxy: false\n"), "no role uses it"},
		"control char in cmd":  {replace("cmd: [jobs]", `cmd: ["jobs\n"]`), "control character"},
		"multiline clear env":  {replace(`WORKERS: "4"`, `WORKERS: "4\n5"`), "single line"},
		"duplicate secret":     {replace("secret: [FORGE_DATABASE_URL]", "secret: [FORGE_DATABASE_URL, FORGE_DATABASE_URL]"), "listed twice"},
		"wrong type for hosts": {replace("hosts: [192.0.2.10, 192.0.2.11]", "hosts: 192.0.2.10"), "cannot unmarshal"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseConfig([]byte(tc.config))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestPublishSpecBindsLoopbackByDefault(t *testing.T) {
	cases := map[string]string{
		"5432":                "127.0.0.1:5432:5432",
		"6543:5432":           "127.0.0.1:6543:5432",
		"10.0.0.5:6543:5432":  "10.0.0.5:6543:5432",
		"0.0.0.0:6379:6379":   "0.0.0.0:6379:6379",
		"[::1]:1:2":           "",
		"5432:5432:5432:5432": "",
		"x":                   "",
	}
	for input, want := range cases {
		got, err := publishSpec(input)
		if (want == "") != (err != nil) || got != want {
			t.Errorf("publishSpec(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
}

func TestInitWritesTemplatesOnceAndIgnoresSecrets(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("bin/"), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := Init(root, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(changed, []string{DefaultConfigPath, DefaultSecretsPath, ".gitignore", ".dockerignore"}) {
		t.Fatalf("changed = %v", changed)
	}
	cfg, err := LoadConfig(filepath.Join(root, DefaultConfigPath))
	if err != nil {
		t.Fatalf("generated config is invalid: %v", err)
	}
	if cfg.Service != "shop" || cfg.Image != "registry.example.com/acme/shop" {
		t.Fatalf("service not substituted: %+v", cfg)
	}
	info, err := os.Stat(filepath.Join(root, DefaultSecretsPath))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("secrets mode %v %v", info, err)
	}
	gitignore, _ := os.ReadFile(filepath.Join(root, ".gitignore"))
	dockerignore, _ := os.ReadFile(filepath.Join(root, ".dockerignore"))
	if string(gitignore) != "bin/\n# forge deploy\n.forge/secrets\n" || !strings.Contains(string(dockerignore), ".forge\n") {
		t.Fatalf("ignore files: %q %q", gitignore, dockerignore)
	}
	if _, err := Init(root, "shop"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second init: %v", err)
	}
	if _, err := Init(t.TempDir(), "Bad Name"); err == nil {
		t.Fatal("invalid service accepted")
	}
	// The secrets template resolves through the environment.
	secrets, err := LoadSecrets(filepath.Join(root, DefaultSecretsPath), func(name string) (string, bool) { return "value-of-" + name, true })
	if err != nil {
		t.Fatal(err)
	}
	if err := secrets.Require(cfg.SecretNames()); err != nil {
		t.Fatalf("template secrets do not cover the template config: %v", err)
	}
}

func TestDefaultServiceName(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module github.com/acme/Shop_Front\n\ngo 1.27\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := defaultServiceName(root); got != "shop-front" {
		t.Fatalf("defaultServiceName = %q", got)
	}
	numeric := filepath.Join(t.TempDir(), "42")
	if err := os.Mkdir(numeric, 0o750); err != nil {
		t.Fatal(err)
	}
	if got := defaultServiceName(numeric); got != "app-42" {
		t.Fatalf("defaultServiceName(42) = %q", got)
	}
}

// The configuration documented in docs/DEPLOYMENT.md must stay valid.
func TestDocumentedConfigIsValid(t *testing.T) {
	content, err := os.ReadFile(filepath.Join("..", "..", "docs", "DEPLOYMENT.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, rest, found := strings.Cut(string(content), "### config/deploy.yml\n\n```yaml\n")
	if !found {
		t.Fatal("docs/DEPLOYMENT.md lacks the config/deploy.yml example")
	}
	example, _, _ := strings.Cut(rest, "```")
	cfg, err := ParseConfig([]byte(example))
	if err != nil {
		t.Fatalf("documented config is invalid: %v", err)
	}
	if cfg.Service != "shop" || len(cfg.Accessories) != 1 || cfg.SSH.ProxyJump == "" {
		t.Fatalf("documented config parsed unexpectedly: %+v", cfg)
	}
}

func TestProxiedRoleTrustsTheForgeNetworkUnlessConfigured(t *testing.T) {
	cfg, err := ParseConfig([]byte(validConfig))
	if err != nil {
		t.Fatal(err)
	}
	cmds := commands{cfg: cfg}
	web := cmds.runRole("web", "v1", "shop-web-v1-a").Script
	if !strings.HasPrefix(web, "forge_proxies=$(docker network inspect --format ") ||
		!strings.Contains(web, ` --env "FORGE_HTTP_TRUSTED_PROXIES=$forge_proxies" `) {
		t.Fatalf("proxied role does not trust the forge network:\n%s", web)
	}
	if !strings.Contains(web, `[ -n "$forge_proxies" ] || {`) {
		t.Fatalf("an empty subnet list is not refused:\n%s", web)
	}
	if jobs := cmds.runRole("jobs", "v1", "shop-jobs-v1-a").Script; strings.Contains(jobs, "FORGE_HTTP_TRUSTED_PROXIES") || strings.Contains(jobs, "forge_proxies") {
		t.Fatalf("role without the proxy trusts proxies:\n%s", jobs)
	}

	for name, set := range map[string]func(*Config){
		"global clear": func(c *Config) { c.Env.Clear[TrustedProxiesEnv] = "10.0.0.0/8" },
		"role clear": func(c *Config) {
			role := c.Servers["web"]
			role.Env.Clear = map[string]string{TrustedProxiesEnv: ""}
			c.Servers["web"] = role
		},
		"secret": func(c *Config) { c.Env.Secret = append(c.Env.Secret, TrustedProxiesEnv) },
	} {
		cfg, err := ParseConfig([]byte(validConfig))
		if err != nil {
			t.Fatal(err)
		}
		set(cfg)
		script := commands{cfg: cfg}.runRole("web", "v1", "shop-web-v1-a").Script
		if strings.Contains(script, "forge_proxies") {
			t.Errorf("%s: an explicit %s is overridden:\n%s", name, TrustedProxiesEnv, script)
		}
	}
}
