package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// Report summarizes an operation for --json output.
type Report struct {
	SchemaVersion int          `json:"schema_version"`
	Action        string       `json:"action"`
	Service       string       `json:"service"`
	Version       string       `json:"version,omitempty"`
	DryRun        bool         `json:"dry_run"`
	Steps         []RoleReport `json:"steps"`
	Pruned        []string     `json:"pruned"`
	Warnings      []string     `json:"warnings"`
	DurationMS    int64        `json:"duration_ms"`
}

// RoleReport describes the containers of one role on one host.
type RoleReport struct {
	Host      string   `json:"host"`
	Role      string   `json:"role"`
	Container string   `json:"container"`
	Stopped   []string `json:"stopped"`
}

func (d *Deployer) newReport(action, version string) *Report {
	return &Report{SchemaVersion: 1, Action: action, Service: d.Config.Service, Version: version,
		DryRun: d.DryRun, Steps: []RoleReport{}, Pruned: []string{}, Warnings: []string{}}
}

func (r *Report) warn(format string, args ...any) {
	r.Warnings = append(r.Warnings, fmt.Sprintf(format, args...))
}

// Setup verifies Docker on every host, creates the network and state
// directory, logs in to the registry, and boots forge-proxy and the
// accessories. It is idempotent.
func (d *Deployer) Setup(ctx context.Context) (*Report, error) {
	if err := d.prepare(); err != nil {
		return nil, err
	}
	report := d.newReport("setup", "")
	started := d.Now()
	hosts := d.Config.AllHosts()
	release, err := d.acquire(ctx, hosts, "setup")
	if err != nil {
		return report, err
	}
	defer release()
	if d.Config.Proxy.Image != "" && !strings.Contains(d.Config.Proxy.Image, "@sha256:") {
		report.warn("proxy.image %s is not pinned by digest; pin it (image@sha256:...) so every host runs the reviewed build", d.Config.Proxy.Image)
	}
	proxyRole := ""
	for _, role := range d.Config.Roles() {
		if d.Config.Proxied(role) {
			proxyRole = role
		}
	}
	err = d.eachHost(ctx, hosts, func(ctx context.Context, host string) error {
		result, err := d.run(ctx, host, d.cmds.dockerVersion())
		if err != nil {
			return fmt.Errorf("%s: docker is not available: %w", host, err)
		}
		if version := strings.TrimSpace(result.Stdout); version != "" {
			d.logf(host, "docker %s", version)
		}
		for _, cmd := range []Command{d.cmds.prepareStateDir(), d.cmds.ensureNetwork()} {
			if _, err := d.run(ctx, host, cmd); err != nil {
				return err
			}
		}
		if err := d.login(ctx, host); err != nil {
			return err
		}
		if proxyRole != "" && slices.Contains(d.Config.Servers[proxyRole].Hosts, host) {
			if _, err := d.run(ctx, host, d.cmds.bootProxy()); err != nil {
				return fmt.Errorf("%s: boot forge-proxy: %w", host, err)
			}
			d.logf(host, "forge-proxy running")
		}
		for _, name := range sortedKeys(d.Config.Accessories) {
			if d.Config.Accessories[name].Host != host {
				continue
			}
			if _, err := d.run(ctx, host, d.cmds.uploadAccessoryEnv(name, d.Secrets)); err != nil {
				return err
			}
			if _, err := d.run(ctx, host, d.cmds.bootAccessory(name)); err != nil {
				return fmt.Errorf("%s: boot accessory %s: %w", host, name, err)
			}
			d.logf(host, "accessory %s running", name)
		}
		return nil
	})
	report.DurationMS = d.Now().Sub(started).Milliseconds()
	return report, err
}

func (d *Deployer) login(ctx context.Context, host string) error {
	if d.Config.Registry.Username == "" {
		return nil
	}
	password, _ := d.Secrets.Get(d.Config.Registry.Password)
	if _, err := d.run(ctx, host, d.cmds.registryLogin([]byte(password))); err != nil {
		return fmt.Errorf("%s: registry login: %w", host, err)
	}
	return nil
}

