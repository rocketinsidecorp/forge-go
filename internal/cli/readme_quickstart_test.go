package cli

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/dbtest"
	"github.com/rocketinsidecorp/forge-go/internal/waittest"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

// The README quickstart, end to end (decision D21): a fresh `forge new`
// application, the exact command block of its README's "Quickstart" section
// run by a real forge binary with no code edited, then a person's HTTP
// session against `forge dev`: sign in as the owner (PostgreSQL), list,
// create, show, edit and delete products through the CSRF-protected HTML
// forms, and use /v1/products as JSON. TestGeneratedApplication runs it (CI's
// generated job).
//
// The one line the test does not run as written is the database start,
// `docker compose up -d --wait postgres`: CI's PostgreSQL is a service
// container, so the test creates what the compose service's
// docker/postgres/init.sql creates (a migrator role that owns the database
// and schema, a runtime role with data privileges only) on that server, under
// unique names, and points .env.local at them, as a developer with their own
// server does.

// forgeBinary builds the forge CLI into the test's temporary directory, which
// is removed when the test ends.
func forgeBinary(t *testing.T, frameworkPath string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "forge")
	command := exec.Command("go", "build", "-o", path, "./cmd/forge")
	command.Dir = frameworkPath
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build the forge CLI: %v\n%s", err, output)
	}
	return path
}

// quickstartCommands is the first bash block after "## Quickstart".
func quickstartCommands(t *testing.T, readme string) []string {
	t.Helper()
	_, section, ok := strings.Cut(readme, "\n## Quickstart\n")
	if !ok {
		t.Fatalf("the README has no Quickstart section:\n%s", readme)
	}
	_, block, ok := strings.Cut(section, "```bash\n")
	if !ok {
		t.Fatal("the Quickstart section has no bash block")
	}
	block, _, _ = strings.Cut(block, "```")
	return strings.Split(strings.TrimSpace(block), "\n")
}

