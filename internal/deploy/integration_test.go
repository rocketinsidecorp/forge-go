//go:build unix

package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The SSH integration tests run the real system ssh client against private
// sshd instances (one per simulated host) started by the test. Each
// session runs with a fake `docker` (internal/fakedocker) first on PATH and
// its own home and state directory, so the full orchestration, including
// quoting, stdin secrets, ControlMaster multiplexing, exit codes and stream
// cancellation, is exercised over real SSH. No Docker daemon is involved.

const (
	itRegistryPassword = "reg-pass-1234567"
	itDatabaseURL      = "postgres://shop:db-secret-9876543@db/shop"
	itPostgresPassword = "pg-secret-5555555"
)

type sshHost struct {
	name  string
	home  string
	state string
}

type sshCluster struct {
	dir       string
	sshConfig string
	hosts     map[string]*sshHost
}

func requireSSHD(t *testing.T) (string, string, string) {
	t.Helper()
	if testing.Short() {
		t.Skip("starts sshd")
	}
	required := os.Getenv("FORGE_TEST_REQUIRE_SSHD") == "true"
	skip := func(format string, args ...any) {
		if required {
			t.Fatalf(format, args...)
		}
		t.Skipf(format, args...)
	}
	sshd, err := exec.LookPath("sshd")
	if err != nil {
		for _, candidate := range []string{"/usr/sbin/sshd", "/usr/local/sbin/sshd"} {
			if _, statErr := os.Stat(candidate); statErr == nil {
				sshd, err = candidate, nil
			}
		}
	}
	if err != nil {
		skip("sshd not installed")
	}
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		skip("ssh not installed")
	}
	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		skip("ssh-keygen not installed")
	}
	if os.Geteuid() == 0 {
		if _, err := os.Stat("/run/sshd"); err != nil {
			skip("sshd running as root needs /run/sshd (mkdir -m 0755 /run/sshd)")
		}
	}
	_ = ssh
	return sshd, keygen, ssh
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	return port
}

func run(t *testing.T, name string, args ...string) {
	t.Helper()
	if output, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, output)
	}
}

