package deploy

import (
	"fmt"
	"slices"
	"strings"
)

// Names and paths shared by every host. Remote paths are relative to the
// SSH user's home directory, where commands start.
const (
	NetworkName        = "forge"
	ProxyContainer     = "forge-proxy"
	proxyVolume        = "forge-proxy-data"
	proxyBinary        = "/usr/local/bin/forge-proxy"
	stateRoot          = ".forge"
	labelService       = "forge.service"
	labelRole          = "forge.role"
	labelVersion       = "forge.version"
	labelAccessory     = "forge.accessory"
	lockHeldExitStatus = 75
)

// commands builds the remote commands for one configuration. Keeping them
// here, apart from the orchestration, makes the exact text reviewable and
// golden-tested.
type commands struct {
	cfg *Config
}

func (c commands) stateDir() string { return stateRoot + "/" + c.cfg.Service }
func (c commands) envDir() string   { return c.stateDir() + "/env" }
func (c commands) lockDir() string  { return c.stateDir() + "/lock" }
func (c commands) logFile() string  { return c.stateDir() + "/deploys.log" }

func (c commands) envFile(role string) string { return c.envDir() + "/" + role + ".env" }
func (c commands) accessoryEnvFile(name string) string {
	return c.envDir() + "/accessory-" + name + ".env"
}

func (c commands) imageRef(version string) string { return c.cfg.Image + ":" + version }

func (c commands) containerName(role, version, suffix string) string {
	return c.cfg.Service + "-" + role + "-" + version + "-" + suffix
}

func (c commands) accessoryName(name string) string { return c.cfg.Service + "-" + name }

func (c commands) dockerVersion() Command {
	return Command{Script: "docker version --format '{{.Server.Version}}'", ReadOnly: true}
}

func (c commands) prepareStateDir() Command {
	return Command{Script: Join("mkdir", "-p", c.envDir()) + " && " + Join("chmod", "700", stateRoot, c.stateDir(), c.envDir())}
}

func (c commands) ensureNetwork() Command {
	return Command{Script: Join("docker", "network", "inspect", NetworkName) + " >/dev/null 2>&1 || " + Join("docker", "network", "create", NetworkName)}
}

func (c commands) registryLogin(password []byte) Command {
	argv := []string{"docker", "login"}
	if c.cfg.Registry.Server != "" {
		argv = append(argv, c.cfg.Registry.Server)
	}
	argv = append(argv, "--username", c.cfg.Registry.Username, "--password-stdin")
	return Command{Script: Join(argv...), Stdin: password, StdinNote: "registry password (" + c.cfg.Registry.Password + ")"}
}

func (c commands) pull(version string) Command {
	return Command{Script: Join("docker", "pull", c.imageRef(version))}
}

// upload writes content to path atomically with 0600 permissions; content
// never appears in the command line or the process list.
func (c commands) upload(path string, content []byte, note string) Command {
	dir := path[:strings.LastIndex(path, "/")]
	tmp := path + ".tmp"
	return Command{
		Script:    "umask 077 && " + Join("mkdir", "-p", dir) + " && " + Join("cat") + " > " + Quote(tmp) + " && " + Join("mv", tmp, path),
		Stdin:     content,
		StdinNote: note,
	}
}

// envContent renders a docker --env-file. Values are single-line (checked
// at load) and written verbatim.
func envContent(clear map[string]string, secretNames []string, secrets Secrets) []byte {
	var b strings.Builder
	for _, key := range sortedKeys(clear) {
		fmt.Fprintf(&b, "%s=%s\n", key, clear[key])
	}
	for _, name := range secretNames {
		value, _ := secrets.Get(name)
		fmt.Fprintf(&b, "%s=%s\n", name, value)
	}
	return []byte(b.String())
}

// roleEnv merges the global and role environment.
func (c commands) roleEnv(role string) (map[string]string, []string) {
	clear := map[string]string{}
	for key, value := range c.cfg.Env.Clear {
		clear[key] = value
	}
	for key, value := range c.cfg.Servers[role].Env.Clear {
		clear[key] = value
	}
	secret := slices.Clone(c.cfg.Env.Secret)
	for _, name := range c.cfg.Servers[role].Env.Secret {
		if !slices.Contains(secret, name) {
			secret = append(secret, name)
		}
	}
	slices.Sort(secret)
	return clear, secret
}

