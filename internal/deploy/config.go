// Package deploy implements `forge deploy`, `forge logs` and `forge app`:
// container deployment to hosts over SSH with zero-downtime switches through
// forge-proxy (ADR 0030).
//
// Every remote action is a [Command] executed by a [Runner]. The production
// runner is the system ssh client; tests use a recording fake or real sshd
// with a docker shim, and --dry-run prints commands instead of running
// them.
package deploy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// DefaultConfigPath is where `forge deploy` looks for its configuration.
const DefaultConfigPath = "config/deploy.yml"

// Config is config/deploy.yml.
type Config struct {
	Service     string               `yaml:"service"`
	Image       string               `yaml:"image"`
	Registry    Registry             `yaml:"registry"`
	Servers     map[string]Role      `yaml:"servers"`
	Env         Env                  `yaml:"env"`
	Volumes     []string             `yaml:"volumes"`
	Proxy       Proxy                `yaml:"proxy"`
	Migrate     Migrate              `yaml:"migrate"`
	Accessories map[string]Accessory `yaml:"accessories"`
	Builder     Builder              `yaml:"builder"`
	Retain      int                  `yaml:"retain"`
	StopTimeout Duration             `yaml:"stop_timeout"`
	BootTimeout Duration             `yaml:"boot_timeout"`
	Hooks       string               `yaml:"hooks"`
	SSH         SSH                  `yaml:"ssh"`
	Parallelism int                  `yaml:"parallelism"`
}

