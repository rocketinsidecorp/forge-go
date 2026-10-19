package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestDeployCommandsAreRegistered(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"deploy", "explode"}, {"logs", "extra"}, {"app"}} {
		var stdout, stderr bytes.Buffer
		if code := Run(args, &stdout, &stderr); code != 1 || strings.Contains(stderr.String(), "unknown command") {
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
