// Command loadgen is the HTTP load generator of scripts/bench.sh: a closed
// loop of a fixed number of workers that send the requests of the benchmark
// scenarios (docs/BENCHMARKS.md) to a running generated application for a
// fixed time, and report requests per second and latency percentiles.
//
// It uses only the standard library. It signs in once (the password comes
// from the BENCH_PASSWORD environment variable, never an argument), creates
// the rows the scenarios read, and measures nothing before the warm-up has
// passed. Scenarios are measured in rounds, interleaved, so a slow drift of
// the host affects every scenario alike.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// Scenario names are shared with the Forge-Rust suite (docs/BENCHMARKS.md).
const (
	scenarioHealth     = "health"
	scenarioWelcome    = "welcome"
	scenarioIndexHTML  = "index_html"
	scenarioShowJSON   = "show_json"
	scenarioCreateForm = "create_form"
)

var allScenarios = []string{scenarioHealth, scenarioWelcome, scenarioIndexHTML, scenarioShowJSON, scenarioCreateForm}

const (
	sessionCookie = "__Host-forge_session"
	csrfCookie    = "__Host-forge_csrf"
)

type config struct {
	base        string
	collection  string
	tenant      string
	email       string
	password    string
	concurrency int
	duration    time.Duration
	warmup      time.Duration
	runs        int
	rows        int
	scenarios   []string
	format      string
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "loadgen:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("loadgen", flag.ContinueOnError)
	flags.SetOutput(stderr)
	cfg := config{}
	var scenarios string
	flags.StringVar(&cfg.base, "base", "http://127.0.0.1:8080", "base URL of the application")
	flags.StringVar(&cfg.collection, "collection", "posts", "plural name of the scaffolded resource (HTML /NAME, JSON /v1/NAME; fields title and body)")
	flags.StringVar(&cfg.tenant, "tenant", "bench", "tenant slug to sign in to")
	flags.StringVar(&cfg.email, "email", "bench@example.com", "email of the owner account")
	flags.IntVar(&cfg.concurrency, "concurrency", 16, "concurrent workers (connections)")
	flags.DurationVar(&cfg.duration, "duration", 10*time.Second, "measured time of each scenario in each run")
	flags.DurationVar(&cfg.warmup, "warmup", 2*time.Second, "unmeasured warm-up of each scenario")
	flags.IntVar(&cfg.runs, "runs", 3, "measured runs of each scenario")
	flags.IntVar(&cfg.rows, "rows", 50, "rows listed by index_html (the page size, at most 200)")
	flags.StringVar(&scenarios, "scenarios", strings.Join(allScenarios, ","), "comma-separated scenarios to run")
	flags.StringVar(&cfg.format, "format", "markdown", "output: markdown or json")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg.password = os.Getenv("BENCH_PASSWORD")
	cfg.scenarios = strings.Split(scenarios, ",")
	if err := cfg.validate(); err != nil {
		return err
	}

	ctx, stop := notifyContext()
	defer stop()
	target, err := newTarget(ctx, cfg)
	if err != nil {
		return err
	}
	results, err := measure(ctx, cfg, target, stderr)
	if err != nil {
		return err
	}
	return report(stdout, cfg, results)
}

func (c config) validate() error {
	switch {
	case c.concurrency < 1 || c.runs < 1 || c.duration <= 0 || c.warmup < 0:
		return errors.New("concurrency, runs and duration must be positive and warmup not negative")
	case c.rows < 1 || c.rows > 200:
		return errors.New("rows must be between 1 and 200")
	case c.password == "":
		return errors.New("BENCH_PASSWORD is required")
	case c.format != "markdown" && c.format != "json":
		return errors.New("format must be markdown or json")
	}
	for _, name := range c.scenarios {
		if !slices.Contains(allScenarios, name) {
			return fmt.Errorf("unknown scenario %q (have %s)", name, strings.Join(allScenarios, ", "))
		}
	}
	return nil
}

// request describes one request of a scenario; build runs once per request.
type request struct {
	method string
	path   string
	header http.Header
	body   string
	// want is the status that counts as success.
	want int
}

func (r request) build(ctx context.Context, base string) (*http.Request, error) {
	var body io.Reader
	if r.body != "" {
		body = strings.NewReader(r.body)
	}
	built, err := http.NewRequestWithContext(ctx, r.method, base+r.path, body)
	if err != nil {
		return nil, err
	}
	for name, values := range r.header {
		built.Header[name] = slices.Clone(values)
	}
	return built, nil
}

// target is the signed-in state the scenarios need.
type target struct {
	client    *http.Client
	base      string
	scenarios map[string]request
}

var tokenField = regexp.MustCompile(`name="authenticity_token" value="([^"]+)"`)