// DeployOptions select the version and build.
type DeployOptions struct {
	// Version is the image tag; it must already be resolved.
	Version string
	// SkipBuild deploys an image already in the registry.
	SkipBuild bool
}

// boot is one role container being brought up on one host.
type boot struct {
	host, role string
	name       string
	version    string
	// existing is true for rollback: the container already exists and is
	// started rather than created.
	existing bool
	previous []Container
	started  bool
	switched bool
}

// Deploy builds (unless skipped), distributes and migrates a version, then
// switches every host to it. The switch is all-or-nothing: previous
// containers keep running until every host passed its health gate; if any
// host fails, switched hosts are routed back to their previous container
// and the new containers are removed.
func (d *Deployer) Deploy(ctx context.Context, opts DeployOptions) (*Report, error) {
	if err := d.prepare(); err != nil {
		return nil, err
	}
	version := opts.Version
	if !versionPattern.MatchString(version) {
		return nil, fmt.Errorf("version %q must match %s", version, versionPattern)
	}
	report := d.newReport("deploy", version)
	started := d.Now()
	hosts := d.Config.AppHosts()
	release, err := d.acquire(ctx, hosts, "deploy "+version)
	if err != nil {
		return report, err
	}
	defer release()
	env := d.hookEnv("deploy", version, time.Time{})
	if !opts.SkipBuild {
		if err := d.hook(ctx, "pre-build", env); err != nil {
			return report, err
		}
		if err := d.build(ctx, version); err != nil {
			return report, err
		}
	}
	if err := d.hook(ctx, "pre-deploy", env); err != nil {
		return report, err
	}
	err = d.eachHost(ctx, hosts, func(ctx context.Context, host string) error {
		if err := d.login(ctx, host); err != nil {
			return err
		}
		if _, err := d.run(ctx, host, d.cmds.pull(version)); err != nil {
			return fmt.Errorf("%s: pull %s: %w", host, d.cmds.imageRef(version), err)
		}
		for _, role := range d.Config.RolesOn(host) {
			if _, err := d.run(ctx, host, d.cmds.uploadRoleEnv(role, d.Secrets)); err != nil {
				return err
			}
		}
		d.logf(host, "pulled %s", d.cmds.imageRef(version))
		return nil
	})
	if err != nil {
		return report, err
	}
	if *d.Config.Migrate.Enabled {
		host := d.Config.Servers[d.Config.Migrate.Role].Hosts[0]
		d.logf(host, "migrating with %s", d.cmds.imageRef(version))
		if _, err := d.run(ctx, host, d.cmds.migrate(version, d.Suffix())); err != nil {
			return report, withCode(ExitMigrationFailed, fmt.Errorf("migration failed; nothing was switched: %w", err))
		}
	}
	inventories, err := d.inventories(ctx, hosts)
	if err != nil {
		return report, err
	}
	plan := map[string][]*boot{}
	for _, host := range hosts {
		for _, role := range d.Config.RolesOn(host) {
			plan[host] = append(plan[host], &boot{
				host: host, role: role, version: version,
				name:     d.cmds.containerName(role, version, d.Suffix()),
				previous: running(inventories[host], role, ""),
			})
		}
	}
	if err := d.switchAll(ctx, hosts, plan, version); err != nil {
		return report, err
	}
	d.finish(ctx, report, hosts, plan, inventories)
	if err := d.hook(ctx, "post-deploy", d.hookEnv("deploy", version, started)); err != nil {
		return report, err
	}
	report.DurationMS = d.Now().Sub(started).Milliseconds()
	return report, nil
}

// running returns the running containers of role, except name.
func running(containers []Container, role, except string) []Container {
	var result []Container
	for _, c := range containers {
		if c.Role == role && c.Running() && c.Name != except {
			result = append(result, c)
		}
	}
	return result
}

