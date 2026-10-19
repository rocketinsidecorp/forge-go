//go:build unix

// Command fakedocker stands in for the docker CLI in the SSH integration
// tests of internal/deploy, where no Docker daemon is available. It keeps
// containers, images, networks and forge-proxy routes in a JSON file in
// $FAKE_DOCKER_STATE and implements exactly the invocations `forge deploy`
// issues, failing on anything else so the tests notice drift.
//
// Behaviour switches for failure tests:
//   - an image tag containing "badmigrate" fails `run --rm ... migrate`;
//   - a file named "unhealthy" in the state directory makes forge-proxy
//     deploy exit 4, like a target that never passes its health check;
//   - an image tag containing "unready" reports non-proxied containers as
//     exited.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

type container struct {
	Name    string            `json:"name"`
	Image   string            `json:"image"`
	Labels  map[string]string `json:"labels"`
	State   string            `json:"state"`
	Seq     int               `json:"seq"`
	Created time.Time         `json:"created"`
	EnvFile string            `json:"env_file"`
	Args    []string          `json:"args"`
	Publish []string          `json:"publish"`
}

type state struct {
	Seq         int               `json:"seq"`
	Networks    []string          `json:"networks"`
	Images      []string          `json:"images"`
	Containers  []*container      `json:"containers"`
	Routes      map[string]string `json:"routes"`
	Maintenance map[string]bool   `json:"maintenance"`
	Logins      []string          `json:"logins"`
	OneOffs     [][]string        `json:"one_offs"`
	EnvSeen     map[string]string `json:"env_seen"`
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	dir := os.Getenv("FAKE_DOCKER_STATE")
	if dir == "" {
		fmt.Fprintln(os.Stderr, "fakedocker: FAKE_DOCKER_STATE is not set")
		return 125
	}
	appendCall(dir, args)
	// Streaming commands must not hold the state lock while they block.
	if len(args) > 0 && args[0] == "logs" {
		return logs(dir, args[1:])
	}
	lock, err := os.OpenFile(filepath.Join(dir, "state.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 125
	}
	defer func() { _ = lock.Close() }()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 125
	}
	st := load(dir)
	code := dispatch(dir, st, args)
	save(dir, st)
	return code
}