func newTarget(ctx context.Context, cfg config) (*target, error) {
	transport := &http.Transport{
		MaxIdleConns:        cfg.concurrency,
		MaxIdleConnsPerHost: cfg.concurrency,
		MaxConnsPerHost:     cfg.concurrency,
		IdleConnTimeout:     time.Minute,
		DisableCompression:  true,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
		// Redirects are answers to measure, not links to follow.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	t := &target{client: client, base: strings.TrimSuffix(cfg.base, "/")}
	if err := t.waitReady(ctx); err != nil {
		return nil, err
	}

	// Cookies are set by hand: the session and CSRF cookies are Secure, so a
	// cookie jar would not send them over plain HTTP.
	_, header, page, err := t.do(ctx, request{method: http.MethodGet, path: "/session/new", want: http.StatusOK})
	if err != nil {
		return nil, fmt.Errorf("open the sign-in page: %w", err)
	}
	csrf := cookieValue(header, csrfCookie)
	token := tokenField.FindStringSubmatch(page)
	if csrf == "" || token == nil {
		return nil, errors.New("the sign-in page carries no CSRF cookie and token")
	}
	form := url.Values{"authenticity_token": {token[1]}, "tenant": {cfg.tenant}, "email": {cfg.email}, "password": {cfg.password}}
	_, header, _, err = t.do(ctx, request{
		method: http.MethodPost, path: "/session", want: http.StatusSeeOther, body: form.Encode(),
		header: http.Header{"Content-Type": {"application/x-www-form-urlencoded"}, "Cookie": {csrfCookie + "=" + csrf}},
	})
	if err != nil {
		return nil, fmt.Errorf("sign in: %w", err)
	}
	session := cookieValue(header, sessionCookie)
	if session == "" {
		return nil, errors.New("sign-in set no session cookie")
	}
	cookies := http.Header{"Cookie": {sessionCookie + "=" + session + "; " + csrfCookie + "=" + csrf}}

	// The JSON API takes a bearer token, not the cookie.
	login, err := json.Marshal(map[string]string{"tenant": cfg.tenant, "email": cfg.email, "password": cfg.password})
	if err != nil {
		return nil, err
	}
	_, _, answer, err := t.do(ctx, request{method: http.MethodPost, path: "/auth/login", want: http.StatusOK, body: string(login), header: http.Header{"Content-Type": {"application/json"}}})
	if err != nil {
		return nil, fmt.Errorf("log in for a bearer token: %w", err)
	}
	var bearer struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal([]byte(answer), &bearer); err != nil || bearer.AccessToken == "" {
		return nil, errors.New("the login answer carries no access token")
	}

	create := request{
		method: http.MethodPost, path: "/" + cfg.collection, want: http.StatusSeeOther,
		header: http.Header{"Content-Type": {"application/x-www-form-urlencoded"}, "Cookie": cookies["Cookie"]},
		body:   url.Values{"authenticity_token": {csrf}, "title": {"Benchmark post"}, "body": {"A short body written by the load generator."}}.Encode(),
	}
	// Create the rows the index lists and one to show.
	var location string
	for range cfg.rows {
		_, header, _, err := t.do(ctx, create)
		if err != nil {
			return nil, fmt.Errorf("create a row: %w", err)
		}
		location = header.Get("Location")
	}
	id := location[strings.LastIndex(location, "/")+1:]
	if id == "" {
		return nil, fmt.Errorf("unexpected Location %q after create", location)
	}
	t.scenarios = map[string]request{
		scenarioHealth:     {method: http.MethodGet, path: "/health", want: http.StatusOK},
		scenarioWelcome:    {method: http.MethodGet, path: "/", want: http.StatusOK},
		scenarioIndexHTML:  {method: http.MethodGet, path: fmt.Sprintf("/%s?limit=%d", cfg.collection, cfg.rows), want: http.StatusOK, header: cookies},
		scenarioShowJSON:   {method: http.MethodGet, path: "/v1/" + cfg.collection + "/" + id, want: http.StatusOK, header: http.Header{"Authorization": {"Bearer " + bearer.AccessToken}}},
		scenarioCreateForm: create,
	}
	return t, nil
}

// waitReady polls readiness so a server that is still starting is not measured.
func (t *target) waitReady(ctx context.Context) error {
	deadline := time.Now().Add(30 * time.Second)
	for {
		_, _, _, err := t.do(ctx, request{method: http.MethodGet, path: "/health/ready", want: http.StatusOK})
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return fmt.Errorf("the application is not ready at %s: %w", t.base, err)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// do sends one request, drains the body and returns it (capped at 1 MiB).
func (t *target) do(ctx context.Context, r request) (status int, header http.Header, body string, err error) {
	built, err := r.build(ctx, t.base)
	if err != nil {
		return 0, nil, "", err
	}
	response, err := t.client.Do(built)
	if err != nil {
		return 0, nil, "", err
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return 0, nil, "", err
	}
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode != r.want {
		return response.StatusCode, response.Header, string(data), fmt.Errorf("%s %s answered %d, want %d", r.method, r.path, response.StatusCode, r.want)
	}
	return response.StatusCode, response.Header, string(data), nil
}

func cookieValue(header http.Header, name string) string {
	for _, cookie := range (&http.Response{Header: header}).Cookies() {
		if cookie.Name == name {
			return cookie.Value
		}
	}
	return ""
}

// sample is the outcome of one measured window of one scenario.
type sample struct {
	ok, errors int
	elapsed    time.Duration
	latencies  []time.Duration // of the successful requests, sorted
}

// drive runs the closed loop for d with the given number of workers.
func (t *target) drive(ctx context.Context, r request, workers int, d time.Duration) sample {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	type part struct {
		latencies []time.Duration
		errors    int
	}
	parts := make([]part, workers)
	var wg sync.WaitGroup
	start := time.Now()
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mine := &parts[w]
			mine.latencies = make([]time.Duration, 0, 1<<16)
			for ctx.Err() == nil {
				began := time.Now()
				_, _, _, err := t.do(ctx, r)
				took := time.Since(began)
				switch {
				case ctx.Err() != nil && err != nil:
					return // the window closed under this request: not an outcome
				case err != nil:
					mine.errors++
				default:
					mine.latencies = append(mine.latencies, took)
				}
			}
		}()
	}
	wg.Wait()
	out := sample{elapsed: time.Since(start)}
	for _, p := range parts {
		out.latencies = append(out.latencies, p.latencies...)
		out.errors += p.errors
	}
	out.ok = len(out.latencies)
	slices.Sort(out.latencies)
	return out
}

// percentile is the nearest-rank percentile of sorted.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(p / 100 * float64(len(sorted))))
	return sorted[min(max(rank, 1), len(sorted))-1]
}
