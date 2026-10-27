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

	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/dbtest"
	"github.com/rocketinsidecorp/forge-go/internal/waittest"
	"github.com/rocketinsidecorp/forge-go/mysql/mysqltest"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

// The README quickstarts, end to end (decisions D21, D30, D31, D33, D35): a
// fresh `forge new` application per database (SQLite with --auth, MySQL or
// MariaDB with --auth, PostgreSQL with --tenants), the exact command block of
// its README's "Quickstart" section run by a real forge binary with no code
// edited, then a person's HTTP session against `forge dev`: sign in as the
// owner, list, create, show, edit and delete products through the
// CSRF-protected HTML forms, and use /v1/products as JSON. The PostgreSQL
// application also signs in to a second organization and gets 404 for the
// first one's product. A plain `forge new site` must serve its welcome page.
// TestGeneratedApplication runs them (CI's generated and mariadb jobs).
//
// The one line the test does not run as written is the database start,
// `docker compose up -d --wait postgres|mysql`: CI's servers are service
// containers, so the test creates what the compose service's init script
// creates (for PostgreSQL a migrator role that owns the database and schema
// and a runtime role with data privileges only; for MySQL a database) on that
// server, under unique names, and points .env.local at them, as a developer
// with their own server does.

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

// quickstartVariant is one application of the README quickstarts.
type quickstartVariant struct {
	// newLine is the README's first command.
	newLine string
	// database is the server the test provides (postgresql or mysql), or ""
	// for SQLite.
	database string
	// tenants is an application with organizations (forge new --tenants).
	tenants bool
}

// quickstartVariants are the quickstarts CI runs (decisions D21, D30, D33,
// D35): SQLite with --auth and no Docker, MySQL/MariaDB with --auth, and
// PostgreSQL with --tenants.
var quickstartVariants = map[string]quickstartVariant{
	"sqlite3":    {newLine: "forge new shop --auth"},
	"mysql":      {newLine: "forge new shop -d mysql --auth", database: "mysql"},
	"postgresql": {newLine: "forge new shop --tenants", database: "postgresql", tenants: true},
}

const quickstartPassword = "correct horse battery staple"

// quickstartEnvironment is the test's environment without the developer's
// FORGE_ settings, with the forge binary first on PATH.
func quickstartEnvironment(forge, address string) []string {
	var env []string
	for _, entry := range os.Environ() {
		if key, _, _ := strings.Cut(entry, "="); !strings.HasPrefix(key, "FORGE_") || strings.HasPrefix(key, "FORGE_TEST_") {
			env = append(env, entry)
		}
	}
	return append(env, "PATH="+filepath.Dir(forge)+string(os.PathListSeparator)+os.Getenv("PATH"), "FORGE_SETUP_PASSWORD="+quickstartPassword, "FORGE_HTTP_ADDR="+address)
}

func runQuickstartLine(t *testing.T, dir, line string, env []string) string {
	t.Helper()
	command := exec.Command("sh", "-c", line)
	command.Dir, command.Env = dir, env
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", line, err, output)
	}
	return string(output)
}

// startDev runs `forge dev` in app until the test ends and waits until it
// serves /health/ready.
func startDev(t *testing.T, forge, app, address string, env []string) *quickstartBrowser {
	t.Helper()
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
	waittest.Until(t, 5*time.Minute, "forge dev to serve "+base+"/health/ready", func() bool {
		response, err := browser.client.Get(base + "/health/ready")
		if err != nil {
			return false
		}
		_ = response.Body.Close()
		return response.StatusCode == http.StatusOK
	})
	return browser
}

// signIn signs browser in through the sign-in form: with the organization
// in an application with organizations, with the email and password only
// otherwise.
func (b *quickstartBrowser) signIn(org, email, password string, status int) {
	b.t.Helper()
	form := url.Values{"authenticity_token": {b.token("/session/new")}, "email": {email}, "password": {password}}
	if org != "" {
		form.Set("tenant", org)
	}
	b.expect(http.MethodPost, "/session", form, status, "")
}

