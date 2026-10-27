package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestEveryCommandPrintsItsHelpAndExitsZero(t *testing.T) {
	t.Parallel()
	for _, command := range currentCommandCatalog().Commands {
		for _, form := range [][]string{{command.Name, "--help"}, {command.Name, "-h"}, {"help", command.Name}} {
			var stdout, stderr bytes.Buffer
			if code := Run(form, &stdout, &stderr); code != 0 {
				t.Errorf("forge %s: exit %d, stderr %q", strings.Join(form, " "), code, stderr.String())
				continue
			}
			if !strings.HasPrefix(stdout.String(), "usage: "+command.Usage+"\n") || stderr.Len() != 0 {
				t.Errorf("forge %s: stdout %q, stderr %q", strings.Join(form, " "), stdout.String(), stderr.String())
			}
		}
	}
}

func TestSubcommandHelpListsFlagsAndExamples(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"new", "--help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	for _, want := range []string{"usage: forge new NAME", "--database DB", "(default: sqlite)", "examples:", "forge new inventory"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("forge new --help lacks %q:\n%s", want, stdout.String())
		}
	}
	if strings.Contains(stdout.String()+stderr.String(), "flag: help requested") {
		t.Errorf("flag package message leaked: %q", stderr.String())
	}
}

func TestHelpMisuseIsAUsageErrorWithExitStatus2(t *testing.T) {
	t.Parallel()
	for name, args := range map[string][]string{
		"unknown command": {"help", "nope"},
		"unknown format":  {"help", "--xml"},
		"too many":        {"help", "new", "extra"},
		"json plus name":  {"help", "--json", "new"},
	} {
		var stdout, stderr bytes.Buffer
		if code := Run(args, &stdout, &stderr); code != 2 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "forge: ") {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", name, code, stdout.String(), stderr.String())
		}
	}
}

func TestHelpJSONCatalogStillWorks(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"help", "--json"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), `"schema_version":1`) {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
}

func TestHelpFlagBelongsToForwardedProgramsAfterTheirFirstArgument(t *testing.T) {
	t.Parallel()
	for args, want := range map[string]bool{
		"test --help":              true,
		"test ./... -h":            false,
		"test ./... -run X --help": false,
		"app exec --help":          true,
		"app exec ls --help":       false,
		"app exec -- ls --help":    false,
		"new inventory --help":     true,
		"generate scaffold -h":     true,
		"new -- --help":            false,
		"nope --help":              false,
		"doctor":                   false,
	} {
		if got := commandHelpRequested(strings.Fields(args)); got != want {
			t.Errorf("commandHelpRequested(%q) = %t, want %t", args, got, want)
		}
	}
}

func TestFlagSetHelpAfterOwnFlagsExitsZeroToo(t *testing.T) {
	t.Parallel()
	// The pre-scan leaves `app exec -i --help` to the flag set, which reports
	// flag.ErrHelp; Run still treats it as a help request.
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"app", "exec", "-i", "--help"}, &stdout, &stderr); code != 0 || !strings.HasPrefix(stdout.String(), "usage: forge app exec") {
		t.Errorf("exit %d, stdout %q", code, stdout.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("the flag set's own usage was printed as well: %q", stderr.String())
	}
	if strings.Count(stdout.String(), "usage: ") != 1 {
		t.Errorf("help printed more than once: %q", stdout.String())
	}
}

func TestHelpOfACommandAfterHelpAndTheHelpFlagOfHelp(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"help", "new", "--help"}, {"help", "new", "-h"}, {"help", "new"}} {
		var stdout, stderr bytes.Buffer
		if code := Run(args, &stdout, &stderr); code != 0 || !strings.HasPrefix(stdout.String(), "usage: forge new NAME") || stderr.Len() != 0 {
			t.Errorf("%v: exit %d, stdout %q, stderr %q", args, code, stdout.String(), stderr.String())
		}
	}
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"help", "--help"}, &stdout, &stderr); code != 0 || !strings.HasPrefix(stdout.String(), "usage: forge help") {
		t.Errorf("help --help: exit %d, stdout %q", code, stdout.String())
	}
}

func TestUnknownTopLevelCommandAndUnknownHelpTopicBothExit2(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"nope"}, {"help", "nope"}} {
		var stdout, stderr bytes.Buffer
		if code := Run(args, &stdout, &stderr); code != 2 {
			t.Errorf("forge %v: exit %d, want 2", args, code)
		}
	}
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"--error-format=json", "help", "nope"}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), `"exit_code":2`) {
		t.Errorf("json mode: exit %d, stderr %q", code, stderr.String())
	}
}