func testReadmeQuickstart(t *testing.T, frameworkPath, database string) {
	t.Helper()
	identity := database == "postgresql"
	var databaseURL string
	if identity {
		// Skips (or fails under FORGE_TEST_REQUIRE_DATABASE) without a server.
		databaseURL = dbtest.ServerURL(t, config.AdapterPostgres)
	}
	forge := forgeBinary(t, frameworkPath)
	work := t.TempDir()
	address := freeLoopbackAddress(t)
	const password = "correct horse battery staple"
	var env []string
	for _, entry := range os.Environ() {
		if key, _, _ := strings.Cut(entry, "="); !strings.HasPrefix(key, "FORGE_") || strings.HasPrefix(key, "FORGE_TEST_") {
			env = append(env, entry)
		}
	}
	env = append(env, "PATH="+filepath.Dir(forge)+string(os.PathListSeparator)+os.Getenv("PATH"), "FORGE_SETUP_PASSWORD="+password, "FORGE_HTTP_ADDR="+address)
	run := func(dir, line string) string {
		t.Helper()
		command := exec.Command("sh", "-c", line)
		command.Dir, command.Env = dir, env
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", line, err, output)
		}
		return string(output)
	}

	// forge new, as the README's first line says, with the local framework
	// (this checkout is not a published module version).
	newLine := "forge new shop"
	if !identity {
		newLine += " -d sqlite3"
	}
	run(work, newLine+" --forge-path "+frameworkPath)
	app := filepath.Join(work, "shop")
	commands := quickstartCommands(t, readFile(t, filepath.Join(app, "README.md")))
	if len(commands) < 4 || commands[0] != newLine || commands[1] != "cd shop" || commands[len(commands)-1] != "forge dev" {
		t.Fatalf("the quickstart is not `%s`, `cd shop`, ..., `forge dev`:\n%s", newLine, strings.Join(commands, "\n"))
	}
	for _, line := range commands[2 : len(commands)-1] {
		if line == "docker compose up -d --wait postgres" && identity {
			createQuickstartDatabase(t, databaseURL, app)
			continue
		}
		if !strings.HasPrefix(line, "forge ") {
			t.Fatalf("the quickstart runs %q, which is not a forge command", line)
		}
		output := run(app, line)
		if strings.Contains(output, "Forge did not edit") || strings.Contains(output, "Wire the") {
			t.Fatalf("%s asks for a manual edit:\n%s", line, output)
		}
	}

	// forge dev, until the test ends.
	ctx, cancel := context.WithCancel(context.Background())
	dev := exec.CommandContext(ctx, forge, "dev")
	dev.Dir, dev.Env = app, env
	var logs lockedBuffer
	dev.Stdout, dev.Stderr = &logs, &logs
	dev.Cancel = func() error { return dev.Process.Signal(os.Interrupt) }
	dev.WaitDelay = 30 * time.Second
	if err := dev.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		_ = dev.Wait()
		if t.Failed() {
			t.Logf("forge dev output:\n%s", logs.String())
		}
	})
	base := "http://" + address
	browser := newQuickstartBrowser(t, base)
	waitTimeout := 5 * time.Minute
	waittest.Until(t, waitTimeout, "forge dev to serve "+base+"/health/ready", func() bool {
		response, err := browser.client.Get(base + "/health/ready")
		if err != nil {
			return false
		}
		_ = response.Body.Close()
		return response.StatusCode == http.StatusOK
	})

	if identity {
		// Signed out: the pages send the browser to sign in, JSON gets 401.
		browser.expect(http.MethodGet, "/products", nil, http.StatusSeeOther, "")
		browser.expectJSON(http.MethodGet, "/v1/products", "", "", http.StatusUnauthorized)
		token := browser.token("/session/new")
		browser.expect(http.MethodPost, "/session", url.Values{"authenticity_token": {token}, "tenant": {"acme"}, "email": {"you@example.com"}, "password": {"wrong password"}}, http.StatusUnprocessableEntity, "")
		token = browser.token("/session/new")
		browser.expect(http.MethodPost, "/session", url.Values{"authenticity_token": {token}, "tenant": {"acme"}, "email": {"you@example.com"}, "password": {password}}, http.StatusSeeOther, "")
	}
	browser.expect(http.MethodGet, "/products", nil, http.StatusOK, "Products")
	// Forms need their CSRF token.
	browser.expect(http.MethodPost, "/products", url.Values{"name": {"Lamp"}, "price": {"1999"}}, http.StatusForbidden, "")
	created := browser.expect(http.MethodPost, "/products", url.Values{"authenticity_token": {browser.token("/products/new")}, "name": {"Lamp"}, "price": {"1999"}}, http.StatusSeeOther, "")
	show := created.Header.Get("Location")
	if !regexp.MustCompile(`^/products/[0-9a-f-]{36}$`).MatchString(show) {
		t.Fatalf("create redirected to %q", show)
	}
	browser.expect(http.MethodGet, show, nil, http.StatusOK, "Lamp")
	edit := browser.page(show + "/edit")
	browser.expect(http.MethodPost, show, url.Values{
		"authenticity_token": {quickstartField(t, edit, "authenticity_token")}, "_method": {"PATCH"}, "version": {quickstartField(t, edit, "version")},
		"name": {"Desk lamp"}, "price": {"2499"},
	}, http.StatusSeeOther, "")
	browser.expect(http.MethodGet, show, nil, http.StatusOK, "Desk lamp")
	page := browser.page(show)
	browser.expect(http.MethodPost, show, url.Values{
		"authenticity_token": {quickstartField(t, page, "authenticity_token")}, "_method": {"DELETE"}, "version": {quickstartField(t, page, "version")},
	}, http.StatusSeeOther, "")
	browser.expect(http.MethodGet, show, nil, http.StatusNotFound, "")

	// JSON, as the same signed-in owner (the session cookie, and the CSRF
	// token on changes) or, without identity, as anyone (--public).
	csrfHeader := ""
	if identity {
		browser.expectJSON(http.MethodPost, "/v1/products", `{"name":"Chair","price":4900}`, "", http.StatusForbidden)
		csrfHeader = browser.cookies["__Host-forge_csrf"]
	}
	body := browser.expectJSON(http.MethodPost, "/v1/products", `{"name":"Chair","price":4900}`, csrfHeader, http.StatusCreated)
	var chair struct {
		ID    string `json:"id"`
		Price int64  `json:"price"`
	}
	if err := json.Unmarshal(body, &chair); err != nil || chair.Price != 4900 || chair.ID == "" {
		t.Fatalf("POST /v1/products answered %s (%v)", body, err)
	}
	var list struct {
		Items []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"items"`
	}
	if err := json.Unmarshal(browser.expectJSON(http.MethodGet, "/v1/products", "", "", http.StatusOK), &list); err != nil || len(list.Items) != 1 || list.Items[0].ID != chair.ID {
		t.Fatalf("GET /v1/products = %+v (%v)", list, err)
	}
	browser.expect(http.MethodGet, "/products", nil, http.StatusOK, "Chair")
}

// createQuickstartDatabase does what the compose service's init.sql does,
// under unique role names, on the test server at serverURL, and points the
// application's .env.local at it.
func createQuickstartDatabase(t *testing.T, serverURL, app string) {
	t.Helper()
	suffix := strings.ReplaceAll(uuid.MustNew().String(), "-", "")[:16]
	migrator, runtime, name := "quickstart_migrator_"+suffix, "quickstart_runtime_"+suffix, "quickstart_"+suffix
	secret := strings.ReplaceAll(uuid.MustNew().String(), "-", "")
	ctx, cancel := context.WithTimeout(context.Background(), dbtest.SetupTimeout)
	defer cancel()
	execute := func(target string, statements ...string) {
		t.Helper()
		conn, err := pgx.Connect(ctx, target)
		if err != nil {
			t.Fatalf("connect to the PostgreSQL test server: %v", err)
		}
		defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
		for _, statement := range statements {
			if _, err := conn.Exec(ctx, statement); err != nil {
				t.Fatalf("%s: %v", statement, err)
			}
		}
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), dbtest.SetupTimeout)
		defer cancel()
		conn, err := pgx.Connect(ctx, serverURL)
		if err != nil {
			t.Errorf("drop the quickstart database: %v", err)
			return
		}
		defer func() { _ = conn.Close(ctx) }()
		for _, statement := range []string{"DROP DATABASE IF EXISTS " + name + " WITH (FORCE)", "DROP ROLE IF EXISTS " + runtime, "DROP ROLE IF EXISTS " + migrator} {
			if _, err := conn.Exec(ctx, statement); err != nil {
				t.Errorf("%s: %v", statement, err)
			}
		}
	})
	// Names and the password are generated above, never input.
	execute(serverURL,
		"CREATE ROLE "+migrator+" LOGIN PASSWORD '"+secret+"' NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS",
		"CREATE ROLE "+runtime+" LOGIN PASSWORD '"+secret+"' NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS",
		"CREATE DATABASE "+name+" OWNER "+migrator,
		"REVOKE TEMPORARY ON DATABASE "+name+" FROM PUBLIC",
	)
	parsed, err := url.Parse(serverURL)
	if err != nil {
		t.Fatal("the PostgreSQL test server URL is not valid") // not wrapped: may contain a password
	}
	as := func(role, password string) string {
		target := *parsed
		target.Path = "/" + name
		if role != "" {
			target.User = url.UserPassword(role, password)
		}
		return target.String()
	}
	execute(as("", ""),
		"REVOKE ALL ON SCHEMA public FROM PUBLIC",
		"GRANT ALL ON SCHEMA public TO "+migrator,
		"GRANT USAGE ON SCHEMA public TO "+runtime,
		"ALTER DEFAULT PRIVILEGES FOR ROLE "+migrator+" IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO "+runtime,
		"ALTER DEFAULT PRIVILEGES FOR ROLE "+migrator+" IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO "+runtime,
	)
	local := filepath.Join(app, ".env.local")
	writeFile(t, local, readFile(t, local)+"FORGE_DATABASE_URL="+as(runtime, secret)+"\nFORGE_MIGRATION_DATABASE_URL="+as(migrator, secret)+"\n")
}

func freeLoopbackAddress(t *testing.T) string {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

// quickstartBrowser keeps cookies as a browser on http://localhost does: the
// __Host- cookies are Secure, which net/http/cookiejar never sends over
// plain HTTP, while browsers treat loopback as a secure context.
type quickstartBrowser struct {
	t       *testing.T
	base    string
	client  *http.Client
	cookies map[string]string
}

func newQuickstartBrowser(t *testing.T, base string) *quickstartBrowser {
	return &quickstartBrowser{t: t, base: base, cookies: make(map[string]string), client: &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (b *quickstartBrowser) do(method, path, contentType string, body io.Reader, header http.Header) (*http.Response, []byte) {
	b.t.Helper()
	request, err := http.NewRequestWithContext(context.Background(), method, b.base+path, body)
	if err != nil {
		b.t.Fatal(err)
	}
	for key, values := range header {
		request.Header[key] = values
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	for name, value := range b.cookies {
		request.AddCookie(&http.Cookie{Name: name, Value: value})
	}
	response, err := b.client.Do(request)
	if err != nil {
		b.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = response.Body.Close() }()
	content, err := io.ReadAll(response.Body)
	if err != nil {
		b.t.Fatal(err)
	}
	for _, cookie := range response.Cookies() {
		if cookie.MaxAge < 0 || cookie.Value == "" {
			delete(b.cookies, cookie.Name)
		} else {
			b.cookies[cookie.Name] = cookie.Value
		}
	}
	return response, content
}

// expect sends a browser request (a form when values is not nil) and checks
// the status and, when contains is set, the body.
func (b *quickstartBrowser) expect(method, path string, values url.Values, status int, contains string) *http.Response {
	b.t.Helper()
	var body io.Reader
	contentType := ""
	if values != nil {
		body, contentType = strings.NewReader(values.Encode()), "application/x-www-form-urlencoded"
	}
	response, content := b.do(method, path, contentType, body, http.Header{"Accept": {"text/html"}})
	if response.StatusCode != status || !strings.Contains(string(content), contains) {
		b.t.Fatalf("%s %s = %d, want %d with %q:\n%s", method, path, response.StatusCode, status, contains, content)
	}
	return response
}

func (b *quickstartBrowser) expectJSON(method, path, body, csrfToken string, status int) []byte {
	b.t.Helper()
	header := http.Header{"Accept": {"application/json"}}
	if csrfToken != "" {
		header.Set("X-CSRF-Token", csrfToken)
	}
	contentType := ""
	if body != "" {
		contentType = "application/json"
	}
	response, content := b.do(method, path, contentType, strings.NewReader(body), header)
	if response.StatusCode != status {
		b.t.Fatalf("%s %s (JSON) = %d, want %d:\n%s", method, path, response.StatusCode, status, content)
	}
	return content
}

// page is a page's HTML, which must load.
func (b *quickstartBrowser) page(path string) string {
	b.t.Helper()
	response, content := b.do(http.MethodGet, path, "", nil, http.Header{"Accept": {"text/html"}})
	if response.StatusCode != http.StatusOK {
		b.t.Fatalf("GET %s = %d:\n%s", path, response.StatusCode, content)
	}
	return string(content)
}

// token is the CSRF token of the first form on the page at path.
func (b *quickstartBrowser) token(path string) string {
	b.t.Helper()
	return quickstartField(b.t, b.page(path), "authenticity_token")
}

func quickstartField(t *testing.T, page, name string) string {
	t.Helper()
	match := regexp.MustCompile(`name="` + regexp.QuoteMeta(name) + `" value="([^"]*)"`).FindStringSubmatch(page)
	if match == nil {
		t.Fatalf("the page has no %s field:\n%s", name, page)
	}
	return match[1]
}
