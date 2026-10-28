package forge_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	forge "github.com/rocketinsidecorp/forge-go"
)

func TestExecuteRejectsAnInvalidLogFormat(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	lookup := envLookup(map[string]string{"FORGE_ENV": "test", "FORGE_LOG_FORMAT": "xml"})
	code := forge.Execute(context.Background(), nil, lookup, &stdout, &stderr, func(context.Context, *forge.App) error { return nil })
	if code != 1 || !strings.Contains(stderr.String(), "FORGE_LOG_FORMAT") {
		t.Fatalf("code %d: %s", code, stderr.String())
	}
}
