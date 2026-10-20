package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// scaffoldBroadcastE2ETest drives the real wired application over HTTP: the
// owner signs in, the index page hands out a signed stream name, the event
// stream accepts it only with the session, and creating a product through the
// form reaches the open stream as a Turbo Stream.
const scaffoldBroadcastE2ETest = `package bootstrap

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	forge "github.com/rocketinsidecorp/forge-go"
	authpostgres "github.com/rocketinsidecorp/forge-go/auth/postgres"
	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/csrf"
	"github.com/rocketinsidecorp/forge-go/migrate"
	"github.com/rocketinsidecorp/forge-go/postgres"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"

	appdb "example.com/mx/db"
)

func TestOwnerReceivesLiveUpdatesOverTheEventStream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	database, databaseConfig := postgrestest.NewWithConfig(t)
	loaded, err := migrate.LoadMigrationSets(authpostgres.Migrations(), appdb.Migrations())
	if err != nil {
		t.Fatal(err)
	}
	migrator, err := postgres.NewMigrator(databaseConfig, loaded, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrator.Up(ctx); err != nil {
		t.Fatal(err)
	}
	_ = migrator.Close()
	repository, err := authpostgres.New(database)
	if err != nil {
		t.Fatal(err)
	}
	const password = "correct horse battery staple"
	if _, err := repository.ProvisionOwner(ctx, authpostgres.OwnerAccount{Organization: "Acme", TenantSlug: "acme", Email: "owner@example.com", Password: password}); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.Database = databaseConfig
	cfg.Auth.MFAKey = config.NewSecret("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	app, err := forge.New(forge.WithConfig(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if err := Configure(ctx, app); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close(context.Background()) })
	server := httptest.NewServer(app.Handler())
	// Cleanups run last to first: the open event stream must end (cancel)
	// before the server can close, which waits for its connections.
	t.Cleanup(server.Close)
	t.Cleanup(cancel)

	// The browser: cookies are replayed by hand because the session and CSRF
	// cookies are Secure (browsers treat localhost as secure; Go's jar does not).
	cookies := map[string]*http.Cookie{}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	do := func(method, target string, form url.Values, withCookies bool) *http.Response {
		t.Helper()
		var body io.Reader
		if form != nil {
			form.Set(csrf.FormField, cookies[csrf.CookieName].Value)
			body = strings.NewReader(form.Encode())
		}
		request, err := http.NewRequestWithContext(ctx, method, server.URL+target, body)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Accept", "text/html")
		if form != nil {
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		if withCookies {
			for _, cookie := range cookies {
				request.AddCookie(cookie)
			}
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		for _, cookie := range response.Cookies() {
			if cookie.MaxAge < 0 {
				delete(cookies, cookie.Name)
			} else {
				cookies[cookie.Name] = cookie
			}
		}
		return response
	}
	read := func(response *http.Response) string {
		t.Helper()
		defer func() { _ = response.Body.Close() }()
		data, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}

	_ = read(do(http.MethodGet, "/session/new", nil, true))
	signedIn := do(http.MethodPost, "/session", url.Values{"tenant": {"acme"}, "email": {"owner@example.com"}, "password": {password}}, true)
	if signedIn.StatusCode != http.StatusSeeOther {
		t.Fatalf("sign in status=%d: %s", signedIn.StatusCode, read(signedIn))
	}
	_ = read(signedIn)

	index := do(http.MethodGet, "/products", nil, true)
	body := read(index)
	if index.StatusCode != http.StatusOK || !strings.Contains(body, ` + "`" + `<tbody id="products">` + "`" + `) {
		t.Fatalf("index status=%d: %s", index.StatusCode, body)
	}
	match := regexp.MustCompile(` + "`" + `<turbo-stream-source src="(/streams\?stream=[^"]+)">` + "`" + `).FindStringSubmatch(body)
	if match == nil {
		t.Fatalf("the index page does not subscribe to a stream:\n%s", body)
	}
	source := match[1]

	// Signed out, or with a name the server did not sign, the stream is closed.
	if response := do(http.MethodGet, source, nil, false); response.StatusCode != http.StatusUnauthorized {
		t.Errorf("event stream without a session = %d", response.StatusCode)
	} else {
		_ = read(response)
	}
	if response := do(http.MethodGet, "/streams?stream=forged", nil, true); response.StatusCode != http.StatusForbidden {
		t.Errorf("event stream with a forged name = %d", response.StatusCode)
	} else {
		_ = read(response)
	}

	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+source, nil)
	request.Header.Set("Accept", "text/event-stream")
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	stream, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Body.Close() }()
	if stream.StatusCode != http.StatusOK || !strings.HasPrefix(stream.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("event stream status=%d type=%q", stream.StatusCode, stream.Header.Get("Content-Type"))
	}
	lines := make(chan string, 64)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(stream.Body)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()
	// event returns the lines of the next event that contains want, joined.
	event := func(want string) string {
		t.Helper()
		deadline := time.After(10 * time.Second)
		var current []string
		for {
			select {
			case line, ok := <-lines:
				if !ok {
					t.Fatalf("the stream ended before %q", want)
				}
				if line != "" {
					current = append(current, line)
					continue
				}
				if joined := strings.Join(current, "\n"); strings.Contains(joined, want) {
					return joined
				}
				current = nil
			case <-deadline:
				t.Fatalf("no event containing %q", want)
			}
		}
	}
	event(": connected")

	created := do(http.MethodPost, "/products", url.Values{"name": {"Live <widget>"}}, true)
	if created.StatusCode != http.StatusSeeOther {
		t.Fatalf("create status=%d: %s", created.StatusCode, read(created))
	}
	_ = read(created)
	update := event("target=\"products\"")
	if !strings.Contains(update, "action=\"prepend\"") || !strings.Contains(update, "Live &lt;widget&gt;") || strings.Contains(update, "<widget>") {
		t.Errorf("the update is not an escaped prepend of the new row: %s", update)
	}
}
`

