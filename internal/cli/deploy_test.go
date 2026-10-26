package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestDeployCommandsAreRegistered(t *testing.T) {
	t.Parallel()
	// An invalid command line exits 2, as the README documents; a failed
	// command (below, a missing configuration) exits 1.
	for _, args := range [][]string{{"deploy", "explode"}, {"deploy", "--nope"}, {"deploy", "rollback", "--nope"}, {"logs", "extra"}, {"logs", "--nope"}, {"app"}, {"app", "exec", "--nope"}} {
		var stdout, stderr bytes.Buffer
		if code := Run(args, &stdout, &stderr); code != 2 || strings.Contains(stderr.String(), "unknown command") {
			t.Errorf("%v: code=%d stderr=%s", args, code, stderr.String())
		}
	}
	var stdout, stderr bytes.Buffer
	if code := Run([]string{jsonErrorFormatFlag, "deploy", "--config", "does-not-exist.yml"}, &stdout, &stderr); code != 1 {
		t.Fatalf("code = %d", code)
	}
	var report cliErrorReport
	if err := json.Unmarshal(stderr.Bytes(), &report); err != nil || report.Error.Command != "deploy" || report.Error.ExitCode != 1 {
		t.Fatalf("json error = %s (%v)", stderr.String(), err)
	}
}