func (c commands) uploadRoleEnv(role string, secrets Secrets) Command {
	clear, secret := c.roleEnv(role)
	note := fmt.Sprintf("env file: %d clear, %d secret (%s), values not shown", len(clear), len(secret), strings.Join(secret, ", "))
	return c.upload(c.envFile(role), envContent(clear, secret, secrets), note)
}

func (c commands) roleVolumes(role string) []string {
	volumes := slices.Clone(c.cfg.Volumes)
	return append(volumes, c.cfg.Servers[role].Volumes...)
}

func volumeArgs(volumes []string) []string {
	var args []string
	for _, volume := range volumes {
		args = append(args, "--volume", volume)
	}
	return args
}

func (c commands) migrate(version, suffix string) Command {
	role := c.cfg.Migrate.Role
	argv := []string{"docker", "run", "--rm", "--name", c.cfg.Service + "-migrate-" + version + "-" + suffix,
		"--network", NetworkName, "--env-file", c.envFile(role)}
	argv = append(argv, volumeArgs(c.roleVolumes(role))...)
	argv = append(argv, c.imageRef(version))
	argv = append(argv, c.cfg.Migrate.Cmd...)
	return Command{Script: Join(argv...)}
}

func (c commands) runRole(role, version, name string) Command {
	argv := []string{"docker", "run", "--detach", "--name", name,
		"--network", NetworkName, "--restart", "unless-stopped",
		"--stop-timeout", fmt.Sprint(int(c.cfg.StopTimeout.D().Seconds())),
		"--env-file", c.envFile(role),
		"--label", labelService + "=" + c.cfg.Service,
		"--label", labelRole + "=" + role,
		"--label", labelVersion + "=" + version,
	}
	argv = append(argv, volumeArgs(c.roleVolumes(role))...)
	argv = append(argv, c.imageRef(version))
	argv = append(argv, c.cfg.Servers[role].Cmd...)
	return Command{Script: Join(argv...)}
}

func (c commands) start(name string) Command {
	return Command{Script: Join("docker", "start", name)}
}

// stop sends SIGTERM and waits up to stop_timeout before SIGKILL, which
// lets the application's graceful shutdown finish.
func (c commands) stop(names ...string) Command {
	argv := append([]string{"docker", "stop", "-t", fmt.Sprint(int(c.cfg.StopTimeout.D().Seconds()))}, names...)
	return Command{Script: Join(argv...)}
}

func (c commands) remove(names ...string) Command {
	return Command{Script: Join(append([]string{"docker", "rm"}, names...)...)}
}

func (c commands) removeImage(ref string) Command {
	return Command{Script: Join("docker", "image", "rm", ref)}
}

func (c commands) inventory() Command {
	return Command{Script: Join("docker", "ps", "--all", "--no-trunc", "--filter", "label="+labelService+"="+c.cfg.Service, "--format", "{{json .}}"), ReadOnly: true}
}

func (c commands) images() Command {
	return Command{Script: Join("docker", "image", "ls", "--format", "{{json .}}", c.cfg.Image), ReadOnly: true}
}

// proxyDeploy asks forge-proxy to health-gate and switch to name. The
// proxy exits 4 when the target never became healthy.
func (c commands) proxyDeploy(name string) Command {
	p := c.cfg.Proxy
	argv := []string{"docker", "exec", ProxyContainer, proxyBinary, "deploy",
		"-service", c.cfg.Service,
		"-target", fmt.Sprintf("%s:%d", name, p.AppPort),
	}
	if len(p.Hosts) > 0 {
		argv = append(argv, "-hosts", strings.Join(p.Hosts, ","))
	}
	if p.SSL {
		argv = append(argv, "-tls")
	}
	argv = append(argv,
		"-health-path", p.Healthcheck.Path,
		"-health-interval", p.Healthcheck.Interval.D().String(),
		"-health-timeout", p.Healthcheck.Timeout.D().String(),
		"-healthy-count", fmt.Sprint(p.Healthcheck.HealthyCount),
		"-deploy-timeout", p.Healthcheck.DeployTimeout.D().String(),
		"-drain-timeout", p.DrainTimeout.D().String(),
		"-response-timeout", p.ResponseTimeout.D().String(),
		"-max-body-bytes", fmt.Sprint(p.MaxBodyBytes),
	)
	return Command{Script: Join(argv...)}
}

