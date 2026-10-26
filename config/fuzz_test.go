package config_test

import (
	"net/url"
	"strings"
	"testing"
	"unicode"

	"github.com/rocketinsidecorp/forge-go/config"
)

// canary is planted in every fuzzed URL: configuration errors must never
// echo any part of a URL, which may hold a password.
const canary = "Kq7canaryZ"

func fuzzLookup(production bool, values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		if value, ok := values[key]; ok {
			return value, true
		}
		if production {
			switch key {
			case "FORGE_ENV":
				return "production", true
			case "FORGE_HTTP_TRANSPORT":
				return "trusted-proxy", true
			}
		}
		return "", false
	}
}

// FuzzDatabaseURL checks FORGE_DATABASE_URL validation, rejectAmbiguous
// included: errors never quote the URL, and an accepted URL is one that
// net/url and the database drivers read alike (no fragment, no "@" outside
// the userinfo, no whitespace or control character in the parameters, no
// repeated parameter), with one host and, in production, a verified TLS mode.
func FuzzDatabaseURL(f *testing.F) {
	for _, seed := range []struct {
		before, after string
		production    bool
	}{
		{"postgres://app:", "@db:5432/app?sslmode=verify-full", true},
		{"postgres://app:", "@db/app?sslmode=disable&sslmode=verify-full", true},
		{"postgres://app:", "@db/app?sslmode=verify-full& host=evil", true},
		{"postgres://app:", "@db/app?x=@evil/", false},
		{"postgres://app:", "@db/app#frag", false},
		{"postgres://app:", "@a,b/app", false},
		{"postgres://app:", "@a%2Cb/app", false},
		{"postgresql://", "@[::1]:5432/app?sslmode=verify-ca&sslrootcert=/etc/ca.pem", true},
		{"mysql://root:", "@db:3306/app?tls=true", true},
		{"mysql://root:", "@db:3306/app?tls=skip-verify", true},
		{"sqlite:storage/", ".db", false},
		{"sqlite:///var/lib/", "/app.db?_pragma=x", true},
		{"sqlite:", ":memory:", false},
		{"oracle://u:", "@db/app", false},
		{"postgres://u:", "\x7f@db/app", false},
		{"", "", false},
	} {
		f.Add(seed.before, seed.after, seed.production)
	}
	f.Fuzz(func(t *testing.T, before, after string, production bool) {
		raw := before + canary + after
		cfg, err := config.LoadWithLookup(fuzzLookup(production, map[string]string{"FORGE_DATABASE_URL": raw}))
		if err != nil {
			if strings.Contains(err.Error(), canary) {
				t.Fatalf("error quotes the URL: %v", err)
			}
			return
		}
		adapter := cfg.Database.Adapter()
		parsed, parseErr := url.Parse(raw)
		if adapter == "" || parseErr != nil {
			t.Fatalf("accepted %q without an adapter (%v)", raw, parseErr)
		}
		if parsed.Fragment != "" || strings.Contains(parsed.RawQuery, "@") || strings.Contains(parsed.Path, "@") {
			t.Fatalf("accepted ambiguous %q", raw)
		}
		if strings.IndexFunc(parsed.RawQuery, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
			t.Fatalf("accepted whitespace in the parameters of %q", raw)
		}
		query, err := url.ParseQuery(parsed.RawQuery)
		if err != nil {
			t.Fatalf("accepted a malformed query in %q", raw)
		}
		for key, values := range query {
			if len(values) != 1 {
				t.Fatalf("accepted repeated parameter %q in %q", key, raw)
			}
		}
		switch adapter {
		case config.AdapterPostgres:
			if parsed.Host == "" || strings.Contains(parsed.Host, ",") {
				t.Fatalf("accepted %q without exactly one host", raw)
			}
			if mode := query.Get("sslmode"); production && mode != "verify-full" && mode != "verify-ca" {
				t.Fatalf("production accepted sslmode %q in %q", mode, raw)
			}
			if production && query.Get("sslmode") == "verify-ca" && query.Get("sslrootcert") == "" {
				t.Fatalf("production accepted verify-ca without sslrootcert in %q", raw)
			}
		case config.AdapterMySQL:
			if parsed.Host == "" || strings.Trim(parsed.Path, "/") == "" {
				t.Fatalf("accepted %q without a host and database", raw)
			}
			if production && query.Get("tls") != "true" {
				t.Fatalf("production accepted %q without tls=true", raw)
			}
		case config.AdapterSQLite:
			if path := cfg.Database.SQLitePath(); path == "" || parsed.RawQuery != "" || parsed.Host != "" {
				t.Fatalf("accepted SQLite URL %q (path %q)", raw, path)
			}
		default:
			t.Fatalf("unknown adapter %q", adapter)
		}
	})
}

// FuzzMailURL checks FORGE_MAIL_URL validation: errors never quote the URL,
// and an accepted SMTP URL has one host, a valid port, no path and no
// parameters.
func FuzzMailURL(f *testing.F) {
	for _, seed := range []struct{ before, after string }{
		{"smtp://user:", "@mail.example.com:587"},
		{"smtps://user:", "@mail.example.com"},
		{"smtp://user:", "@a,b:25"},
		{"smtp://user:", "@mail:99999"},
		{"smtp://user:", "@mail/path"},
		{"smtp://user:", "@mail?tls=false"},
		{"smtp://user:", "@mail#x"},
		{"file:///var/tmp/", ""},
		{"file:tmp/", "%zz"},
		{"http://", "@x"},
	} {
		f.Add(seed.before, seed.after)
	}
	f.Fuzz(func(t *testing.T, before, after string) {
		raw := before + canary + after
		cfg, err := config.LoadWithLookup(fuzzLookup(false, map[string]string{
			"FORGE_MAIL_URL":  raw,
			"FORGE_MAIL_FROM": "App <app@example.com>",
		}))
		if err != nil {
			if strings.Contains(err.Error(), canary) {
				t.Fatalf("error quotes the URL: %v", err)
			}
			return
		}
		parsed, parseErr := url.Parse(raw)
		if parseErr != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			t.Fatalf("accepted %q", raw)
		}
		switch cfg.Mail.Transport() {
		case config.MailSTARTTLS, config.MailImplicitTLS:
			endpoint, ok := cfg.Mail.SMTP()
			if !ok || endpoint.Host == "" || strings.Contains(parsed.Host, ",") || endpoint.Port < 1 || endpoint.Port > 65535 {
				t.Fatalf("accepted SMTP URL %q as %+v, %v", raw, endpoint, ok)
			}
			if strings.Trim(parsed.Path, "/") != "" || parsed.Opaque != "" {
				t.Fatalf("accepted SMTP URL %q with a path", raw)
			}
		case config.MailFile:
			if cfg.Mail.FilePath() == "" {
				t.Fatalf("accepted file URL %q without a path", raw)
			}
		default:
			t.Fatalf("accepted %q with transport %q", raw, cfg.Mail.Transport())
		}
	})
}
