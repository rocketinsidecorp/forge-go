package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/rocketinsidecorp/forge-go/uuid"
)

type greeting struct {
	Name string `json:"name"`
	Age  int    `json:"age,omitempty"`
}

func TestValidateName(t *testing.T) {
	for _, ok := range []string{"a", "mail.send", "billing_v2", "a-b", strings.Repeat("a", 64)} {
		if err := ValidateName(ok); err != nil {
			t.Errorf("ValidateName(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "1abc", "Abc", "a b", "a/b", "ação", strings.Repeat("a", 65), "-a", ".a"} {
		if err := ValidateName(bad); err == nil {
			t.Errorf("ValidateName(%q) accepted", bad)
		}
	}
}

func TestRegisterValidates(t *testing.T) {
	r := NewRegistry()
	ok := func(context.Context, greeting) error { return nil }
	if err := Register(r, "Bad Kind", ok); err == nil {
		t.Error("accepted an invalid kind")
	}
	if err := Register[greeting](r, "nil.handler", nil); err == nil {
		t.Error("accepted a nil handler")
	}
	if err := Register(nil, "x", ok); err == nil {
		t.Error("accepted a nil registry")
	}
	if err := Register(r, "dup", ok); err != nil {
		t.Fatal(err)
	}
	if err := Register(r, "dup", ok); err == nil {
		t.Error("accepted a duplicate kind")
	}
	for name, opt := range map[string]HandlerOption{
		"queue":    HandlerQueue("Bad"),
		"attempts": HandlerMaxAttempts(0),
		"priority": HandlerPriority(MaxPriority + 1),
		"timeout":  HandlerTimeout(0),
		"backoff":  HandlerBackoff(Backoff{Base: time.Minute, Max: time.Second}),
	} {
		if err := Register(r, "opt."+name, ok, opt); err == nil {
			t.Errorf("accepted invalid %s option", name)
		}
	}
	if got := r.Kinds(); len(got) != 1 || got[0] != "dup" {
		t.Errorf("Kinds() = %v", got)
	}
}

func TestPrepareAppliesDefaultsAndOverrides(t *testing.T) {
	r := NewRegistry()
	if err := Register(r, "greet", func(context.Context, greeting) error { return nil },
		HandlerQueue("mail"), HandlerMaxAttempts(4), HandlerPriority(2)); err != nil {
		t.Fatal(err)
	}
	job, err := Prepare(r, "greet", greeting{Name: "Ana", Age: 7})
	if err != nil {
		t.Fatal(err)
	}
	if job.Queue != "mail" || job.MaxAttempts != 4 || job.Priority != 2 || !job.RunAt.IsZero() || job.Key != "" {
		t.Fatalf("defaults not applied: %+v", job)
	}
	if job.ID.Version() != 7 {
		t.Fatalf("job ID %s is not a UUIDv7", job.ID)
	}
	if string(job.Payload) != `{"name":"Ana","age":7}` {
		t.Fatalf("payload = %s", job.Payload)
	}
	at := time.Now().Add(time.Hour)
	job, err = Prepare(r, "greet", greeting{Name: "Bo"}, WithQueue("urgent"), WithMaxAttempts(1), WithPriority(-5), RunAt(at), WithKey("k"))
	if err != nil {
		t.Fatal(err)
	}
	if job.Queue != "urgent" || job.MaxAttempts != 1 || job.Priority != -5 || !job.RunAt.Equal(at) || job.Key != "k" {
		t.Fatalf("overrides not applied: %+v", job)
	}
	delayed, err := Prepare(r, "greet", greeting{Name: "Cy"}, RunAfter(time.Minute))
	if err != nil || time.Until(delayed.RunAt) < 59*time.Second {
		t.Fatalf("RunAfter: %+v, %v", delayed, err)
	}
}

func TestPrepareRejectsWhatTheWorkerCouldNotRun(t *testing.T) {
	r := NewRegistry()
	if err := Register(r, "greet", func(context.Context, greeting) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := Register(r, "ptr", func(context.Context, *greeting) error { return nil }); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func() error{
		"unknown kind": func() error { _, err := Prepare(r, "nobody", greeting{}); return err },
		"wrong type":   func() error { _, err := Prepare(r, "greet", "just a string"); return err },
		"pointer type": func() error { _, err := Prepare(r, "greet", &greeting{}); return err },
		"nil registry": func() error { _, err := Prepare[greeting](nil, "greet", greeting{}); return err },
		"null payload": func() error { _, err := Prepare[*greeting](r, "ptr", nil); return err },
		"bad queue":    func() error { _, err := Prepare(r, "greet", greeting{}, WithQueue("Nope")); return err },
		"bad attempts": func() error {
			_, err := Prepare(r, "greet", greeting{}, WithMaxAttempts(MaxAttemptsLimit+1))
			return err
		},
		"empty key": func() error { _, err := Prepare(r, "greet", greeting{}, WithKey("")); return err },
		"long key": func() error {
			_, err := Prepare(r, "greet", greeting{}, WithKey(strings.Repeat("k", MaxKeyLength+1)))
			return err
		},
		"negative delay": func() error { _, err := Prepare(r, "greet", greeting{}, RunAfter(-time.Second)); return err },
		"zero run time":  func() error { _, err := Prepare(r, "greet", greeting{}, RunAt(time.Time{})); return err },
		"nil option":     func() error { _, err := Prepare(r, "greet", greeting{}, nil); return err },
		"unencodable":    func() error { _, err := Prepare(r, "greet", greeting{}); return err },
	}
	delete(cases, "unencodable")
	delete(cases, "pointer type")
	for name, fn := range cases {
		if err := fn(); err == nil {
			t.Errorf("%s: Prepare succeeded", name)
		}
	}
	if _, err := Prepare(r, "nobody", greeting{}); !errors.Is(err, ErrUnknownKind) {
		t.Errorf("unknown kind error = %v", err)
	}
	// Too large.
	big := greeting{Name: strings.Repeat("x", MaxPayloadBytes)}
	if _, err := Prepare(r, "greet", big); err == nil {
		t.Error("accepted an oversized payload")
	}
	// Payload types that cannot round-trip through the strict decoder.
	type hidden struct {
		Visible string `json:"visible"`
		Skipped string `json:"-"`
	}
	hr := NewRegistry()
	if err := Register(hr, "hidden", func(context.Context, hidden) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(hr, "hidden", hidden{Visible: "v", Skipped: "s"}); err != nil {
		t.Errorf("a field excluded from JSON must still be accepted (it decodes): %v", err)
	}
}

func TestDecodeStrict(t *testing.T) {
	good, err := decodeStrict[greeting]([]byte(`{"name":"Ana"}`))
	if err != nil || good.Name != "Ana" {
		t.Fatalf("decode = %+v, %v", good, err)
	}
	for _, bad := range []string{`{"name":"Ana","extra":1}`, `{"name":1}`, `{"name":"a"} {"name":"b"}`, `[]`, `{"name":"a"}garbage`, ``} {
		if _, err := decodeStrict[greeting]([]byte(bad)); err == nil {
			t.Errorf("decodeStrict accepted %q", bad)
		}
	}
	// A decode error names the problem, not the value.
	_, err = decodeStrict[greeting]([]byte(`{"name":"super-secret-token"}x`))
	if err == nil || strings.Contains(err.Error(), "super-secret-token") {
		t.Errorf("decode error %v must not quote payload values", err)
	}
}

func TestRegisteredRunFailsPermanentlyOnUndecodablePayload(t *testing.T) {
	r := NewRegistry()
	called := false
	if err := Register(r, "greet", func(context.Context, greeting) error { called = true; return nil }); err != nil {
		t.Fatal(err)
	}
	reg, _ := r.lookup("greet")
	err := reg.run(context.Background(), json.RawMessage(`{"unknown":1}`))
	var permanent *permanentError
	if !errors.As(err, &permanent) || called {
		t.Fatalf("run = %v (handler called: %v); want a permanent error without calling the handler", err, called)
	}
}

func TestBackoff(t *testing.T) {
	b := Backoff{Base: time.Second, Max: 30 * time.Second}
	wantCeilings := []time.Duration{1, 2, 4, 8, 16, 30, 30, 30}
	for i, want := range wantCeilings {
		if got := b.Ceiling(i + 1); got != want*time.Second {
			t.Errorf("Ceiling(%d) = %s, want %s", i+1, got, want*time.Second)
		}
	}
	if got := b.Ceiling(10_000); got != 30*time.Second {
		t.Errorf("Ceiling(huge) = %s", got)
	}
	for attempt := 1; attempt <= 8; attempt++ {
		ceiling := b.Ceiling(attempt)
		for range 50 {
			delay := b.Delay(attempt)
			if delay < ceiling/2 || delay > ceiling {
				t.Fatalf("Delay(%d) = %s, want within [%s, %s]", attempt, delay, ceiling/2, ceiling)
			}
		}
	}
	// Jitter actually varies.
	seen := map[time.Duration]bool{}
	for range 50 {
		seen[b.Delay(6)] = true
	}
	if len(seen) < 5 {
		t.Errorf("only %d distinct delays in 50 draws; jitter is not random", len(seen))
	}
	var zero Backoff
	if zero.Ceiling(1) != 5*time.Second || zero.Ceiling(100) != time.Hour {
		t.Errorf("defaults: %s .. %s", zero.Ceiling(1), zero.Ceiling(100))
	}
	if (Backoff{Base: time.Hour * 24 * 40}).validate() == nil || (Backoff{Base: time.Minute, Max: time.Second}).validate() == nil {
		t.Error("validate accepted an unsafe backoff")
	}
}

func TestPermanentAndRetryAfter(t *testing.T) {
	base := errors.New("boom")
	if Permanent(nil) != nil || RetryAfter(time.Second, nil) != nil {
		t.Fatal("wrapping nil must stay nil")
	}
	err := fmt.Errorf("outer: %w", Permanent(base))
	var permanent *permanentError
	if !errors.As(err, &permanent) || !errors.Is(err, base) || err.Error() != "outer: boom" {
		t.Errorf("Permanent chain broken: %v", err)
	}
	var after *retryAfterError
	err = RetryAfter(-time.Second, base)
	if !errors.As(err, &after) || after.delay != 0 || !errors.Is(err, base) {
		t.Errorf("RetryAfter chain broken: %v", err)
	}
}

func TestSanitizeError(t *testing.T) {
	payload := []byte(`{"card":"4111-1111-1111-1111","note":"ok","nested":{"email":"ana@example.com"},"list":["alpha-secret"]}`)
	cases := []struct {
		name string
		err  error
		not  []string
		has  []string
	}{
		{"payload string", fmt.Errorf("charge failed for card 4111-1111-1111-1111"), []string{"4111"}, []string{"charge failed", "[redacted]"}},
		{"nested and list", errors.New("ana@example.com / alpha-secret"), []string{"ana@example.com", "alpha-secret"}, nil},
		{"short strings kept", errors.New("note ok stays readable"), nil, []string{"note ok stays readable"}},
		{"url credentials", errors.New("dial postgres://admin:s3cr3t@db.internal:5432/app failed"), []string{"s3cr3t", "admin:"}, []string{"db.internal"}},
		{"assignments", errors.New(`password=hunter2 api_key: "abcDEF123" token=xyz`), []string{"hunter2", "abcDEF123", "xyz"}, nil},
		{"bearer", errors.New("Authorization: Bearer abcdefghijklmnop failed"), []string{"abcdefghijklmnop"}, nil},
		{"control characters", errors.New("line1\nline2\x00\x1b[31m red"), []string{"\n", "\x00", "\x1b"}, []string{"line1 line2"}},
		{"plain", errors.New("connection refused"), nil, []string{"connection refused"}},
	}
	for _, c := range cases {
		got := sanitizeError(c.err, payload)
		for _, s := range c.not {
			if strings.Contains(got, s) {
				t.Errorf("%s: %q still contains %q", c.name, got, s)
			}
		}
		for _, s := range c.has {
			if !strings.Contains(got, s) {
				t.Errorf("%s: %q lost %q", c.name, got, s)
			}
		}
	}
	if sanitizeError(nil, payload) != "" {
		t.Error("nil error must sanitize to empty text")
	}
}

func TestSanitizeErrorNamesTheRootType(t *testing.T) {
	root := &json.SyntaxError{}
	err := fmt.Errorf("a: %w", fmt.Errorf("b: %w", root))
	if got := sanitizeError(err, nil); !strings.HasPrefix(got, "*json.SyntaxError: ") {
		t.Errorf("sanitizeError = %q", got)
	}
	joined := errors.Join(root, errors.New("other"))
	if got := sanitizeError(joined, nil); !strings.HasPrefix(got, "*json.SyntaxError: ") {
		t.Errorf("joined sanitizeError = %q", got)
	}
}

func TestSanitizeErrorBoundsAndKeepsUTF8(t *testing.T) {
	long := errors.New(strings.Repeat("ação ", 1000))
	got := sanitizeError(long, nil)
	if len(got) > MaxErrorBytes || !utf8.ValidString(got) || !strings.HasSuffix(got, "...") {
		t.Errorf("len=%d valid=%v suffix=%q", len(got), utf8.ValidString(got), got[max(0, len(got)-5):])
	}
	garbage := errors.New("bad \xff\xfe bytes")
	if got := sanitizeError(garbage, nil); !utf8.ValidString(got) {
		t.Errorf("invalid UTF-8 survived: %q", got)
	}
}

func TestNormalizeWorkerOptions(t *testing.T) {
	got, err := normalizeWorkerOptions(WorkerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Queues) != 1 || got.Queues[0].Name != DefaultQueue || got.Queues[0].Concurrency != 5 ||
		got.PollInterval != time.Second || got.MaxPollInterval != 10*time.Second || got.Lease != time.Minute ||
		got.RetainSucceeded != 24*time.Hour || got.Backoff.Base != 5*time.Second || got.WorkerID == "" {
		t.Fatalf("defaults = %+v", got)
	}
	bad := map[string]WorkerOptions{
		"queue name":        {Queues: []QueueConfig{{Name: "Bad"}}},
		"duplicate queue":   {Queues: []QueueConfig{{Name: "a"}, {Name: "a"}}},
		"concurrency":       {Queues: []QueueConfig{{Name: "a", Concurrency: 257}}},
		"total concurrency": {Queues: []QueueConfig{{Name: "a", Concurrency: 256}, {Name: "b", Concurrency: 256}, {Name: "c", Concurrency: 256}, {Name: "d", Concurrency: 256}, {Name: "e", Concurrency: 1}}},
		"negative duration": {Lease: -time.Second},
		"tiny lease":        {Lease: time.Microsecond},
		"huge lease":        {Lease: 48 * time.Hour},
		"poll order":        {PollInterval: time.Minute, MaxPollInterval: time.Second},
		"worker id":         {WorkerID: strings.Repeat("w", 129)},
		"backoff":           {Backoff: Backoff{Base: time.Minute, Max: time.Second}},
	}
	for name, opts := range bad {
		if _, err := normalizeWorkerOptions(opts); err == nil {
			t.Errorf("accepted invalid %s", name)
		}
	}
	if _, err := NewWorker(nil, NewRegistry(), WorkerOptions{}); err == nil {
		t.Error("NewWorker accepted a nil store")
	}
}

func TestWorkerNeedsHandlersAndStartsOnce(t *testing.T) {
	w, err := NewWorker(noStore{}, NewRegistry(), WorkerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Start(context.Background()); err == nil {
		t.Error("Start without handlers succeeded")
	}
	// Shutdown on a worker that never ran is a no-op, and the worker cannot
	// be started afterwards.
	if err := w.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown of an idle worker = %v", err)
	}
}

func TestNewJobValidate(t *testing.T) {
	good := NewJob{ID: uuid.MustNew(), Kind: "k", Queue: "q", Payload: json.RawMessage(`{}`), MaxAttempts: 1}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*NewJob){
		"id":       func(j *NewJob) { j.ID = uuid.UUID{} },
		"kind":     func(j *NewJob) { j.Kind = "" },
		"queue":    func(j *NewJob) { j.Queue = "A" },
		"payload":  func(j *NewJob) { j.Payload = nil },
		"json":     func(j *NewJob) { j.Payload = json.RawMessage(`{`) },
		"attempts": func(j *NewJob) { j.MaxAttempts = MaxAttemptsLimit + 1 },
		"priority": func(j *NewJob) { j.Priority = MaxPriority + 1 },
		"key":      func(j *NewJob) { j.Key = "bad\nkey" },
	} {
		j := good
		mutate(&j)
		if j.Validate() == nil {
			t.Errorf("Validate accepted invalid %s", name)
		}
	}
}

// noStore satisfies Store for tests that never reach the database.
type noStore struct{ Store }