func (c commands) proxyStatus() Command {
	return Command{Script: Join("docker", "exec", ProxyContainer, proxyBinary, "status"), ReadOnly: true}
}

func (c commands) proxyMaintenance(on bool) Command {
	state := "off"
	if on {
		state = "on"
	}
	return Command{Script: Join("docker", "exec", ProxyContainer, proxyBinary, "maintenance", "-service", c.cfg.Service, state)}
}

// bootProxy starts forge-proxy unless it already exists. Ports 80 and 443
// are published on all addresses; the control socket is only reachable
// through docker exec.
func (c commands) bootProxy() Command {
	argv := []string{"docker", "run", "--detach", "--name", ProxyContainer,
		"--network", NetworkName, "--restart", "unless-stopped",
		"--publish", "80:8080", "--publish", "443:8443",
		"--volume", proxyVolume + ":/var/lib/forge-proxy",
		"--label", "forge.proxy=true",
		c.cfg.Proxy.Image, "run"}
	if c.cfg.Proxy.ACMEEmail != "" {
		argv = append(argv, "-acme-email", c.cfg.Proxy.ACMEEmail)
	}
	return Command{Script: Join("docker", "container", "inspect", ProxyContainer) + " >/dev/null 2>&1 && " +
		Join("docker", "start", ProxyContainer) + " >/dev/null || " + Join(argv...)}
}

func (c commands) uploadAccessoryEnv(name string, secrets Secrets) Command {
	accessory := c.cfg.Accessories[name]
	secret := slices.Clone(accessory.Env.Secret)
	slices.Sort(secret)
	note := fmt.Sprintf("env file: %d clear, %d secret (%s), values not shown", len(accessory.Env.Clear), len(secret), strings.Join(secret, ", "))
	return c.upload(c.accessoryEnvFile(name), envContent(accessory.Env.Clear, secret, secrets), note)
}

func (c commands) bootAccessory(name string) Command {
	accessory := c.cfg.Accessories[name]
	container := c.accessoryName(name)
	argv := []string{"docker", "run", "--detach", "--name", container,
		"--network", NetworkName, "--restart", "unless-stopped",
		"--env-file", c.accessoryEnvFile(name),
		"--label", labelAccessory + "=" + c.cfg.Service + "/" + name}
	if accessory.Port != "" {
		spec, _ := publishSpec(accessory.Port)
		argv = append(argv, "--publish", spec)
	}
	argv = append(argv, volumeArgs(accessory.Volumes)...)
	argv = append(argv, accessory.Image)
	argv = append(argv, accessory.Cmd...)
	return Command{Script: Join("docker", "container", "inspect", container) + " >/dev/null 2>&1 && " +
		Join("docker", "start", container) + " >/dev/null || " + Join(argv...)}
}

// waitReady polls a container without a proxy until Docker reports it
// healthy (when the image has a HEALTHCHECK) or running for three checks.
func (c commands) waitReady(name string) Command {
	seconds := int(c.cfg.BootTimeout.D().Seconds())
	format := "{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}"
	return Command{Script: "end=$(($(date +%s)+" + fmt.Sprint(seconds) + ")); ok=0; " +
		"while :; do s=$(" + Join("docker", "inspect", "--format", format, name) + ") || exit 1; " +
		`case "$s" in "running healthy") exit 0;; "running none") ok=$((ok+1)); [ "$ok" -ge 3 ] && exit 0;; "running starting") ;; *) echo "container ` + name + ` is $s" >&2; exit 1;; esac; ` +
		`[ "$(date +%s)" -ge "$end" ] && { echo "container ` + name + ` not ready after ` + fmt.Sprint(seconds) + `s" >&2; exit 1; }; sleep 1; done`}
}