// Registry holds the image registry credentials. Password names a secret.
type Registry struct {
	Server   string `yaml:"server"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// Role is a group of hosts running the image with the same command.
type Role struct {
	Hosts   []string `yaml:"hosts"`
	Cmd     []string `yaml:"cmd"`
	Proxy   *bool    `yaml:"proxy"`
	Env     Env      `yaml:"env"`
	Volumes []string `yaml:"volumes"`
}

// Env lists clear values and the names of secrets read from .forge/secrets.
type Env struct {
	Clear  map[string]string `yaml:"clear"`
	Secret []string          `yaml:"secret"`
}

// Proxy configures forge-proxy for the proxied role.
type Proxy struct {
	Image           string      `yaml:"image"`
	Hosts           []string    `yaml:"hosts"`
	SSL             bool        `yaml:"ssl"`
	ACMEEmail       string      `yaml:"acme_email"`
	AppPort         int         `yaml:"app_port"`
	Healthcheck     Healthcheck `yaml:"healthcheck"`
	DrainTimeout    Duration    `yaml:"drain_timeout"`
	ResponseTimeout Duration    `yaml:"response_timeout"`
	MaxBodyBytes    int64       `yaml:"max_body_bytes"`
}

// Healthcheck gates traffic to a new container.
type Healthcheck struct {
	Path          string   `yaml:"path"`
	Interval      Duration `yaml:"interval"`
	Timeout       Duration `yaml:"timeout"`
	HealthyCount  int      `yaml:"healthy_count"`
	DeployTimeout Duration `yaml:"deploy_timeout"`
}

// Migrate runs the image's migrate command once per deploy before any
// container is switched.
//
// Env is an overlay for that one-off container only: the container gets the
// environment of Role plus Env, Env winning, and no serving container ever
// sees it. It is how the schema owner's credential
// (FORGE_MIGRATION_DATABASE_URL) reaches the migrate step alone. A pointer
// tells an absent block (the container uses the role's env file, as before)
// from an empty one (an error).
type Migrate struct {
	Enabled *bool    `yaml:"enabled"`
	Cmd     []string `yaml:"cmd"`
	Role    string   `yaml:"role"`
	Env     *Env     `yaml:"env"`
}

// Accessory is a long-lived supporting container such as a database.
type Accessory struct {
	Image   string   `yaml:"image"`
	Host    string   `yaml:"host"`
	Port    string   `yaml:"port"`
	Volumes []string `yaml:"volumes"`
	Env     Env      `yaml:"env"`
	Cmd     []string `yaml:"cmd"`
}

// Builder configures the local image build.
type Builder struct {
	Context    string            `yaml:"context"`
	Dockerfile string            `yaml:"dockerfile"`
	Platform   string            `yaml:"platform"`
	Args       map[string]string `yaml:"args"`
}

// SSH configures the system ssh client. Unset values defer to ~/.ssh/config.
type SSH struct {
	User      string `yaml:"user"`
	Port      int    `yaml:"port"`
	ProxyJump string `yaml:"proxy_jump"`
}

// Duration is a YAML duration string such as "30s".
type Duration time.Duration

// UnmarshalYAML accepts only strings, so "30" is not silently 30ns.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
		return fmt.Errorf("line %d: duration must be a string such as \"30s\"", node.Line)
	}
	parsed, err := time.ParseDuration(node.Value)
	if err != nil {
		return fmt.Errorf("line %d: invalid duration %q", node.Line, node.Value)
	}
	*d = Duration(parsed)
	return nil
}

// D returns the value as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// Defaults.
const (
	DefaultRetain        = 5
	MinRetain            = 2
	DefaultStopTimeout   = 35 * time.Second
	DefaultBootTimeout   = 60 * time.Second
	DefaultAppPort       = 8080
	DefaultParallelism   = 4
	DefaultHooksDir      = ".forge/hooks"
	DefaultHealthPath    = "/health/ready"
	DefaultHealthyCount  = 3
	DefaultDeployTimeout = 60 * time.Second
	DefaultDrainTimeout  = 30 * time.Second
	DefaultResponseLimit = 30 * time.Second
	DefaultMaxBodyBytes  = 64 << 20
)

var (
	serviceNamePattern  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)
	roleNamePattern     = regexp.MustCompile(`^[a-z][a-z0-9-]{0,19}$`)
	envKeyPattern       = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	imagePattern        = regexp.MustCompile(`^[a-z0-9][a-z0-9._/:-]{0,254}$`)
	imageRefPattern     = regexp.MustCompile(`^[a-z0-9][a-z0-9._/:@-]{0,254}$`)
	sshHostPattern      = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._:%-]{0,252}$`)
	userPattern         = regexp.MustCompile(`^[a-z_][a-z0-9_.-]{0,31}$`)
	dnsLabelPattern     = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	volumePattern       = regexp.MustCompile(`^[A-Za-z0-9_./-]+:/[A-Za-z0-9_./-]*(:(ro|rw))?$`)
	healthPathPattern   = regexp.MustCompile(`^/[A-Za-z0-9._~/-]*$`)
	emailPattern        = regexp.MustCompile(`^[^@\s]+@[^@\s]+$`)
	registryHostPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*(:[0-9]{1,5})?$`)
	proxyJumpPattern    = regexp.MustCompile(`^([a-z_][a-z0-9_.-]*@)?[A-Za-z0-9_][A-Za-z0-9._-]*(:[0-9]{1,5})?$`)
	platformPattern     = regexp.MustCompile(`^[a-z0-9]+/[a-z0-9]+(/[a-z0-9]+)?(,[a-z0-9]+/[a-z0-9]+(/[a-z0-9]+)?)*$`)
	argPattern          = regexp.MustCompile(`^[^\x00-\x1f]*$`)
)

// LoadConfig reads and validates the configuration at path. Unknown keys,
// duplicate keys, several YAML documents and wrongly typed values are
// errors.
func LoadConfig(path string) (*Config, error) {
	content, err := os.ReadFile(path) // #nosec G304 -- the operator names the config file.
	if err != nil {
		return nil, fmt.Errorf("read deploy config: %w", err)
	}
	return ParseConfig(content)
}

// ParseConfig decodes and validates a configuration.
func ParseConfig(content []byte) (*Config, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	decoder.KnownFields(true)
	var cfg Config
	if err := decoder.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("deploy config is empty")
		}
		return nil, fmt.Errorf("deploy config: %w", err)
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("deploy config must contain exactly one YAML document")
	}
	if err := cfg.normalize(); err != nil {
		return nil, fmt.Errorf("deploy config: %w", err)
	}
	return &cfg, nil
}

func (c *Config) normalize() error {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	if !serviceNamePattern.MatchString(c.Service) {
		add("service must match %s", serviceNamePattern)
	}
	if err := validImageName(c.Image); err != nil {
		add("image: %v", err)
	}
	if c.Registry.Server != "" && !registryHostPattern.MatchString(c.Registry.Server) {
		add("registry.server must be a host name with an optional port")
	}
	if (c.Registry.Username == "") != (c.Registry.Password == "") {
		add("registry.username and registry.password (a secret name) must be set together")
	}
	if c.Registry.Password != "" && !envKeyPattern.MatchString(c.Registry.Password) {
		add("registry.password must name a secret in .forge/secrets, not contain the password")
	}
	if c.Registry.Username != "" && strings.ContainsAny(c.Registry.Username, " \t\r\n") {
		add("registry.username contains whitespace")
	}
	if len(c.Servers) == 0 {
		add("servers must define at least one role")
	}
	proxyRoles := 0
	for _, name := range sortedKeys(c.Servers) {
		role := c.Servers[name]
		if !roleNamePattern.MatchString(name) {
			add("servers.%s: role name must match %s", name, roleNamePattern)
		}
		if len(role.Hosts) == 0 {
			add("servers.%s.hosts must list at least one host", name)
		}
		seen := map[string]bool{}
		for _, host := range role.Hosts {
			if !validSSHHost(host) {
				add("servers.%s.hosts: %q is not a valid host name or address", name, host)
			}
			if seen[host] {
				add("servers.%s.hosts: %q is listed twice", name, host)
			}
			seen[host] = true
		}
		if role.Proxy == nil {
			proxied := name == "web"
			role.Proxy = &proxied
		}
		if *role.Proxy {
			proxyRoles++
		}
		checkArgs(fmt.Sprintf("servers.%s.cmd", name), role.Cmd, add)
		checkEnv(fmt.Sprintf("servers.%s.env", name), role.Env, add)
		checkVolumes(fmt.Sprintf("servers.%s.volumes", name), role.Volumes, add)
		c.Servers[name] = role
	}
	if proxyRoles > 1 {
		add("only one role may use the proxy (proxy: true)")
	}
	checkEnv("env", c.Env, add)
	checkVolumes("volumes", c.Volumes, add)
	for _, name := range sortedKeys(c.Servers) {
		for key := range c.Servers[name].Env.Clear {
			if slices.Contains(c.Env.Secret, key) {
				add("servers.%s.env.clear.%s is also a secret in env.secret", name, key)
			}
		}
		for _, key := range c.Servers[name].Env.Secret {
			if _, ok := c.Env.Clear[key]; ok {
				add("servers.%s.env.secret %s is also in env.clear", name, key)
			}
		}
	}

	c.normalizeProxy(proxyRoles > 0, add)

	if c.Migrate.Enabled == nil {
		enabled := true
		c.Migrate.Enabled = &enabled
	}
	if len(c.Migrate.Cmd) == 0 {
		c.Migrate.Cmd = []string{"migrate"}
	}
	checkArgs("migrate.cmd", c.Migrate.Cmd, add)
	if c.Migrate.Role == "" {
		c.Migrate.Role = c.PrimaryRole()
	} else if _, ok := c.Servers[c.Migrate.Role]; !ok {
		add("migrate.role %q is not a role in servers", c.Migrate.Role)
	}
	if env := c.Migrate.Env; env != nil {
		if len(env.Clear) == 0 && len(env.Secret) == 0 {
			add("migrate.env must set at least one clear value or secret (remove the block to migrate with the role's env file)")
		}
		if !*c.Migrate.Enabled {
			add("migrate.env is set but migrate.enabled is false, so nothing would use it")
		}
		checkEnv("migrate.env", *env, add)
	}

	for _, name := range sortedKeys(c.Accessories) {
		accessory := c.Accessories[name]
		if !roleNamePattern.MatchString(name) {
			add("accessories.%s: name must match %s", name, roleNamePattern)
		}
		if !imageRefPattern.MatchString(accessory.Image) {
			add("accessories.%s.image is required and must be an image reference", name)
		}
		if !validSSHHost(accessory.Host) {
			add("accessories.%s.host must be a host name or address", name)
		}
		if accessory.Port != "" {
			if _, err := publishSpec(accessory.Port); err != nil {
				add("accessories.%s.port: %v", name, err)
			}
		}
		checkVolumes(fmt.Sprintf("accessories.%s.volumes", name), accessory.Volumes, add)
		checkEnv(fmt.Sprintf("accessories.%s.env", name), accessory.Env, add)
		checkArgs(fmt.Sprintf("accessories.%s.cmd", name), accessory.Cmd, add)
	}

	if c.Builder.Context == "" {
		c.Builder.Context = "."
	}
	if c.Builder.Dockerfile == "" {
		c.Builder.Dockerfile = "Dockerfile"
	}
	if strings.HasPrefix(c.Builder.Context, "-") || strings.HasPrefix(c.Builder.Dockerfile, "-") {
		add("builder.context and builder.dockerfile must not start with '-'")
	}
	if c.Builder.Platform != "" && !platformPattern.MatchString(c.Builder.Platform) {
		add("builder.platform must look like linux/amd64")
	}
	for key, value := range c.Builder.Args {
		if !envKeyPattern.MatchString(key) || !argPattern.MatchString(value) {
			add("builder.args.%s is invalid", key)
		}
	}

	if c.Retain == 0 {
		c.Retain = DefaultRetain
	}
	// Containers are counted newest first including the running one, so
	// retain: 1 would prune the previous version and make rollback impossible.
	if c.Retain < MinRetain || c.Retain > 100 {
		add("retain must be between 2 and 100 (1 would remove the previous version, so rollback would be impossible)")
	}
	durationDefault(&c.StopTimeout, DefaultStopTimeout, time.Second, 10*time.Minute, "stop_timeout", add)
	durationDefault(&c.BootTimeout, DefaultBootTimeout, time.Second, time.Hour, "boot_timeout", add)
	if c.Hooks == "" {
		c.Hooks = DefaultHooksDir
	}
	if c.SSH.User != "" && !userPattern.MatchString(c.SSH.User) {
		add("ssh.user is not a valid user name")
	}
	if c.SSH.Port < 0 || c.SSH.Port > 65535 {
		add("ssh.port must be between 1 and 65535")
	}
	if c.SSH.ProxyJump != "" && !proxyJumpPattern.MatchString(c.SSH.ProxyJump) {
		add("ssh.proxy_jump must look like [user@]host[:port]")
	}
	if c.Parallelism == 0 {
		c.Parallelism = DefaultParallelism
	}
	if c.Parallelism < 1 || c.Parallelism > 64 {
		add("parallelism must be between 1 and 64")
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func (c *Config) normalizeProxy(used bool, add func(string, ...any)) {
	p := &c.Proxy
	if !used {
		if p.Image != "" || len(p.Hosts) > 0 || p.SSL {
			add("proxy is configured but no role uses it (set proxy: true on a role)")
		}
		return
	}
	if !imageRefPattern.MatchString(p.Image) {
		add("proxy.image is required: the forge-proxy image built from cmd/forge-proxy/Dockerfile, ideally pinned by digest")
	}
	seen := map[string]bool{}
	for i, host := range p.Hosts {
		host = strings.ToLower(host)
		if !validDNSName(host) {
			add("proxy.hosts: %q is not a DNS name", host)
		}
		if seen[host] {
			add("proxy.hosts: %q is listed twice", host)
		}
		seen[host] = true
		p.Hosts[i] = host
	}
	if p.SSL && len(p.Hosts) == 0 {
		add("proxy.ssl requires proxy.hosts")
	}
	if p.ACMEEmail != "" && !emailPattern.MatchString(p.ACMEEmail) {
		add("proxy.acme_email is not an e-mail address")
	}
	if p.AppPort == 0 {
		p.AppPort = DefaultAppPort
	}
	if p.AppPort < 1 || p.AppPort > 65535 {
		add("proxy.app_port must be between 1 and 65535")
	}
	h := &p.Healthcheck
	if h.Path == "" {
		h.Path = DefaultHealthPath
	}
	if !healthPathPattern.MatchString(h.Path) {
		add("proxy.healthcheck.path must be an absolute path without query")
	}
	if h.HealthyCount == 0 {
		h.HealthyCount = DefaultHealthyCount
	}
	if h.HealthyCount < 1 || h.HealthyCount > 100 {
		add("proxy.healthcheck.healthy_count must be between 1 and 100")
	}
	durationDefault(&h.Interval, time.Second, 10*time.Millisecond, time.Minute, "proxy.healthcheck.interval", add)
	durationDefault(&h.Timeout, 2*time.Second, 10*time.Millisecond, time.Minute, "proxy.healthcheck.timeout", add)
	durationDefault(&h.DeployTimeout, DefaultDeployTimeout, time.Second, time.Hour, "proxy.healthcheck.deploy_timeout", add)
	durationDefault(&p.DrainTimeout, DefaultDrainTimeout, 0, time.Hour, "proxy.drain_timeout", add)
	durationDefault(&p.ResponseTimeout, DefaultResponseLimit, time.Second, time.Hour, "proxy.response_timeout", add)
	if p.MaxBodyBytes == 0 {
		p.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if p.MaxBodyBytes < 0 {
		add("proxy.max_body_bytes must be positive")
	}
}

func durationDefault(d *Duration, def, minimum, maximum time.Duration, name string, add func(string, ...any)) {
	if *d == 0 {
		*d = Duration(def)
	}
	if d.D() < minimum || d.D() > maximum {
		add("%s must be between %s and %s", name, minimum, maximum)
	}
}

func checkEnv(prefix string, env Env, add func(string, ...any)) {
	for key, value := range env.Clear {
		if !envKeyPattern.MatchString(key) {
			add("%s.clear: %q is not a valid variable name", prefix, key)
		}
		if strings.ContainsAny(value, "\r\n\x00") {
			add("%s.clear.%s must be a single line", prefix, key)
		}
	}
	seen := map[string]bool{}
	for _, key := range env.Secret {
		if !envKeyPattern.MatchString(key) {
			add("%s.secret: %q is not a valid secret name", prefix, key)
		}
		if seen[key] {
			add("%s.secret: %s is listed twice", prefix, key)
		}
		seen[key] = true
		if _, ok := env.Clear[key]; ok {
			add("%s: %s is both clear and secret", prefix, key)
		}
	}
}

func checkVolumes(prefix string, volumes []string, add func(string, ...any)) {
	for _, volume := range volumes {
		if !volumePattern.MatchString(volume) || strings.Contains(volume, "..") || strings.HasPrefix(volume, "-") {
			add("%s: %q must be NAME:/container/path or /host/path:/container/path with optional :ro", prefix, volume)
		}
	}
}

func checkArgs(prefix string, args []string, add func(string, ...any)) {
	for _, arg := range args {
		if !argPattern.MatchString(arg) {
			add("%s contains a control character", prefix)
		}
	}
}

func validImageName(image string) error {
	if !imagePattern.MatchString(image) {
		return errors.New("required, lower-case repository such as registry.example.com/team/app")
	}
	last := image[strings.LastIndex(image, "/")+1:]
	if strings.Contains(last, ":") || strings.Contains(image, "@") {
		return errors.New("must not include a tag or digest; the deploy version is the tag")
	}
	return nil
}

func validSSHHost(host string) bool {
	return sshHostPattern.MatchString(host)
}

func validDNSName(host string) bool {
	if host == "" || len(host) > 253 || net.ParseIP(host) != nil {
		return false
	}
	for label := range strings.SplitSeq(host, ".") {
		if !dnsLabelPattern.MatchString(label) {
			return false
		}
	}
	return true
}

// publishSpec turns an accessory port into a docker --publish value bound
// to loopback unless an address is given explicitly: "5432" and
// "6543:5432" bind 127.0.0.1; "10.0.0.5:6543:5432" binds that address.
func publishSpec(port string) (string, error) {
	parts := strings.Split(port, ":")
	checkPort := func(value string) error {
		number, err := strconv.Atoi(value)
		if err != nil || number < 1 || number > 65535 {
			return fmt.Errorf("%q is not a port number", value)
		}
		return nil
	}
	switch len(parts) {
	case 1:
		if err := checkPort(parts[0]); err != nil {
			return "", err
		}
		return "127.0.0.1:" + parts[0] + ":" + parts[0], nil
	case 2:
		if err := checkPort(parts[0]); err != nil {
			return "", err
		}
		if err := checkPort(parts[1]); err != nil {
			return "", err
		}
		return "127.0.0.1:" + port, nil
	case 3:
		if net.ParseIP(parts[0]) == nil || parts[0] == "" {
			return "", errors.New("the bind address must be an IP address")
		}
		if err := checkPort(parts[1]); err != nil {
			return "", err
		}
		if err := checkPort(parts[2]); err != nil {
			return "", err
		}
		return port, nil
	default:
		return "", errors.New("use PORT, HOST_PORT:CONTAINER_PORT or IP:HOST_PORT:CONTAINER_PORT")
	}
}

// PrimaryRole is the proxied role, else "web", else the first role by name.
func (c *Config) PrimaryRole() string {
	for _, name := range sortedKeys(c.Servers) {
		if role := c.Servers[name]; role.Proxy != nil && *role.Proxy {
			return name
		}
	}
	if _, ok := c.Servers["web"]; ok {
		return "web"
	}
	names := sortedKeys(c.Servers)
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

// Roles returns the role names, the primary role first, then by name.
func (c *Config) Roles() []string {
	primary := c.PrimaryRole()
	roles := []string{primary}
	for _, name := range sortedKeys(c.Servers) {
		if name != primary {
			roles = append(roles, name)
		}
	}
	return roles
}

// Proxied reports whether role routes traffic through forge-proxy.
func (c *Config) Proxied(role string) bool {
	r, ok := c.Servers[role]
	return ok && r.Proxy != nil && *r.Proxy
}

// AppHosts returns every host of every role, in role order then config
// order, without duplicates.
func (c *Config) AppHosts() []string {
	var hosts []string
	for _, role := range c.Roles() {
		for _, host := range c.Servers[role].Hosts {
			if !slices.Contains(hosts, host) {
				hosts = append(hosts, host)
			}
		}
	}
	return hosts
}

// AllHosts returns the application hosts followed by accessory hosts.
func (c *Config) AllHosts() []string {
	hosts := c.AppHosts()
	for _, name := range sortedKeys(c.Accessories) {
		if host := c.Accessories[name].Host; !slices.Contains(hosts, host) {
			hosts = append(hosts, host)
		}
	}
	return hosts
}

// RolesOn returns the roles that run on host, in [Config.Roles] order.
func (c *Config) RolesOn(host string) []string {
	var roles []string
	for _, role := range c.Roles() {
		if slices.Contains(c.Servers[role].Hosts, host) {
			roles = append(roles, role)
		}
	}
	return roles
}

// SecretNames lists every secret the configuration references, sorted.
func (c *Config) SecretNames() []string {
	var names []string
	addAll := func(list []string) {
		for _, name := range list {
			if !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
	}
	if c.Registry.Password != "" {
		addAll([]string{c.Registry.Password})
	}
	addAll(c.Env.Secret)
	if c.Migrate.Env != nil {
		addAll(c.Migrate.Env.Secret)
	}
	for _, role := range c.Servers {
		addAll(role.Env.Secret)
	}
	for _, accessory := range c.Accessories {
		addAll(accessory.Env.Secret)
	}
	slices.Sort(names)
	return names
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