func testReadmeQuickstart(t *testing.T, frameworkPath, name string) {
	t.Helper()
	variant := quickstartVariants[name]
	// Skips (or fails under FORGE_TEST_REQUIRE_DATABASE) without a server.
	var serverURL string
	switch variant.database {
	case "postgresql":
		serverURL = dbtest.ServerURL(t, config.AdapterPostgres)
	case "mysql":
		serverURL = mysqltest.Config(t).URL.Reveal()
	}
	forge := forgeBinary(t, frameworkPath)
	work := t.TempDir()
	address := freeLoopbackAddress(t)
	env := quickstartEnvironment(forge, address)

	// forge new, as the README's first line says, with the local framework
	// (this checkout is not a published module version).
	runQuickstartLine(t, work, variant.newLine+" --forge-path "+frameworkPath, env)
	app := filepath.Join(work, "shop")
	commands := quickstartCommands(t, readFile(t, filepath.Join(app, "README.md")))
	if len(commands) < 4 || commands[0] != variant.newLine || commands[1] != "cd shop" || commands[len(commands)-1] != "forge dev" {
		t.Fatalf("the quickstart is not `%s`, `cd shop`, ..., `forge dev`:\n%s", variant.newLine, strings.Join(commands, "\n"))
	}
	runtimeURL := ""
	for _, line := range commands[2 : len(commands)-1] {
		switch {
		case line == "docker compose up -d --wait postgres" && variant.database == "postgresql":
			runtimeURL = createQuickstartDatabase(t, serverURL, app)
			continue
		case line == "docker compose up -d --wait mysql" && variant.database == "mysql":
			// A fresh database on the test server, as a developer with
			// their own server points .env.local at it.
			local := filepath.Join(app, ".env.local")
			writeFile(t, local, readFile(t, local)+"FORGE_DATABASE_URL="+serverURL+"\n")
			continue
		case strings.Contains(line, "--public"), strings.Contains(line, "--global"):
			t.Fatalf("the quickstart serves routes without sign-in: %s", line)
		case !strings.HasPrefix(line, "forge "):
			t.Fatalf("the quickstart runs %q, which is not a forge command", line)
		}
		output := runQuickstartLine(t, app, line, env)
		if strings.Contains(output, "Forge did not edit") || strings.Contains(output, "Wire the") {
			t.Fatalf("%s asks for a manual edit:\n%s", line, output)
		}
	}
	org := ""
	if variant.tenants {
		org = "acme"
	}

	browser := startDev(t, forge, app, address, env)
	// Signed out: the pages send the browser to sign in, JSON gets 401.
	browser.expect(http.MethodGet, "/products", nil, http.StatusSeeOther, "")
	browser.expectJSON(http.MethodGet, "/v1/products", "", "", http.StatusUnauthorized)
	signInPage := browser.page("/session/new")
	if strings.Contains(signInPage, `name="tenant"`) != variant.tenants {
		t.Fatalf("the sign-in page asks for an organization = %v, want %v:\n%s", !variant.tenants, variant.tenants, signInPage)
	}
	browser.signIn(org, "you@example.com", "wrong password", http.StatusUnprocessableEntity)
	browser.signIn(org, "you@example.com", quickstartPassword, http.StatusSeeOther)
	chair := quickstartCRUD(t, browser)

	if !variant.tenants {
		return
	}
	// Another organization's owner sees none of acme's rows and changes
	// none of them: every one answers 404.
	createSecondOrganization(t, runtimeURL)
	other := newQuickstartBrowser(t, browser.base)
	other.signIn("globex", "owner@globex.example", quickstartPassword, http.StatusSeeOther)
	other.expect(http.MethodGet, "/products", nil, http.StatusOK, "Products")
	if page := other.page("/products"); strings.Contains(page, "Chair") {
		t.Fatalf("globex lists acme's products:\n%s", page)
	}
	other.expect(http.MethodGet, "/products/"+chair, nil, http.StatusNotFound, "")
	other.expectJSON(http.MethodGet, "/v1/products/"+chair, "", "", http.StatusNotFound)
	csrfHeader := other.cookies["__Host-forge_csrf"]
	other.expectJSON(http.MethodPatch, "/v1/products/"+chair, `{"version":1,"name":"Stolen"}`, csrfHeader, http.StatusNotFound)
	other.expectJSON(http.MethodDelete, "/v1/products/"+chair+"?version=1", "", csrfHeader, http.StatusNotFound)
	var list struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(other.expectJSON(http.MethodGet, "/v1/products", "", "", http.StatusOK), &list); err != nil || len(list.Items) != 0 {
		t.Fatalf("globex GET /v1/products = %+v (%v)", list, err)
	}
	// acme's row is untouched.
	browser.expect(http.MethodGet, "/products/"+chair, nil, http.StatusOK, "Chair")
}