// TestGeneratedBroadcastScaffoldBuildsWiresAndPasses generates scaffolds
// with --broadcast (tenant-scoped and global), pastes the printed wiring into
// bootstrap.go, and requires the application to vet, build, lint and pass the
// generated tests; on PostgreSQL also the end-to-end test over a real HTTP
// server.
func TestGeneratedBroadcastScaffoldBuildsWiresAndPasses(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the go toolchain on generated projects")
	}
	t.Parallel()
	requests := [][]string{
		{"product", "name:string", "summary:string?", "--tenant-scoped", "--broadcast"},
		{"category", "name:string", "--global", "--broadcast"},
	}
	for _, database := range []struct{ name, adapter string }{{"postgresql", "postgres"}, {"sqlite", "sqlite"}} {
		t.Run(database.name, func(t *testing.T) {
			t.Parallel()
			dir := matrixApplicationFor(t, database.name, "scaffold", requests)
			var specs []resourceSpec
			for _, args := range requests {
				request, err := parseGeneratorArgs(args, scaffoldUsage)
				if err != nil {
					t.Fatal(err)
				}
				request.Scaffold = true
				specs = append(specs, resourceSpec{
					resourceRequest: request, Module: "example.com/mx", Database: database.adapter,
					Identity: database.adapter == "postgres", BrowserPipeline: true, ViewRenderer: database.adapter == "postgres",
				})
			}
			for _, path := range []string{"app/adapters/htmlstreams/streams.go", "app/adapters/htmlstreams/streams_test.go", "app/views/products/_item.gohtml", "app/views/categories/_item.gohtml"} {
				if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(path))); err != nil {
					t.Fatal(err)
				}
			}
			wireResources(t, dir, specs)
			if database.adapter == "postgres" {
				writeFile(t, filepath.Join(dir, "app", "bootstrap", "scaffold_broadcast_test.go"), scaffoldBroadcastE2ETest)
			}
			runGo(t, dir, "go", "vet", "./...")
			runGo(t, dir, "go", "build", "-o", os.DevNull, "./cmd/mx")
			packages := []string{"./app/adapters/htmlstreams/...", "./app/adapters/htmlui/...", "./app/views/...", "./app/assets/..."}
			if databaseAvailable(database.name) {
				packages = []string{"./app/..."}
			}
			command := exec.Command("go", append([]string{"test", "-count=1", "-v"}, packages...)...)
			command.Dir = dir
			command.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOPROXY=off")
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("go test: %v\n%s", err, output)
			}
			wants := []string{
				"--- PASS: TestProductPagesBroadcastChanges", "--- PASS: TestProductBroadcastsStayInTheirTenant",
				"--- PASS: TestCategoryPagesBroadcastChanges", "--- PASS: TestProductIndexSubscribesToLiveUpdates",
				"--- PASS: TestProductPagesSucceedWhenPublishingFails", "--- PASS: TestStreamsDeliverOnlyToTheirTenant",
				"--- PASS: TestBundleServesDigestedAssets",
			}
			if database.adapter == "postgres" && databaseAvailable(database.name) {
				wants = append(wants, "--- PASS: TestOwnerReceivesLiveUpdatesOverTheEventStream")
			}
			for _, want := range wants {
				if !strings.Contains(string(output), want) {
					t.Fatalf("generated tests did not run: missing %q\n%s", want, output)
				}
			}
			lint, err := exec.LookPath("golangci-lint")
			if err != nil {
				t.Log("golangci-lint is not on PATH; the lint check was skipped")
				return
			}
			golangciLint.Lock()
			defer golangciLint.Unlock()
			runGo(t, dir, lint, "run", "./app/...")
		})
	}
}

func TestBroadcastFlagIsOnlyForScaffolds(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	err := runGenerateEnv([]string{"resource", "post", "title:string", "--global", "--broadcast"}, &out, t.TempDir(), nil, time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC))
	if err == nil || !strings.Contains(err.Error(), "only for `forge generate scaffold`") {
		t.Fatalf("err = %v", err)
	}
	if _, err := parseGeneratorArgs([]string{"post", "title:string", "--global", "--broadcast", "--broadcast"}, scaffoldUsage); err == nil {
		t.Fatal("a repeated --broadcast was accepted")
	}
	request, err := parseGeneratorArgs([]string{"post", "title:string", "--global", "--broadcast"}, scaffoldUsage)
	if err != nil || !request.Broadcast {
		t.Fatalf("request=%+v err=%v", request, err)
	}
}
