package pgurl_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/internal/pgurl"
)

// A query string net/url parses but ParseQuery rejects (a bad escape, a
// semicolon) is an error that does not quote the URL, which holds a password.
func TestIsolateRejectsAMalformedQueryWithoutQuotingTheURL(t *testing.T) {
	for name, raw := range map[string]string{
		"bad escape": "postgres://u:hunter2@h/db?application_name=%zz",
		"semicolon":  "postgres://u:hunter2@h/db?application_name=a;sslmode=disable",
	} {
		iso, err := pgurl.Isolate(raw, pgurl.TLSOnly, nil)
		if err == nil || iso != nil {
			t.Errorf("%s: Isolate = %v, %v; want an error", name, iso, err)
			continue
		}
		if !strings.Contains(err.Error(), "malformed query string") || strings.Contains(err.Error(), "hunter2") || refused(err) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

// The pinned parameters are appended to the URL text exactly: with "?" when
// there is no query, with "&" after one, and with no separator at all when
// the URL already ends in one.
func TestPinsAreAppendedWithTheRightSeparator(t *testing.T) {
	t.Setenv("PGSSLMODE", "disable")
	const pin = "sslmode=prefer"
	for raw, want := range map[string]string{
		"postgres://u@h/db":                         "postgres://u@h/db?" + pin,
		"postgres://u@h/db?":                        "postgres://u@h/db?" + pin,
		"postgres://u@h/db?application_name=a":      "postgres://u@h/db?application_name=a&" + pin,
		"postgres://u@h/db?application_name=a&":     "postgres://u@h/db?application_name=a&" + pin,
		"postgres://u@h/db?application_name=a&app=": "postgres://u@h/db?application_name=a&app=&" + pin,
	} {
		iso, err := pgurl.Isolate(raw, pgurl.TLSOnly, nil)
		if err != nil {
			t.Errorf("%s: %v", raw, err)
			continue
		}
		if iso.URL != want {
			t.Errorf("Isolate(%q).URL = %q, want %q", raw, iso.URL, want)
		}
		iso.Close()
	}
}

// When the empty service file cannot be created the URL is refused with an
// error that names the cause and nothing from the URL, and nothing is left to
// close.
func TestIsolateFailsWhenTheServiceFileCannotBeCreated(t *testing.T) {
	t.Setenv("PGSERVICE", "s")
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))
	iso, err := pgurl.Isolate("postgres://u:hunter2@h/db", pgurl.Everything, nil)
	if err == nil || iso != nil {
		t.Fatalf("Isolate = %v, %v; want an error", iso, err)
	}
	if !strings.Contains(err.Error(), "create an empty PostgreSQL service file") || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("err = %v", err)
	}
}