// switchAll boots every planned container and reverts all hosts if any
// host fails.
func (d *Deployer) switchAll(ctx context.Context, hosts []string, plan map[string][]*boot, version string) error {
	err := d.eachHost(ctx, hosts, func(ctx context.Context, host string) error {
		for _, b := range plan[host] {
			if err := d.bootOne(ctx, b, version); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		return nil
	}
	code := ExitFailed
	var health *healthError
	if errors.As(err, &health) {
		code = ExitUnhealthy
	}
	if ctx.Err() != nil {
		return withCode(ExitInconsistent, fmt.Errorf("interrupted while switching; hosts may run different versions, run forge deploy details: %w", err))
	}
	d.logf("revert", "switch failed, reverting every host: %v", err)
	revertErr := d.eachHost(context.WithoutCancel(ctx), hosts, func(ctx context.Context, host string) error {
		return d.revert(ctx, plan[host])
	})
	if revertErr != nil {
		return withCode(ExitInconsistent, fmt.Errorf("%w; reverting also failed, hosts may run different versions (run forge deploy details): %v", err, revertErr))
	}
	return withCode(code, fmt.Errorf("%w; every host serves its previous version", err))
}

type healthError struct {
	err error
}

func (e *healthError) Error() string { return e.err.Error() }
func (e *healthError) Unwrap() error { return e.err }

func (d *Deployer) bootOne(ctx context.Context, b *boot, version string) error {
	cmd := d.cmds.runRole(b.role, version, b.name)
	verb := "started"
	if b.existing {
		cmd, verb = d.cmds.start(b.name), "restarted"
	}
	if _, err := d.run(ctx, b.host, cmd); err != nil {
		return fmt.Errorf("%s: start %s: %w", b.host, b.name, err)
	}
	b.started = true
	d.logf(b.host, "%s %s", verb, b.name)
	if d.Config.Proxied(b.role) {
		if _, err := d.run(ctx, b.host, d.cmds.proxyDeploy(b.name)); err != nil {
			var remote *RemoteError
			if errors.As(err, &remote) && remote.ExitCode == 4 {
				return &healthError{fmt.Errorf("%s: %s failed its health check: %w", b.host, b.name, err)}
			}
			return fmt.Errorf("%s: proxy switch to %s: %w", b.host, b.name, err)
		}
		b.switched = true
		d.logf(b.host, "traffic switched to %s", b.name)
		return nil
	}
	if _, err := d.run(ctx, b.host, d.cmds.waitReady(b.name)); err != nil {
		return &healthError{fmt.Errorf("%s: %s did not become ready: %w", b.host, b.name, err)}
	}
	d.logf(b.host, "%s ready", b.name)
	return nil
}

// revert undoes bootOne in reverse order. A host whose role had no previous
// container keeps the new one serving: there is nothing to go back to.
func (d *Deployer) revert(ctx context.Context, boots []*boot) error {
	var errs []error
	for i := len(boots) - 1; i >= 0; i-- {
		b := boots[i]
		if !b.started {
			continue
		}
		if d.Config.Proxied(b.role) && b.switched {
			if len(b.previous) == 0 {
				d.logf(b.host, "keeping %s: no previous %s container to return to", b.name, b.role)
				continue
			}
			if _, err := d.run(ctx, b.host, d.cmds.proxyDeploy(b.previous[0].Name)); err != nil {
				errs = append(errs, fmt.Errorf("%s: switch back to %s: %w", b.host, b.previous[0].Name, err))
				continue
			}
			d.logf(b.host, "traffic switched back to %s", b.previous[0].Name)
		}
		if _, err := d.run(ctx, b.host, d.cmds.stop(b.name)); err != nil {
			errs = append(errs, err)
			continue
		}
		if !b.existing {
			if _, err := d.run(ctx, b.host, d.cmds.remove(b.name)); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		d.logf(b.host, "removed %s", b.name)
	}
	return errors.Join(errs...)
}

// finish stops previous containers, prunes beyond retain and records the
// deploy on each host. Failures here are warnings: the new version serves.
func (d *Deployer) finish(ctx context.Context, report *Report, hosts []string, plan map[string][]*boot, inventories map[string][]Container) {
	var mu sync.Mutex
	_ = d.eachHost(ctx, hosts, func(ctx context.Context, host string) error {
		containers := slices.Clone(inventories[host])
		roles := map[string]string{}
		for _, b := range plan[host] {
			roles[b.role] = b.name
			var stopped []string
			for _, previous := range b.previous {
				stopped = append(stopped, previous.Name)
			}
			if len(stopped) > 0 {
				if _, err := d.run(ctx, host, d.cmds.stop(stopped...)); err != nil {
					mu.Lock()
					report.warn("%s: stopping %s failed: %v", host, strings.Join(stopped, ", "), err)
					mu.Unlock()
				} else {
					d.logf(host, "stopped %s", strings.Join(stopped, ", "))
				}
			}
			containers = applyBoot(containers, b, d.Now())
			mu.Lock()
			report.Steps = append(report.Steps, RoleReport{Host: host, Role: b.role, Container: b.name, Stopped: nonNil(stopped)})
			mu.Unlock()
		}
		pruned, warnings := d.pruneHost(ctx, host, containers)
		entry, _ := json.Marshal(map[string]any{
			"time": d.Now().UTC(), "action": report.Action, "service": d.Config.Service, "version": report.Version,
			"performer": d.Performer, "host": host, "containers": roles,
		})
		if _, err := d.run(ctx, host, d.cmds.appendLog(append(entry, '\n'))); err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: recording the deploy failed: %v", host, err))
		}
		mu.Lock()
		report.Pruned = append(report.Pruned, pruned...)
		report.Warnings = append(report.Warnings, warnings...)
		mu.Unlock()
		return nil
	})
	slices.SortFunc(report.Steps, func(a, b RoleReport) int {
		return strings.Compare(a.Host+"\x00"+a.Role, b.Host+"\x00"+b.Role)
	})
	slices.Sort(report.Pruned)
}

func nonNil(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}

// applyBoot updates a local inventory after a successful switch, so pruning
// works from the post-deploy state without another query.
func applyBoot(containers []Container, b *boot, now time.Time) []Container {
	for i := range containers {
		for _, previous := range b.previous {
			if containers[i].Name == previous.Name {
				containers[i].State = "exited"
			}
		}
		if containers[i].Name == b.name {
			containers[i].State = "running"
			return containers
		}
	}
	// The new container is the newest whatever the clocks say: the host's
	// clock, which stamped the others, may be ahead of the local one.
	created := now
	for _, c := range containers {
		if !c.Created.Before(created) {
			created = c.Created.Add(time.Second)
		}
	}
	return append([]Container{{Name: b.name, Role: b.role, Version: b.version, State: "running", Created: created}}, containers...)
}

// pruneHost removes stopped containers beyond retain per role, and the
// images no remaining container uses. It never touches running containers.
func (d *Deployer) pruneHost(ctx context.Context, host string, containers []Container) ([]string, []string) {
	var remove []string
	var warnings []string
	keepVersions := map[string]bool{}
	byRole := map[string][]Container{}
	for _, c := range containers {
		byRole[c.Role] = append(byRole[c.Role], c)
	}
	for _, role := range sortedKeys(byRole) {
		list := byRole[role]
		slices.SortStableFunc(list, func(a, b Container) int { return b.Created.Compare(a.Created) })
		for i, c := range list {
			if i < d.Config.Retain || c.Running() {
				keepVersions[c.Version] = true
				continue
			}
			remove = append(remove, c.Name)
		}
	}
	if len(remove) > 0 {
		if _, err := d.run(ctx, host, d.cmds.remove(remove...)); err != nil {
			return nil, []string{fmt.Sprintf("%s: pruning containers failed: %v", host, err)}
		}
		d.logf(host, "pruned %s", strings.Join(remove, ", "))
	}
	result, err := d.run(ctx, host, d.cmds.images())
	if err != nil {
		return prefixed(host, remove), append(warnings, fmt.Sprintf("%s: listing images failed: %v", host, err))
	}
	for line := range strings.Lines(result.Stdout) {
		var image struct{ Repository, Tag string }
		if json.Unmarshal([]byte(line), &image) != nil || image.Tag == "" || image.Tag == "<none>" || keepVersions[image.Tag] {
			continue
		}
		if image.Repository != d.Config.Image {
			continue
		}
		ref := image.Repository + ":" + image.Tag
		if _, err := d.run(ctx, host, d.cmds.removeImage(ref)); err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: removing image %s failed: %v", host, ref, err))
			continue
		}
		remove = append(remove, ref)
	}
	return prefixed(host, remove), warnings
}

func prefixed(host string, names []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, host+":"+name)
	}
	return out
}

