package telemetry

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// secretValue must never appear in the output of a logger that redacts.
const secretValue = "s3cr3t-value-42"

// allFormats builds a logger of every output format that writes to out with
// the redactor of extra, the way forge.Run does.
func allFormats(out *bytes.Buffer, extra ...string) map[string]*slog.Logger {
	opts := func() *slog.HandlerOptions {
		return &slog.HandlerOptions{ReplaceAttr: Redactor(extra...)}
	}
	return map[string]*slog.Logger{
		"json":   slog.New(NewOutputHandler(out, LogFormatJSON, opts())),
		"text":   slog.New(NewOutputHandler(out, LogFormatText, opts())),
		"pretty": slog.New(NewOutputHandler(out, LogFormatPretty, opts())),
	}
}

func TestRedactorHidesEveryDefaultKey(t *testing.T) {
	keys := []string{
		"password", "passwd", "secret", "token", "authorization", "cookie", "api_key", "api-key", "apikey",
		"access_key", "private_key", "credential", "session", "csrf", "jwt", "otp", "dsn", "database_url",
		"email", "phone", "cpf", "cnpj",
	}
	if len(keys) != len(DefaultRedactKeys) {
		t.Fatalf("the default list changed: %v", DefaultRedactKeys)
	}
	for _, key := range keys {
		// The key alone, as a fragment of a longer one, in another case, and
		// as the sibling of an unrelated attribute.
		for _, variant := range []string{key, "user_" + key + "_hash", strings.ToUpper(key), strings.ToUpper(key[:1]) + key[1:], "x." + key, "x-" + key} {
			var out bytes.Buffer
			for format, logger := range allFormats(&out) {
				out.Reset()
				logger.Info("m", variant, secretValue, "visible", "shown")
				got := out.String()
				if strings.Contains(got, secretValue) {
					t.Errorf("%s: key %q leaked: %s", format, variant, got)
				}
				if !strings.Contains(got, Redacted) || !strings.Contains(got, "shown") {
					t.Errorf("%s: key %q: %s", format, variant, got)
				}
			}
		}
	}
}

func TestRedactorHidesNestedGroups(t *testing.T) {
	var out bytes.Buffer
	for format, logger := range allFormats(&out) {
		out.Reset()
		logger.Info("m",
			slog.Group("user", slog.String("email", secretValue), slog.String("name", "Ana"),
				slog.Group("auth", slog.String("Token", secretValue))),
			// A member of a sensitive group is redacted whatever its key.
			slog.Group("credentials_password", slog.String("value", secretValue)),
			slog.Group("headers", slog.Group("cookie", slog.String("a", secretValue))))
		logger.WithGroup("request").With("api_key", secretValue).WithGroup("deep").Info("n", "session_id", secretValue)
		got := out.String()
		if strings.Contains(got, secretValue) {
			t.Errorf("%s: leaked: %s", format, got)
		}
		if !strings.Contains(got, "Ana") {
			t.Errorf("%s: unrelated attribute lost: %s", format, got)
		}
		if strings.Count(got, Redacted) != 6 {
			t.Errorf("%s: %d redactions: %s", format, strings.Count(got, Redacted), got)
		}
	}
}

func TestRedactorKeepsTheJSONShape(t *testing.T) {
	var out bytes.Buffer
	allFormats(&out)["json"].Info("m", "password", secretValue, "n", 3)
	var record map[string]any
	if err := json.Unmarshal(out.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if record["password"] != Redacted || record["n"] != float64(3) || record["msg"] != "m" {
		t.Fatalf("%v", record)
	}
}

func TestRedactorExtraKeys(t *testing.T) {
	var out bytes.Buffer
	for format, logger := range allFormats(&out, "IBAN", " card ", "") {
		out.Reset()
		logger.Info("m", "customer_iban", secretValue, "Card", secretValue, "other", "shown")
		if got := out.String(); strings.Contains(got, secretValue) || !strings.Contains(got, "shown") {
			t.Errorf("%s: %s", format, got)
		}
	}
	// The defaults still apply.
	out.Reset()
	allFormats(&out, "iban")["json"].Info("m", "password", secretValue)
	if strings.Contains(out.String(), secretValue) {
		t.Fatal("extra keys replaced the defaults")
	}
}

func TestRedactorLeavesOtherAttributes(t *testing.T) {
	replace := Redactor()
	for _, key := range []string{"request_id", "trace_id", "route", "status", "duration", "queries", "queue", "kind", "worker_id", "job_id", "statement", "address", "error", "msg", "time", "level"} {
		if got := replace(nil, slog.String(key, "v")); got.Value.String() != "v" {
			t.Errorf("%s redacted", key)
		}
	}
	group := slog.Group("g", slog.String("a", "b"))
	if got := replace(nil, group); got.Value.Kind() != slog.KindGroup {
		t.Fatal("a group attribute passes through untouched")
	}
}

func TestRedactorAppliesToTheExport(t *testing.T) {
	// The log exporter applies the ReplaceAttr the output handler reports, so
	// what standard output hides is hidden in the export too.
	handler := NewOutputHandler(&bytes.Buffer{}, LogFormatJSON, &slog.HandlerOptions{ReplaceAttr: Redactor()})
	got := logOutputOf(handler)
	if got.ReplaceAttr == nil || got.ReplaceAttr(nil, slog.String("password", "x")).Value.String() != Redacted {
		t.Fatal("the redactor is not reported to the exporter")
	}
}