func appendCall(dir string, args []string) {
	file, err := os.OpenFile(filepath.Join(dir, "calls.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer func() { _ = file.Close() }()
	_, _ = fmt.Fprintln(file, strings.Join(args, " "))
}

func load(dir string) *state {
	st := &state{Routes: map[string]string{}, Maintenance: map[string]bool{}, EnvSeen: map[string]string{}}
	content, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err == nil {
		_ = json.Unmarshal(content, st)
	}
	if st.Routes == nil {
		st.Routes = map[string]string{}
	}
	if st.Maintenance == nil {
		st.Maintenance = map[string]bool{}
	}
	if st.EnvSeen == nil {
		st.EnvSeen = map[string]string{}
	}
	return st
}

func save(dir string, st *state) {
	content, _ := json.MarshalIndent(st, "", "  ")
	_ = os.WriteFile(filepath.Join(dir, "state.json"), content, 0o600)
}

func fail(code int, format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "fakedocker: "+format+"\n", args...)
	return code
}

func (st *state) find(name string) *container {
	for _, c := range st.Containers {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func dispatch(dir string, st *state, args []string) int {
	if len(args) == 0 {
		return fail(125, "no command")
	}
	switch args[0] {
	case "version":
		fmt.Println("27.3.1")
		return 0
	case "network":
		if len(args) == 3 && args[1] == "inspect" {
			if slices.Contains(st.Networks, args[2]) {
				return 0
			}
			return 1
		}
		if len(args) == 3 && args[1] == "create" {
			st.Networks = append(st.Networks, args[2])
			return 0
		}
	case "login":
		password, _ := io.ReadAll(os.Stdin)
		if !slices.Contains(args, "--password-stdin") || len(password) == 0 {
			return fail(1, "login needs --password-stdin and a password on stdin")
		}
		st.Logins = append(st.Logins, strings.Join(args[1:], " ")+" password="+string(password))
		return 0
	case "pull":
		if len(args) != 2 {
			break
		}
		if strings.Contains(args[1], "missing") {
			return fail(1, "manifest for %s not found", args[1])
		}
		if !slices.Contains(st.Images, args[1]) {
			st.Images = append(st.Images, args[1])
		}
		return 0
	case "run":
		return runContainer(st, args[1:])
	case "container":
		if len(args) == 3 && args[1] == "inspect" {
			if st.find(args[2]) != nil {
				return 0
			}
			return 1
		}
	case "start":
		for _, name := range args[1:] {
			c := st.find(name)
			if c == nil {
				return fail(1, "no such container %s", name)
			}
			c.State = "running"
		}
		return 0
	case "stop":
		if len(args) < 4 || args[1] != "-t" {
			break
		}
		for _, name := range args[3:] {
			c := st.find(name)
			if c == nil {
				return fail(1, "no such container %s", name)
			}
			c.State = "exited"
		}
		return 0
	case "rm":
		for _, name := range args[1:] {
			c := st.find(name)
			if c == nil {
				return fail(1, "no such container %s", name)
			}
			if c.State == "running" {
				return fail(1, "cannot remove running container %s", name)
			}
			st.Containers = slices.DeleteFunc(st.Containers, func(x *container) bool { return x.Name == name })
		}
		return 0
	case "ps":
		return ps(st, args[1:])
	case "inspect":
		if len(args) == 4 && args[1] == "--format" {
			c := st.find(args[3])
			if c == nil {
				return fail(1, "no such object %s", args[3])
			}
			switch {
			case strings.Contains(args[2], ".Config.Image"):
				fmt.Println(c.Image)
			case strings.Contains(args[2], ".State.Health"):
				if strings.Contains(c.Image, "unready") {
					fmt.Println("exited none")
				} else {
					fmt.Println(c.State + " healthy")
				}
			}
			return 0
		}
	case "image":
		return image(st, args[1:])
	case "exec":
		return execute(dir, st, args[1:])
	}
	return fail(125, "unsupported invocation: %s", strings.Join(args, " "))
}

func runContainer(st *state, args []string) int {
	c := &container{Labels: map[string]string{}}
	rm, detach := false, false
	i := 0
	for ; i < len(args) && strings.HasPrefix(args[i], "-"); i++ {
		switch args[i] {
		case "--rm":
			rm = true
		case "--detach":
			detach = true
		case "--interactive", "--tty":
		case "--name", "--network", "--restart", "--stop-timeout", "--env-file", "--label", "--volume", "--publish":
			if i+1 >= len(args) {
				return fail(125, "flag %s needs a value", args[i])
			}
			value := args[i+1]
			switch args[i] {
			case "--name":
				c.Name = value
			case "--env-file":
				c.EnvFile = value
			case "--label":
				key, val, _ := strings.Cut(value, "=")
				c.Labels[key] = val
			case "--publish":
				c.Publish = append(c.Publish, value)
			}
			i++
		default:
			return fail(125, "unsupported run flag %s", args[i])
		}
	}
	if i >= len(args) {
		return fail(125, "run needs an image")
	}
	c.Image, c.Args = args[i], args[i+1:]
	if c.EnvFile != "" {
		info, err := os.Stat(c.EnvFile)
		if err != nil {
			return fail(125, "env file %s: %v", c.EnvFile, err)
		}
		if info.Mode().Perm() != 0o600 {
			return fail(125, "env file %s has mode %04o, want 0600", c.EnvFile, info.Mode().Perm())
		}
		content, _ := os.ReadFile(c.EnvFile)
		st.EnvSeen[c.Name] = string(content)
	}
	if rm {
		st.OneOffs = append(st.OneOffs, append([]string{c.Image}, c.Args...))
		if len(c.Args) > 0 && c.Args[0] == "migrate" && strings.Contains(c.Image, "badmigrate") {
			return fail(1, "migration 0002_add_index failed")
		}
		fmt.Printf("ran %s\n", strings.Join(c.Args, " "))
		return 0
	}
	if !detach || c.Name == "" {
		return fail(125, "run must be --rm or --detach --name")
	}
	if st.find(c.Name) != nil {
		return fail(125, "container name %s is already in use", c.Name)
	}
	st.Seq++
	c.Seq, c.State, c.Created = st.Seq, "running", time.Now().Add(time.Duration(st.Seq)*time.Second)
	st.Containers = append(st.Containers, c)
	fmt.Println(c.Name)
	return 0
}

func filters(args []string) (map[string]string, map[string]bool, string) {
	labels := map[string]string{}
	flags := map[string]bool{}
	format := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--filter":
			i++
			key, value, _ := strings.Cut(args[i], "=")
			if key == "label" {
				k, v, _ := strings.Cut(value, "=")
				labels[k] = v
			} else {
				labels["@"+key] = value
			}
		case "--format":
			i++
			format = args[i]
		default:
			flags[args[i]] = true
		}
	}
	return labels, flags, format
}

func ps(st *state, args []string) int {
	labels, flags, format := filters(args)
	var matched []*container
	for _, c := range st.Containers {
		ok := true
		for key, value := range labels {
			if key == "@status" {
				ok = ok && c.State == value
				continue
			}
			ok = ok && c.Labels[key] == value
		}
		if ok {
			matched = append(matched, c)
		}
	}
	slices.SortFunc(matched, func(a, b *container) int { return b.Seq - a.Seq })
	if flags["--latest"] && len(matched) > 1 {
		matched = matched[:1]
	}
	for _, c := range matched {
		if flags["--quiet"] {
			fmt.Println(c.Name)
			continue
		}
		if format != "{{json .}}" {
			return fail(125, "unsupported ps format %q", format)
		}
		var pairs []string
		for _, key := range slices.Sorted(func(yield func(string) bool) {
			for k := range c.Labels {
				if !yield(k) {
					return
				}
			}
		}) {
			pairs = append(pairs, key+"="+c.Labels[key])
		}
		line, _ := json.Marshal(map[string]string{
			"Names": c.Name, "Image": c.Image, "State": c.State, "Labels": strings.Join(pairs, ","),
			"CreatedAt": c.Created.UTC().Format("2006-01-02 15:04:05 -0700 MST"),
		})
		fmt.Println(string(line))
	}
	return 0
}

func image(st *state, args []string) int {
	if len(args) == 4 && args[0] == "ls" && args[1] == "--format" {
		for _, ref := range st.Images {
			repo, tag, _ := strings.Cut(ref, ":")
			if repo != args[3] {
				continue
			}
			line, _ := json.Marshal(map[string]string{"Repository": repo, "Tag": tag})
			fmt.Println(string(line))
		}
		return 0
	}
	if len(args) == 2 && args[0] == "rm" {
		for _, c := range st.Containers {
			if c.Image == args[1] {
				return fail(1, "image %s is being used by container %s", args[1], c.Name)
			}
		}
		if !slices.Contains(st.Images, args[1]) {
			return fail(1, "no such image %s", args[1])
		}
		st.Images = slices.DeleteFunc(st.Images, func(x string) bool { return x == args[1] })
		return 0
	}
	return fail(125, "unsupported image invocation")
}

func execute(dir string, st *state, args []string) int {
	for len(args) > 0 && (args[0] == "--interactive" || args[0] == "--tty") {
		args = args[1:]
	}
	if len(args) < 2 {
		return fail(125, "exec needs a container and a command")
	}
	target := st.find(args[0])
	if target == nil || target.State != "running" {
		return fail(1, "container %s is not running", args[0])
	}
	if args[0] != "forge-proxy" {
		fmt.Printf("exec in %s: %s\n", target.Name, strings.Join(args[1:], " "))
		return 0
	}
	if args[1] != "/usr/local/bin/forge-proxy" || len(args) < 3 {
		return fail(125, "unexpected forge-proxy invocation")
	}
	options := map[string]string{}
	rest := args[3:]
	var positional []string
	for i := 0; i < len(rest); i++ {
		if strings.HasPrefix(rest[i], "-") {
			if rest[i] == "-tls" {
				options["tls"] = "true"
				continue
			}
			if i+1 < len(rest) {
				options[strings.TrimPrefix(rest[i], "-")] = rest[i+1]
				i++
			}
			continue
		}
		positional = append(positional, rest[i])
	}
	switch args[2] {
	case "deploy":
		name, _, _ := strings.Cut(options["target"], ":")
		app := st.find(name)
		if app == nil || app.State != "running" {
			return fail(4, "target %s not healthy: request failed", options["target"])
		}
		if _, err := os.Stat(filepath.Join(dir, "unhealthy")); err == nil || strings.Contains(app.Image, "unhealthy") {
			return fail(4, "target %s not healthy within %s: status 503", options["target"], options["deploy-timeout"])
		}
		previous := st.Routes[options["service"]]
		st.Routes[options["service"]] = options["target"]
		out, _ := json.Marshal(map[string]any{"ok": true, "deploy": map[string]any{"service": options["service"], "target": options["target"], "previous": previous, "drained": true}})
		fmt.Println(string(out))
		return 0
	case "status":
		var services []map[string]any
		for _, service := range slices.Sorted(func(yield func(string) bool) {
			for k := range st.Routes {
				if !yield(k) {
					return
				}
			}
		}) {
			services = append(services, map[string]any{"service": service, "target": st.Routes[service], "hosts": []string{}, "maintenance": st.Maintenance[service]})
		}
		out, _ := json.Marshal(map[string]any{"ok": true, "services": services})
		fmt.Println(string(out))
		return 0
	case "maintenance":
		if len(positional) != 1 {
			return fail(2, "maintenance needs on|off")
		}
		st.Maintenance[options["service"]] = positional[0] == "on"
		fmt.Println(`{"ok":true}`)
		return 0
	}
	return fail(125, "unsupported forge-proxy command %s", args[2])
}

// logs prints a few lines, including the container's database URL so the
// tests can check redaction, then blocks with --follow until killed.
func logs(dir string, args []string) int {
	follow := slices.Contains(args, "--follow")
	name := args[len(args)-1]
	st := load(dir)
	c := st.find(name)
	if c == nil {
		return fail(1, "no such container %s", name)
	}
	secret := ""
	for line := range strings.Lines(st.EnvSeen[name]) {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "FORGE_DATABASE_URL="); ok {
			secret = value
		}
	}
	fmt.Printf("2026-10-19T10:00:00Z booted %s\n", name)
	fmt.Printf("2026-10-19T10:00:01Z connecting to %s\n", secret)
	fmt.Printf("2026-10-19T10:00:02Z ERROR something failed in %s\n", name)
	if !follow {
		return 0
	}
	_ = os.WriteFile(filepath.Join(dir, fmt.Sprintf("logs-%d.pid", os.Getpid())), []byte(fmt.Sprint(os.Getpid())), 0o600)
	for {
		time.Sleep(time.Hour)
	}
}