// Rollback switches every host back to the retained containers of version.
// The containers restart with the environment they were created with.
// Migrations are not reverted.
func (d *Deployer) Rollback(ctx context.Context, version string) (*Report, error) {
	if err := d.prepare(); err != nil {
		return nil, err
	}
	if !versionPattern.MatchString(version) {
		return nil, fmt.Errorf("version %q must match %s", version, versionPattern)
	}
	report := d.newReport("rollback", version)
	started := d.Now()
	hosts := d.Config.AppHosts()
	release, err := d.acquire(ctx, hosts, "rollback to "+version)
	if err != nil {
		return report, err
	}
	defer release()
	inventories, err := d.inventories(ctx, hosts)
	if err != nil {
		return report, err
	}
	plan := map[string][]*boot{}
	var missing []string
	for _, host := range hosts {
		for _, role := range d.Config.RolesOn(host) {
			var target *Container
			for i, c := range inventories[host] {
				if c.Role == role && c.Version == version {
					target = &inventories[host][i]
					break
				}
			}
			if target == nil {
				missing = append(missing, fmt.Sprintf("%s/%s (available: %s)", host, role, strings.Join(versionsOf(inventories[host], role), ", ")))
				continue
			}
			others := running(inventories[host], role, target.Name)
			if target.Running() && len(others) == 0 {
				d.logf(host, "%s already runs %s", role, version)
				continue
			}
			plan[host] = append(plan[host], &boot{host: host, role: role, version: version, name: target.Name, existing: true, previous: others})
		}
	}
	if len(missing) > 0 {
		return report, fmt.Errorf("version %s is not retained on %s; nothing was changed", version, strings.Join(missing, "; "))
	}
	if err := d.hook(ctx, "pre-deploy", d.hookEnv("rollback", version, time.Time{})); err != nil {
		return report, err
	}
	if err := d.switchAll(ctx, hosts, plan, version); err != nil {
		return report, err
	}
	d.finish(ctx, report, hosts, plan, inventories)
	if err := d.hook(ctx, "post-deploy", d.hookEnv("rollback", version, started)); err != nil {
		return report, err
	}
	report.DurationMS = d.Now().Sub(started).Milliseconds()
	return report, nil
}

