package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDestroyRejectsMalformedCommandLines(t *testing.T) {
	t.Parallel()
	root := resourceProject(t, postgresURL)
	for name, args := range map[string][]string{
		"nothing":                   nil,
		"model without a name":      {"model"},
		"resource without a name":   {"resource"},
		"scaffold without a name":   {"scaffold"},
		"controller without a name": {"controller"},
		"mailer without a name":     {"mailer"},
		"queue job without a name":  {"job", "--queue"},
		"queue job bad flag":        {"job", "x", "--queue", "--wat"},
		"job without a name":        {"job"},
		"job with a bad flag":       {"job", "nightly", "--wat"},
		"job with a bad name":       {"job", "Not-Valid"},
		"view without a name":       {"view"},
		"view with a bad flag":      {"view", "welcome", "--wat"},
		"view with a bad name":      {"view", "layouts/x"},
		"migration":                 {"migration", "add_prices"},
		"unknown generator":         {"widget", "x"},
	} {
		var out bytes.Buffer
		if err := runDestroyEnv(args, &out, root, nil, generateNow); err == nil {
			t.Errorf("%s: forge destroy %v succeeded", name, args)
		}
		if out.Len() != 0 {
			t.Errorf("%s: output %q", name, out.String())
		}
	}
	var out bytes.Buffer
	if err := runDestroyEnv([]string{"migration", "x"}, &out, root, nil, generateNow); err == nil || !strings.Contains(err.Error(), "forge rollback") {
		t.Errorf("destroying a migration must explain how to revert it: %v", err)
	}
}

func TestDestroyNeedsAnApplication(t *testing.T) {
	t.Parallel()
	gone := filepath.Join(t.TempDir(), "gone")
	for name, args := range map[string][]string{
		"model":      {"model", "post", "title:string", "--global"},
		"resource":   {"resource", "post", "title:string", "--global"},
		"scaffold":   {"scaffold", "post", "title:string", "--global"},
		"view":       {"view", "welcome"},
		"job":        {"job", "nightly"},
		"controller": {"controller", "pages", "index"},
		"mailer":     {"mailer", "welcome", "greet"},
	} {
		var out bytes.Buffer
		if err := runDestroyEnv(args, &out, gone, nil, generateNow); err == nil {
			t.Errorf("forge destroy %s in a missing directory succeeded", name)
		}
	}
	var out bytes.Buffer
	plan := destroyPlan{files: []generatedFile{{path: "x", content: "y"}}}
	if err := applyDestroy(&out, gone, plan, false, false); err == nil || !strings.Contains(err.Error(), "go.mod not found") {
		t.Errorf("applyDestroy in a missing directory: %v", err)
	}
}

func TestDestroyReportsWhatItCannotInspect(t *testing.T) {
	t.Parallel()
	t.Run("app is a file", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		writeFile(t, filepath.Join(root, "go.mod"), "module example.com/shop\n")
		writeFile(t, filepath.Join(root, "app"), "a file")
		var out bytes.Buffer
		for _, args := range [][]string{{"model", "post", "title:string", "--global"}, {"resource", "post", "title:string", "--global"}} {
			if err := runDestroyEnv(args, &out, root, nil, generateNow); err == nil {
				t.Errorf("forge destroy %v with app as a file succeeded", args)
			}
		}
	})
	t.Run("the controller file cannot be read", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		writeFile(t, filepath.Join(root, "go.mod"), "module example.com/shop\n")
		writeFile(t, filepath.Join(root, "app", "adapters"), "a file")
		var out bytes.Buffer
		if err := runDestroyEnv([]string{"view", "pages/welcome"}, &out, root, nil, generateNow); err == nil {
			t.Error("forge destroy view with app/adapters as a file succeeded")
		}
	})
}

func TestDestroyRefusesAFileThatIsNotRegular(t *testing.T) {
	t.Parallel()
	root := resourceProject(t, postgresURL)
	var out bytes.Buffer
	if err := runGenerateEnv([]string{"job", "nightly"}, &out, root, nil, generateNow); err != nil {
		t.Fatal(err)
	}
	job := filepath.Join(root, "app", "jobs", "nightly.go")
	content, err := os.ReadFile(job)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(job); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "app", "jobs", "target.txt"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target.txt", job); err != nil {
		t.Skipf("symlinks are not available: %v", err)
	}
	err = runDestroyEnv([]string{"job", "nightly", "--force"}, &out, root, nil, generateNow)
	if err == nil || !strings.Contains(err.Error(), "it is not a regular file") {
		t.Errorf("err = %v", err)
	}
	if _, err := os.Lstat(job); err != nil {
		t.Errorf("the symlink was removed: %v", err)
	}
}

func TestDestroyReportsAnOutputThatCannotBeWritten(t *testing.T) {
	t.Parallel()
	for name, writes := range map[string]int{"the file list": 0, "the wiring note": 2} {
		root := resourceProject(t, postgresURL)
		var out bytes.Buffer
		if err := runGenerateEnv([]string{"job", "nightly"}, &out, root, nil, generateNow); err != nil {
			t.Fatal(err)
		}
		before := snapshotTree(t, root)
		writer := &limitedWriter{okWrites: writes}
		err := runDestroyEnv([]string{"job", "nightly", "--dry-run"}, writer, root, nil, generateNow)
		if err != errBrokenPipe {
			t.Errorf("%s: err = %v", name, err)
		}
		if len(snapshotTree(t, root)) != len(before) {
			t.Errorf("%s: a dry run changed the tree", name)
		}
	}
}

func TestDestroyReportsModifiedAndMissingFilesAndKeepsWhatTheGeneratorDidNotWrite(t *testing.T) {
	t.Parallel()
	root := resourceProject(t, postgresURL)
	var out bytes.Buffer
	if err := runGenerateEnv([]string{"job", "nightly"}, &out, root, nil, generateNow); err != nil {
		t.Fatal(err)
	}
	test := filepath.Join(root, "app", "jobs", "nightly_test.go")
	if err := os.Remove(test); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "app", "jobs", "nightly.go"), "package jobs\n// edited\n")
	out.Reset()
	err := runDestroyEnv([]string{"job", "nightly"}, &out, root, nil, generateNow)
	if err == nil || !strings.Contains(err.Error(), "differ from what this generator would write") {
		t.Fatalf("an edited file: %v", err)
	}
	out.Reset()
	if err := runDestroyEnv([]string{"job", "nightly", "--force"}, &out, root, nil, generateNow); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "skip") || !strings.Contains(out.String(), "not found") || !strings.Contains(out.String(), "removed because of --force") {
		t.Errorf("destroy output:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(root, "app", "jobs")); !os.IsNotExist(err) {
		t.Errorf("the emptied directory was kept: %v", err)
	}
	out.Reset()
	if err := runDestroyEnv([]string{"job", "nightly"}, &out, root, nil, generateNow); err == nil || !strings.Contains(err.Error(), "nothing to destroy") {
		t.Errorf("destroying twice: %v", err)
	}
}