func (c commands) acquireLock(record []byte) Command {
	return Command{
		Script: Join("mkdir", "-p", c.stateDir()) + " && if " + Join("mkdir", c.lockDir()) + " 2>/dev/null; then " +
			Join("cat") + " > " + Quote(c.lockDir()+"/info") + "; else " +
			Join("cat", c.lockDir()+"/info") + " 2>/dev/null; exit " + fmt.Sprint(lockHeldExitStatus) + "; fi",
		Stdin:     record,
		StdinNote: "lock record (holder, time, message)",
	}
}

func (c commands) lockStatus() Command {
	return Command{Script: "if [ -d " + Quote(c.lockDir()) + " ]; then " + Join("cat", c.lockDir()+"/info") + "; fi", ReadOnly: true}
}

func (c commands) releaseLock() Command {
	return Command{Script: Join("rm", "-rf", c.lockDir())}
}

func (c commands) appendLog(entry []byte) Command {
	return Command{
		Script:    "umask 077 && " + Join("mkdir", "-p", c.stateDir()) + " && " + Join("cat") + " >> " + Quote(c.logFile()),
		Stdin:     entry,
		StdinNote: "deploy log entry",
	}
}

func (c commands) readLog(lines int) Command {
	return Command{Script: Join("tail", "-n", fmt.Sprint(lines), c.logFile()) + " 2>/dev/null || true", ReadOnly: true}
}

// latest selects the newest running container of role in a shell variable.
func (c commands) latest(role string) string {
	return "c=$(" + Join("docker", "ps", "--latest", "--quiet", "--filter", "label="+labelService+"="+c.cfg.Service,
		"--filter", "label="+labelRole+"="+role, "--filter", "status=running") + `); [ -n "$c" ] || { echo "no running ` + role + ` container" >&2; exit 1; }; `
}

// logs streams docker logs of the current role container. The watchdog
// stops `docker logs` when the local side closes stdin (Ctrl-C), even
// though no terminal is allocated.
func (c commands) logs(role string, follow bool, since string, lines int) Command {
	argv := []string{"docker", "logs", "--timestamps"}
	if follow {
		argv = append(argv, "--follow")
	}
	if since != "" {
		argv = append(argv, "--since", since)
	}
	if lines > 0 {
		argv = append(argv, "--tail", fmt.Sprint(lines))
	}
	return Command{Script: c.latest(role) + "exec 3<&0; " + Join(argv...) + ` "$c" 2>&1 & p=$!; { read -r _ <&3; kill "$p" 2>/dev/null; } & w=$!; wait "$p"; s=$?; kill "$w" 2>/dev/null; exit "$s"`, ReadOnly: true}
}

// execOneOff runs argv in a new container of the current image, with the
// role's env file and volumes, removed afterwards.
func (c commands) execOneOff(role string, argv []string, interactive bool) string {
	run := []string{"docker", "run", "--rm"}
	if interactive {
		run = append(run, "--interactive", "--tty")
	}
	run = append(run, "--network", NetworkName, "--env-file", c.envFile(role))
	run = append(run, volumeArgs(c.roleVolumes(role))...)
	return c.latest(role) + `i=$(docker inspect --format '{{.Config.Image}}' "$c") && ` + Join(run...) + ` "$i" ` + Join(argv...)
}

// execReuse runs argv inside the running role container.
func (c commands) execReuse(role string, argv []string, interactive bool) string {
	run := []string{"docker", "exec"}
	if interactive {
		run = append(run, "--interactive", "--tty")
	}
	return c.latest(role) + Join(run...) + ` "$c" ` + Join(argv...)
}

// debugShell starts image as a sidecar sharing the network and process
// namespaces of the running container, without its environment.
func (c commands) debugShell(role, image string) string {
	return c.latest(role) + Join("docker", "run", "--rm", "--interactive", "--tty") + ` --network "container:$c" --pid "container:$c" ` + Join(image, "sh")
}