func versionsOf(containers []Container, role string) []string {
	var versions []string
	for _, c := range containers {
		if c.Role == role && !slices.Contains(versions, c.Version) {
			versions = append(versions, c.Version)
		}
	}
	if len(versions) == 0 {
		return []string{"none"}
	}
	return versions
}

// Prune removes stopped containers beyond retain and unused images.
func (d *Deployer) Prune(ctx context.Context) (*Report, error) {
	if err := d.prepare(); err != nil {
		return nil, err
	}
	report := d.newReport("prune", "")
	started := d.Now()
	hosts := d.Config.AppHosts()
	release, err := d.acquire(ctx, hosts, "prune")
	if err != nil {
		return report, err
	}
	defer release()
	var mu sync.Mutex
	err = d.eachHost(ctx, hosts, func(ctx context.Context, host string) error {
		containers, err := d.inventory(ctx, host)
		if err != nil {
			return err
		}
		pruned, warnings := d.pruneHost(ctx, host, containers)
		mu.Lock()
		report.Pruned = append(report.Pruned, pruned...)
		report.Warnings = append(report.Warnings, warnings...)
		mu.Unlock()
		return nil
	})
	slices.Sort(report.Pruned)
	report.DurationMS = d.Now().Sub(started).Milliseconds()
	return report, err
}

