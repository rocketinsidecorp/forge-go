package deploy

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

type recordingRunner struct {
	ran   []string
	close int
}

func (r *recordingRunner) Run(_ context.Context, host string, cmd Command) (Result, error) {
	r.ran = append(r.ran, host+": "+cmd.Script)
	return Result{Stdout: "from the inner runner"}, nil
}
func (r *recordingRunner) Stream(context.Context, string, Command, io.Writer) error { return nil }
func (r *recordingRunner) Interactive(context.Context, string, string) error        { return nil }
func (r *recordingRunner) Close() error                                             { r.close++; return nil }

func TestDryRunnerPrintsMutationsWithoutRunningThem(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	inner := &recordingRunner{}
	redactor := Secrets{values: map[string]string{"REGISTRY_PASSWORD": "hunter2-secret"}}.Redactor()
	runner := &dryRunner{inner: inner, out: &out, redactor: redactor}
	ctx := context.Background()

	// A query runs for real, so the plan names the containers that exist.
	result, err := runner.Run(ctx, "web1", Command{Script: "docker ps", ReadOnly: true})
	if err != nil || result.Stdout != "from the inner runner" || len(inner.ran) != 1 {
		t.Fatalf("read-only: %+v %v %v", result, err, inner.ran)
	}
	// A mutation is only printed, secrets redacted, and its stdin is described, not shown.
	if _, err := runner.Run(ctx, "web1", Command{Script: "docker login -p hunter2-secret", Stdin: []byte("hunter2-secret"), StdinNote: "registry password"}); err != nil {
		t.Fatal(err)
	}
	if err := runner.Stream(ctx, "web2", Command{Script: "docker logs -f app"}, &out); err != nil {
		t.Fatal(err)
	}
	if err := runner.Interactive(ctx, "web2", "docker exec -it app sh"); err != nil {
		t.Fatal(err)
	}
	if len(inner.ran) != 1 {
		t.Errorf("a dry run executed %v", inner.ran)
	}
	text := out.String()
	for _, want := range []string{"web1", "? docker ps", "$ docker login", "< stdin: registry password", "web2", "$ docker logs -f app", "$ docker exec -it app sh"} {
		if !strings.Contains(text, want) {
			t.Errorf("plan lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "hunter2-secret") {
		t.Errorf("the plan shows a secret:\n%s", text)
	}
	if err := runner.Close(); err != nil || inner.close != 1 {
		t.Errorf("Close = %v (inner closed %d times)", err, inner.close)
	}

	// Without an inner runner (a plan for hosts that are not reachable) a
	// query answers with nothing, and closing is a no-op.
	bare := &dryRunner{out: &out, redactor: redactor}
	if result, err := bare.Run(ctx, "web1", Command{Script: "docker ps", ReadOnly: true}); err != nil || result != (Result{}) {
		t.Errorf("bare read-only: %+v %v", result, err)
	}
	if err := bare.Close(); err != nil {
		t.Errorf("bare Close: %v", err)
	}
}

type fixedLocal struct{ output string }

func (l fixedLocal) Output(_ context.Context, dir string, argv []string) (string, error) {
	return l.output + ":" + dir + ":" + Join(argv...), nil
}
func (fixedLocal) Run(context.Context, string, []string, []byte, []string) error { return nil }

func TestDryLocalPrintsCommandsAndStillAnswersQueries(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	local := &dryLocal{inner: fixedLocal{output: "abc123"}, out: &out, redactor: Secrets{values: map[string]string{"REGISTRY_PASSWORD": "hunter2-secret"}}.Redactor()}
	got, err := local.Output(context.Background(), ".", []string{"git", "rev-parse", "HEAD"})
	if err != nil || got != "abc123:.:git rev-parse HEAD" {
		t.Fatalf("Output = %q, %v", got, err)
	}
	if err := local.Run(context.Background(), ".", []string{"docker", "login", "-p", "hunter2-secret"}, []byte("hunter2-secret"), []string{"FORGE_A=1", "FORGE_B=2"}); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{"local", "$ docker login", "< stdin: value not shown", "with 2 FORGE_* variables"} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "hunter2-secret") {
		t.Errorf("the output shows a secret:\n%s", text)
	}
}
