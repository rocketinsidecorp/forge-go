package mail_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/config"
	forgemail "github.com/rocketinsidecorp/forge-go/mail"
)

// An unset FORGE_ENV means development, so the environment alone must not be
// enough to serve previews: the flag has to be set on purpose.
func TestPreviewRequiresExplicitOptIn(t *testing.T) {
	t.Parallel()
	for _, env := range []config.Environment{config.Development, config.Test} {
		cfg := config.Default()
		cfg.Environment = env // what an unset FORGE_ENV yields: development, flag off
		if _, err := forgemail.NewPreviewHandler(cfg, "/_mail"); err == nil || !strings.Contains(err.Error(), "FORGE_MAIL_PREVIEW") {
			t.Errorf("%s without opt-in: err = %v", env, err)
		}
	}
	// The default configuration (nothing set) must not serve previews.
	loaded, err := config.LoadWithLookup(func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := forgemail.NewPreviewHandler(loaded, "/_mail"); err == nil {
		t.Fatal("previews created from an empty environment")
	}
	// Production stays refused even with the flag set by hand.
	cfg := previewConfig(config.Production)
	if _, err := forgemail.NewPreviewHandler(cfg, "/_mail"); err == nil {
		t.Fatal("previews created in production")
	}
}

func TestPreviewPrefixMatchesWholePathSegments(t *testing.T) {
	t.Parallel()
	handler := previewHandler(t)
	for _, path := range []string{"/_mailwelcome", "/_mailwelcome/html", "/_mail.welcome", "/_mail-welcome", "/_mailx"} {
		if rec := get(handler, http.MethodGet, path); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
	}
	if rec := get(handler, http.MethodGet, "/_mail/welcome"); rec.Code != http.StatusOK {
		t.Errorf("GET /_mail/welcome = %d", rec.Code)
	}
}

// The file sender silently swallows mail; if it is active by mistake (for
// example a production container that lost FORGE_ENV and kept a development
// URL) the operator must be able to see it in the logs.
func TestFileSenderAnnouncesThatMailIsNotDelivered(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	sender, err := forgemail.NewFileSender(t.TempDir(), slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sender.Close() })
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "NOT delivered") {
		t.Fatalf("no warning at start-up: %q", logs.String())
	}
	logs.Reset()
	if err := sender.Send(context.Background(), baseMessage()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "level=WARN") && strings.Contains(logs.String(), "alice@example.org") {
		t.Fatal("warning leaks recipients")
	}
}