func startCluster(t *testing.T, names ...string) *sshCluster {
	t.Helper()
	sshd, keygen, _ := requireSSHD(t)
	dir, err := os.MkdirTemp("", "fdi")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	bin := filepath.Join(dir, "bin")
	run(t, "go", "build", "-o", filepath.Join(bin, "docker"), "./internal/fakedocker")
	run(t, keygen, "-q", "-t", "ed25519", "-N", "", "-f", filepath.Join(dir, "hostkey"))
	run(t, keygen, "-q", "-t", "ed25519", "-N", "", "-f", filepath.Join(dir, "userkey"))
	userPub, err := os.ReadFile(filepath.Join(dir, "userkey.pub"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "authorized_keys"), userPub, 0o600); err != nil {
		t.Fatal(err)
	}
	hostPub, err := os.ReadFile(filepath.Join(dir, "hostkey.pub"))
	if err != nil {
		t.Fatal(err)
	}
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	cluster := &sshCluster{dir: dir, hosts: map[string]*sshHost{}}
	var sshConfig, knownHosts strings.Builder
	for _, name := range names {
		host := &sshHost{name: name, home: filepath.Join(dir, name, "home"), state: filepath.Join(dir, name, "docker")}
		for _, path := range []string{host.home, host.state} {
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		wrapper := filepath.Join(dir, name, "session.sh")
		script := fmt.Sprintf("#!/bin/sh\nexport PATH=%s:/usr/bin:/bin HOME=%s FAKE_DOCKER_STATE=%s\ncd \"$HOME\" || exit 1\nexec /bin/sh -c \"$SSH_ORIGINAL_COMMAND\"\n", bin, host.home, host.state)
		if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil { // #nosec G306 -- test wrapper must be executable.
			t.Fatal(err)
		}
		port := startSSHD(t, sshd, filepath.Join(dir, name), filepath.Join(dir, "hostkey"), filepath.Join(dir, "authorized_keys"), wrapper)
		fmt.Fprintf(&knownHosts, "[127.0.0.1]:%d %s", port, hostPub)
		fmt.Fprintf(&sshConfig, "Host %s\n  HostName 127.0.0.1\n  Port %d\n  User %s\n  IdentityFile %s\n  IdentitiesOnly yes\n  UserKnownHostsFile %s\n  StrictHostKeyChecking yes\n  LogLevel ERROR\n",
			name, port, current.Username, filepath.Join(dir, "userkey"), filepath.Join(dir, "known_hosts"))
		cluster.hosts[name] = host
	}
	if err := os.WriteFile(filepath.Join(dir, "known_hosts"), []byte(knownHosts.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	cluster.sshConfig = filepath.Join(dir, "ssh_config")
	if err := os.WriteFile(cluster.sshConfig, []byte(sshConfig.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return cluster
}

// startSSHD runs a private sshd on a free loopback port. Another process
// can take the port between freePort and sshd's bind, so a daemon that
// exits before it reports listening is retried on a new port; only our
// own daemon's "Server listening" line counts as started.
func startSSHD(t *testing.T, sshd, dir, hostKey, authorizedKeys, wrapper string) int {
	t.Helper()
	var lastLog string
	for range 5 {
		port := freePort(t)
		config := filepath.Join(dir, "sshd_config")
		sshdConfig := fmt.Sprintf(`Port %d
ListenAddress 127.0.0.1
HostKey %s
AuthorizedKeysFile %s
StrictModes no
UsePAM no
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitRootLogin prohibit-password
PidFile none
ForceCommand %s
`, port, hostKey, authorizedKeys, wrapper)
		if err := os.WriteFile(config, []byte(sshdConfig), 0o600); err != nil {
			t.Fatal(err)
		}
		daemon := exec.Command(sshd, "-D", "-e", "-f", config)
		daemonLog := &syncBuffer{}
		daemon.Stderr = daemonLog
		if err := daemon.Start(); err != nil {
			t.Fatal(err)
		}
		exited := make(chan struct{})
		go func() {
			_ = daemon.Wait()
			close(exited)
		}()
		stop := func() {
			_ = daemon.Process.Kill()
			<-exited
		}
		listening := fmt.Sprintf("Server listening on 127.0.0.1 port %d", port)
		deadline := time.Now().Add(5 * time.Second)
		for {
			if strings.Contains(daemonLog.String(), listening) {
				t.Cleanup(stop)
				return port
			}
			select {
			case <-exited:
			case <-time.After(20 * time.Millisecond):
				if time.Now().Before(deadline) {
					continue
				}
				stop()
			}
			break
		}
		lastLog = daemonLog.String()
	}
	t.Fatalf("sshd did not start: %s", lastLog)
	return 0
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

const integrationConfig = `
service: shop
image: registry.test/acme/shop
registry:
  server: registry.test
  username: acme
  password: REGISTRY_PASSWORD
servers:
  web:
    hosts: [alpha, beta]
  jobs:
    hosts: [beta]
    cmd: [jobs]
env:
  clear:
    FORGE_ENV: production
  secret: [FORGE_DATABASE_URL]
proxy:
  image: registry.test/forge-proxy@sha256:0000000000000000000000000000000000000000000000000000000000000000
  hosts: [shop.example.com]
  app_port: 8080
accessories:
  db:
    image: postgres:18-alpine
    host: beta
    port: "5432"
    env:
      secret: [POSTGRES_PASSWORD]
retain: 2
stop_timeout: 5s
`

func (c *sshCluster) deployer(t *testing.T, performer string) (*Deployer, *syncBuffer) {
	t.Helper()
	cfg, err := ParseConfig([]byte(integrationConfig))
	if err != nil {
		t.Fatal(err)
	}
	log := &syncBuffer{}
	runner := &SSHRunner{ConfigFile: c.sshConfig}
	t.Cleanup(func() { _ = runner.Close() })
	return &Deployer{
		Config: cfg,
		Secrets: NewSecrets(map[string]string{
			"REGISTRY_PASSWORD": itRegistryPassword, "FORGE_DATABASE_URL": itDatabaseURL, "POSTGRES_PASSWORD": itPostgresPassword,
		}),
		Runner: runner, Local: &recordingLocal{}, Root: t.TempDir(), Log: log, Performer: performer,
	}, log
}

type fakeState struct {
	Networks   []string
	Images     []string
	Containers []struct {
		Name    string
		Image   string
		State   string
		Labels  map[string]string
		Args    []string
		Publish []string
		Env     map[string]string
	}
	Routes      map[string]string
	Maintenance map[string]bool
	Logins      []string
	OneOffs     [][]string        `json:"one_offs"`
	EnvSeen     map[string]string `json:"env_seen"`
}

func (c *sshCluster) state(t *testing.T, host string) fakeState {
	t.Helper()
	var st fakeState
	content, err := os.ReadFile(filepath.Join(c.hosts[host].state, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(content, &st); err != nil {
		t.Fatal(err)
	}
	return st
}

func (s fakeState) versions(role string) map[string]string {
	versions := map[string]string{}
	for _, c := range s.Containers {
		if c.Labels[labelRole] == role {
			versions[c.Labels[labelVersion]] = c.State
		}
	}
	return versions
}

func routeVersion(t *testing.T, s fakeState) string {
	t.Helper()
	target := s.Routes["shop"]
	for _, c := range s.Containers {
		if strings.HasPrefix(target, c.Name+":") {
			return c.Labels[labelVersion]
		}
	}
	t.Fatalf("route %q points at no container", target)
	return ""
}

func TestSSHIntegration(t *testing.T) {
	cluster := startCluster(t, "alpha", "beta")
	ctx := t.Context()
	d, log := cluster.deployer(t, "alice@laptop")

	t.Run("setup", func(t *testing.T) {
		if _, err := d.Setup(ctx); err != nil {
			t.Fatalf("setup: %v\n%s", err, log.String())
		}
		alpha, beta := cluster.state(t, "alpha"), cluster.state(t, "beta")
		for _, c := range alpha.Containers {
			if c.Labels[labelService] != "" {
				t.Fatalf("setup started an application container: %s", c.Name)
			}
		}
		hasProxy := func(s fakeState) bool {
			for _, c := range s.Containers {
				if c.Name == ProxyContainer && c.State == "running" {
					return true
				}
			}
			return false
		}
		if !hasProxy(alpha) || !hasProxy(beta) || len(alpha.Networks) != 1 {
			t.Fatalf("proxy/network missing: %+v %+v", alpha, beta)
		}
		var db bool
		for _, c := range beta.Containers {
			if c.Name == "shop-db" && len(c.Publish) == 1 && c.Publish[0] == "127.0.0.1:5432:5432" {
				db = true
			}
		}
		if !db || !strings.Contains(beta.EnvSeen["shop-db"], "POSTGRES_PASSWORD="+itPostgresPassword) {
			t.Fatalf("accessory not booted with loopback port and env: %+v", beta)
		}
		if len(alpha.Logins) != 1 || !strings.HasSuffix(alpha.Logins[0], "password="+itRegistryPassword) {
			t.Fatalf("registry password not delivered on stdin: %v", alpha.Logins)
		}
		info, err := os.Stat(filepath.Join(cluster.hosts["alpha"].home, ".forge", "shop"))
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("state dir mode: %v %v", info, err)
		}
		// Setup is idempotent.
		if _, err := d.Setup(ctx); err != nil {
			t.Fatalf("second setup: %v", err)
		}
	})

	t.Run("deploy", func(t *testing.T) {
		for _, version := range []string{"v1", "v2"} {
			report, err := d.Deploy(ctx, DeployOptions{Version: version, SkipBuild: true})
			if err != nil {
				t.Fatalf("deploy %s: %v\n%s", version, err, log.String())
			}
			if len(report.Steps) != 3 {
				t.Fatalf("steps = %+v", report.Steps)
			}
		}
		alpha, beta := cluster.state(t, "alpha"), cluster.state(t, "beta")
		for name, s := range map[string]fakeState{"alpha": alpha, "beta": beta} {
			if v := routeVersion(t, s); v != "v2" {
				t.Fatalf("%s routes to %s", name, v)
			}
			if got := s.versions("web"); got["v1"] != "exited" || got["v2"] != "running" {
				t.Fatalf("%s web versions = %v", name, got)
			}
		}
		if got := beta.versions("jobs"); got["v1"] != "exited" || got["v2"] != "running" {
			t.Fatalf("jobs versions = %v", got)
		}
		for _, c := range beta.Containers {
			if c.Labels[labelRole] == "jobs" && (len(c.Args) != 1 || c.Args[0] != "jobs") {
				t.Fatalf("jobs cmd = %v", c.Args)
			}
			// Only the proxied role trusts the forge network's forwarded
			// client addresses (ADR 0037).
			proxies, set := c.Env[TrustedProxiesEnv]
			switch c.Labels[labelRole] {
			case "web":
				if proxies != "172.18.0.0/16" {
					t.Fatalf("web %s: %s = %q, want the forge network's subnet", c.Name, TrustedProxiesEnv, proxies)
				}
			case "jobs":
				if set {
					t.Fatalf("jobs %s trusts proxies %q", c.Name, proxies)
				}
			}
		}
		if len(alpha.OneOffs) != 2 || len(beta.OneOffs) != 0 || alpha.OneOffs[1][1] != "migrate" {
			t.Fatalf("migrations ran %v / %v, want once per deploy on alpha", alpha.OneOffs, beta.OneOffs)
		}
		envFile := filepath.Join(cluster.hosts["beta"].home, ".forge", "shop", "env", "web.env")
		info, err := os.Stat(envFile)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("env file mode %v %v", info, err)
		}
		content, _ := os.ReadFile(envFile)
		if string(content) != "FORGE_ENV=production\nFORGE_DATABASE_URL="+itDatabaseURL+"\n" {
			t.Fatalf("env file = %q", content)
		}
		history, _ := os.ReadFile(filepath.Join(cluster.hosts["alpha"].home, ".forge", "shop", "deploys.log"))
		if lines := strings.Count(string(history), "\n"); lines != 2 || !strings.Contains(string(history), `"performer":"alice@laptop"`) {
			t.Fatalf("deploy log = %s", history)
		}
		if _, err := os.Stat(filepath.Join(cluster.hosts["alpha"].home, ".forge", "shop", "lock")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("lock not released after deploy")
		}
	})

	t.Run("migration failure aborts before any switch", func(t *testing.T) {
		before := cluster.state(t, "alpha")
		_, err := d.Deploy(ctx, DeployOptions{Version: "v3-badmigrate", SkipBuild: true})
		if ExitCodeOf(err) != ExitMigrationFailed || !strings.Contains(err.Error(), "migration 0002_add_index failed") {
			t.Fatalf("err = %v (exit %d)", err, ExitCodeOf(err))
		}
		for _, host := range []string{"alpha", "beta"} {
			s := cluster.state(t, host)
			if _, ok := s.versions("web")["v3-badmigrate"]; ok {
				t.Fatalf("%s started a container after a failed migration", host)
			}
			if routeVersion(t, s) != "v2" {
				t.Fatalf("%s switched after a failed migration", host)
			}
		}
		if len(cluster.state(t, "alpha").Containers) != len(before.Containers) {
			t.Fatal("containers changed")
		}
		if _, err := os.Stat(filepath.Join(cluster.hosts["beta"].home, ".forge", "shop", "lock")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("lock not released after failure")
		}
	})

	t.Run("health failure on one host reverts every host", func(t *testing.T) {
		marker := filepath.Join(cluster.hosts["beta"].state, "unhealthy")
		if err := os.WriteFile(marker, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := d.Deploy(ctx, DeployOptions{Version: "v4", SkipBuild: true})
		_ = os.Remove(marker)
		if ExitCodeOf(err) != ExitUnhealthy || !strings.Contains(err.Error(), "previous version") {
			t.Fatalf("err = %v (exit %d)\n%s", err, ExitCodeOf(err), log.String())
		}
		for _, host := range []string{"alpha", "beta"} {
			s := cluster.state(t, host)
			if v := routeVersion(t, s); v != "v2" {
				t.Fatalf("%s routes to %s after revert", host, v)
			}
			for _, role := range []string{"web", "jobs"} {
				versions := s.versions(role)
				if _, ok := versions["v4"]; ok {
					t.Fatalf("%s kept a v4 %s container: %v", host, role, versions)
				}
				if role == "web" || host == "beta" {
					if versions["v2"] != "running" {
						t.Fatalf("%s %s v2 is %q", host, role, versions["v2"])
					}
				}
			}
		}
		if !strings.Contains(log.String(), "traffic switched back to shop-web-v2-") {
			t.Fatalf("alpha was not switched back:\n%s", log.String())
		}
	})

	t.Run("lock contention", func(t *testing.T) {
		other, _ := cluster.deployer(t, "bob@ci")
		if err := other.LockAcquire(ctx, "maintenance window"); err != nil {
			t.Fatal(err)
		}
		_, err := d.Deploy(ctx, DeployOptions{Version: "v5", SkipBuild: true})
		var held *LockHeldError
		if ExitCodeOf(err) != ExitLockHeld || !errors.As(err, &held) || !strings.Contains(err.Error(), "bob@ci") {
			t.Fatalf("err = %v", err)
		}
		if _, ok := cluster.state(t, "alpha").versions("web")["v5"]; ok {
			t.Fatal("deploy ran while locked")
		}
		locks, err := d.LockStatus(ctx)
		if err != nil || len(locks) != 2 || locks["alpha"].Message != "maintenance window" {
			t.Fatalf("lock status = %v, %v", locks, err)
		}
		if err := d.LockRelease(ctx, false); ExitCodeOf(err) != ExitLockHeld {
			t.Fatalf("released someone else's lock without --force: %v", err)
		}
		if err := d.LockRelease(ctx, true); err != nil {
			t.Fatal(err)
		}
		if locks, _ := d.LockStatus(ctx); len(locks) != 0 {
			t.Fatalf("locks remain: %v", locks)
		}
	})

	t.Run("rollback", func(t *testing.T) {
		if _, err := d.Rollback(ctx, "v1"); err != nil {
			t.Fatalf("rollback: %v\n%s", err, log.String())
		}
		for _, host := range []string{"alpha", "beta"} {
			s := cluster.state(t, host)
			if routeVersion(t, s) != "v1" {
				t.Fatalf("%s not rolled back", host)
			}
			if got := s.versions("web"); got["v1"] != "running" || got["v2"] != "exited" {
				t.Fatalf("%s web = %v", host, got)
			}
		}
		if got := cluster.state(t, "beta").versions("jobs"); got["v1"] != "running" || got["v2"] != "exited" {
			t.Fatalf("jobs = %v", got)
		}
		if _, err := d.Rollback(ctx, "v9"); err == nil || !strings.Contains(err.Error(), "not retained") {
			t.Fatalf("rollback to unknown version: %v", err)
		}
	})

	t.Run("deploy prunes beyond retain", func(t *testing.T) {
		report, err := d.Deploy(ctx, DeployOptions{Version: "v5", SkipBuild: true})
		if err != nil {
			t.Fatal(err)
		}
		alpha := cluster.state(t, "alpha")
		got := alpha.versions("web")
		if len(got) != 2 || got["v5"] != "running" {
			t.Fatalf("retain 2 kept %v", got)
		}
		if _, ok := got["v1"]; ok {
			t.Fatalf("oldest version not pruned: %v (report %v)", got, report.Pruned)
		}
		for _, image := range alpha.Images {
			if image == "registry.test/acme/shop:v1" {
				t.Fatalf("image of pruned version kept: %v", alpha.Images)
			}
		}
		if _, err := d.Prune(ctx); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("logs", func(t *testing.T) {
		var out syncBuffer
		if err := d.Logs(ctx, &out, LogsOptions{Grep: "booted|connecting"}); err != nil {
			t.Fatal(err)
		}
		text := out.String()
		for _, host := range []string{"alpha", "beta"} {
			if !strings.Contains(text, host+"|2026-10-19T10:00:00Z booted shop-web-v5-") {
				t.Fatalf("missing %s lines:\n%s", host, text)
			}
		}
		if strings.Contains(text, "ERROR") {
			t.Fatalf("grep did not filter:\n%s", text)
		}
		if strings.Contains(text, "db-secret") || !strings.Contains(text, Redacted) {
			t.Fatalf("secret not redacted:\n%s", text)
		}
	})

	t.Run("logs follow stops remote processes on cancel", func(t *testing.T) {
		followCtx, cancel := context.WithCancel(ctx)
		var out syncBuffer
		done := make(chan error, 1)
		go func() { done <- d.Logs(followCtx, &out, LogsOptions{Follow: true}) }()
		deadline := time.Now().Add(10 * time.Second)
		for strings.Count(out.String(), "ERROR something failed") < 2 {
			if time.Now().After(deadline) {
				t.Fatalf("no output from both hosts:\n%s", out.String())
			}
			time.Sleep(20 * time.Millisecond)
		}
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("logs did not return after cancel")
		}
		for _, host := range []string{"alpha", "beta"} {
			pids, _ := filepath.Glob(filepath.Join(cluster.hosts[host].state, "logs-*.pid"))
			if len(pids) == 0 {
				t.Fatalf("%s: no follow process recorded", host)
			}
			for _, pidFile := range pids {
				content, _ := os.ReadFile(pidFile)
				var pid int
				_, _ = fmt.Sscan(string(content), &pid)
				gone := false
				for range 100 {
					if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
						gone = true
						break
					}
					time.Sleep(50 * time.Millisecond)
				}
				if !gone {
					_ = syscall.Kill(pid, syscall.SIGKILL)
					t.Fatalf("%s: remote docker logs (pid %d) still running after cancel", host, pid)
				}
				_ = os.Remove(pidFile)
			}
		}
	})

	t.Run("app exec", func(t *testing.T) {
		var out syncBuffer
		if err := d.Exec(ctx, &out, ExecOptions{Argv: []string{"migrate", "status"}}); err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(out.String()) != "ran migrate status" {
			t.Fatalf("exec output %q", out.String())
		}
		oneOffs := cluster.state(t, "alpha").OneOffs
		last := oneOffs[len(oneOffs)-1]
		if strings.Join(last, " ") != "registry.test/acme/shop:v5 migrate status" {
			t.Fatalf("one-off = %v", last)
		}
		out = syncBuffer{}
		if err := d.Exec(ctx, &out, ExecOptions{Role: "jobs", Reuse: true, Argv: []string{"/app/server", "it's quoted"}}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "exec in shop-jobs-v5-") || !strings.Contains(out.String(), "/app/server it's quoted") {
			t.Fatalf("reuse output %q", out.String())
		}
	})

	t.Run("details", func(t *testing.T) {
		details, err := d.Details(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(details) != 2 || len(details[0].Proxy) != 1 || !strings.Contains(string(details[0].Proxy[0]), "shop-web-v5-") || len(details[1].Deploys) == 0 {
			t.Fatalf("details = %+v", details)
		}
	})

	t.Run("dry run changes nothing", func(t *testing.T) {
		before := cluster.state(t, "alpha")
		dry, plan := cluster.deployer(t, "alice@laptop")
		dry.DryRun = true
		if _, err := dry.Deploy(ctx, DeployOptions{Version: "v6", SkipBuild: true}); err != nil {
			t.Fatal(err)
		}
		after := cluster.state(t, "alpha")
		if len(after.Containers) != len(before.Containers) || routeVersion(t, after) != "v5" {
			t.Fatal("dry run changed state")
		}
		text := plan.String()
		if !strings.Contains(text, "docker stop -t 5 shop-web-v5-") || !strings.Contains(text, "forge-proxy deploy -service shop") {
			t.Fatalf("plan lacks real container names:\n%s", text)
		}
		if strings.Contains(text, itDatabaseURL) || strings.Contains(text, itRegistryPassword) {
			t.Fatalf("plan leaks a secret:\n%s", text)
		}
	})

	t.Run("secrets never appear in remote command lines", func(t *testing.T) {
		for _, host := range []string{"alpha", "beta"} {
			calls, err := os.ReadFile(filepath.Join(cluster.hosts[host].state, "calls.log"))
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{itRegistryPassword, itDatabaseURL, itPostgresPassword} {
				if strings.Contains(string(calls), secret) {
					t.Fatalf("%s: secret in docker argv", host)
				}
			}
		}
		if strings.Contains(log.String(), "db-secret") {
			t.Fatal("secret in progress log")
		}
	})
}