// quickstartCRUD lists, creates, shows, edits and deletes a product through
// the CSRF-protected HTML forms, then creates and lists one as JSON with the
// session cookie and the CSRF header, as a signed-in browser. It returns the
// JSON product's ID.
func quickstartCRUD(t *testing.T, browser *quickstartBrowser) string {
	t.Helper()
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

	// JSON, as the same signed-in owner: the session cookie, and the CSRF
	// token on changes.
	browser.expectJSON(http.MethodPost, "/v1/products", `{"name":"Chair","price":4900}`, "", http.StatusForbidden)
	csrfHeader := browser.cookies["__Host-forge_csrf"]
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
	browser.expectJSON(http.MethodPatch, "/v1/products/"+chair.ID, `{"version":1,"price":5900}`, csrfHeader, http.StatusOK)
	browser.expect(http.MethodGet, "/products", nil, http.StatusOK, "Chair")
	return chair.ID
}

// createSecondOrganization adds the organization globex and its owner,
// holding every permission acme's owner holds, as an operator would: setup
// creates only the first one.
func createSecondOrganization(t *testing.T, runtimeURL string) {
	t.Helper()
	hash, err := auth.DefaultPasswordHasher().Hash(context.Background(), quickstartPassword)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), dbtest.SetupTimeout)
	defer cancel()
	conn, err := pgx.Connect(ctx, runtimeURL)
	if err != nil {
		t.Fatalf("connect as the runtime role: %v", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	org, tenant, user, membership, role := uuid.MustNew(), uuid.MustNew(), uuid.MustNew(), uuid.MustNew(), uuid.MustNew()
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{"INSERT INTO forge_organizations (id, name) VALUES ($1, 'Globex')", []any{org}},
		{"INSERT INTO forge_tenants (id, organization_id, slug) VALUES ($1, $2, 'globex')", []any{tenant, org}},
		{"INSERT INTO forge_users (id, email, email_normalized, password_hash) VALUES ($1, 'owner@globex.example', 'owner@globex.example', $2)", []any{user, hash}},
		{"INSERT INTO forge_memberships (id, user_id, tenant_id) VALUES ($1, $2, $3)", []any{membership, user, tenant}},
		{"INSERT INTO forge_roles (id, tenant_id, name) VALUES ($1, $2, 'owner')", []any{role, tenant}},
		{"INSERT INTO forge_role_permissions (role_id, permission) SELECT $1, permission FROM forge_role_permissions WHERE role_id = (SELECT r.id FROM forge_roles r JOIN forge_tenants t ON t.id = r.tenant_id WHERE t.slug = 'acme' AND r.name = 'owner')", []any{role}},
		{"INSERT INTO forge_membership_roles (membership_id, role_id, tenant_id) VALUES ($1, $2, $3)", []any{membership, role, tenant}},
	} {
		if _, err := conn.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("%s: %v", statement.sql, err)
		}
	}
}

// testWelcomeQuickstart: `forge new site` (no authentication, SQLite) and
// `forge dev` serve the welcome page.
func testWelcomeQuickstart(t *testing.T, frameworkPath string) {
	t.Helper()
	forge := forgeBinary(t, frameworkPath)
	work := t.TempDir()
	address := freeLoopbackAddress(t)
	env := quickstartEnvironment(forge, address)
	runQuickstartLine(t, work, "forge new site --forge-path "+frameworkPath, env)
	app := filepath.Join(work, "site")
	if _, err := os.Stat(filepath.Join(app, "app", "adapters", "htmlauth")); !os.IsNotExist(err) {
		t.Fatalf("forge new site generated sign-in pages: %v", err)
	}
	browser := startDev(t, forge, app, address, env)
	browser.expect(http.MethodGet, "/", nil, http.StatusOK, "site")
	browser.expect(http.MethodGet, "/session/new", nil, http.StatusNotFound, "")
}

// createQuickstartDatabase does what the compose service's init.sql does,
// under unique role names, on the test server at serverURL, points the
// application's .env.local at it and returns the runtime role's URL.
func createQuickstartDatabase(t *testing.T, serverURL, app string) string {
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
	return as(runtime, secret)
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