// Maintenance turns the proxy maintenance page on or off on every web host.
func (d *Deployer) Maintenance(ctx context.Context, on bool) error {
	if err := d.prepare(); err != nil {
		return err
	}
	role := d.Config.PrimaryRole()
	if !d.Config.Proxied(role) {
		return errors.New("no role uses the proxy")
	}
	return d.eachHost(ctx, d.Config.Servers[role].Hosts, func(ctx context.Context, host string) error {
		if _, err := d.run(ctx, host, d.cmds.proxyMaintenance(on)); err != nil {
			return err
		}
		d.logf(host, "maintenance %v", on)
		return nil
	})
}

// HostDetails is the state of one host for `forge deploy details`.
type HostDetails struct {
	Host       string            `json:"host"`
	Roles      []string          `json:"roles"`
	Containers []Container       `json:"containers"`
	Proxy      []json.RawMessage `json:"proxy,omitempty"`
	Lock       *LockInfo         `json:"lock,omitempty"`
	Deploys    []json.RawMessage `json:"recent_deploys"`
	Error      string            `json:"error,omitempty"`
}

// Details collects containers, proxy routing, lock and recent deploys from
// every host.
func (d *Deployer) Details(ctx context.Context) ([]HostDetails, error) {
	if err := d.prepare(); err != nil {
		return nil, err
	}
	hosts := d.Config.AppHosts()
	details := make([]HostDetails, len(hosts))
	var mu sync.Mutex
	index := map[string]int{}
	for i, host := range hosts {
		index[host] = i
		details[i] = HostDetails{Host: host, Roles: d.Config.RolesOn(host), Containers: []Container{}, Deploys: []json.RawMessage{}}
	}
	err := d.eachHost(ctx, hosts, func(ctx context.Context, host string) error {
		hd := HostDetails{Host: host, Roles: d.Config.RolesOn(host), Containers: []Container{}, Deploys: []json.RawMessage{}}
		defer func() {
			mu.Lock()
			details[index[host]] = hd
			mu.Unlock()
		}()
		containers, err := d.inventory(ctx, host)
		if err != nil {
			hd.Error = err.Error()
			return err
		}
		if containers != nil {
			hd.Containers = containers
		}
		if slices.ContainsFunc(hd.Roles, d.Config.Proxied) {
			result, err := d.run(ctx, host, d.cmds.proxyStatus())
			if err != nil {
				hd.Error = "forge-proxy: " + err.Error()
			} else {
				var status struct {
					Services []json.RawMessage `json:"services"`
				}
				if json.Unmarshal([]byte(result.Stdout), &status) == nil {
					hd.Proxy = status.Services
				}
			}
		}
		if result, err := d.run(ctx, host, d.cmds.lockStatus()); err == nil && strings.TrimSpace(result.Stdout) != "" {
			info := parseLockInfo(result.Stdout)
			hd.Lock = &info
		}
		if result, err := d.run(ctx, host, d.cmds.readLog(5)); err == nil {
			for line := range strings.Lines(result.Stdout) {
				if json.Valid([]byte(strings.TrimSpace(line))) {
					hd.Deploys = append(hd.Deploys, json.RawMessage(strings.TrimSpace(line)))
				}
			}
		}
		return nil
	})
	return details, err
}
