package config_test

import (
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/config"
)

func loadMail(env map[string]string) (config.Config, error) {
	return config.LoadWithLookup(func(key string) (string, bool) { value, ok := env[key]; return value, ok })
}

func production(extra map[string]string) map[string]string {
	env := map[string]string{"FORGE_ENV": "production", "FORGE_HTTP_TRANSPORT": "trusted-proxy"}
	for k, v := range extra {
		env[k] = v
	}
	return env
}

func TestMailConfiguration(t *testing.T) {
	t.Parallel()
	const from = "App <app@example.com>"
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{name: "mail disabled", env: map[string]string{}},
		{name: "development smtp", env: map[string]string{"FORGE_MAIL_URL": "smtp://u:p@localhost:1025", "FORGE_MAIL_FROM": from}},
		{name: "development smtp without credentials", env: map[string]string{"FORGE_MAIL_URL": "smtp://localhost:1025", "FORGE_MAIL_FROM": from, "FORGE_MAIL_ALLOW_PLAINTEXT": "true"}},
		{name: "development file", env: map[string]string{"FORGE_MAIL_URL": "file:///tmp/forge-mail", "FORGE_MAIL_FROM": from}},
		{name: "development relative file", env: map[string]string{"FORGE_MAIL_URL": "file:tmp/mails", "FORGE_MAIL_FROM": from}},
		{name: "production smtp", env: production(map[string]string{"FORGE_MAIL_URL": "smtp://u:p@smtp.example.com:587", "FORGE_MAIL_FROM": from})},
		{name: "production smtps", env: production(map[string]string{"FORGE_MAIL_URL": "smtps://u:p%40x@smtp.example.com:465", "FORGE_MAIL_FROM": "app@example.com"})},
		{name: "production plaintext override", env: production(map[string]string{"FORGE_MAIL_URL": "smtp://relay.internal:25", "FORGE_MAIL_FROM": from, "FORGE_MAIL_ALLOW_PLAINTEXT": "true"})},
		{name: "production refuses file sender", env: production(map[string]string{"FORGE_MAIL_URL": "file:///tmp/mail", "FORGE_MAIL_FROM": from}), wantErr: "production cannot use the development file mail sender"},
		{name: "production file refused even with plaintext override", env: production(map[string]string{"FORGE_MAIL_URL": "file:///tmp/mail", "FORGE_MAIL_FROM": from, "FORGE_MAIL_ALLOW_PLAINTEXT": "true"}), wantErr: "development file"},
		{name: "staging allows file", env: map[string]string{"FORGE_ENV": "staging", "FORGE_MAIL_URL": "file:///tmp/mail", "FORGE_MAIL_FROM": from}},
		{name: "unknown scheme", env: map[string]string{"FORGE_MAIL_URL": "http://smtp.example.com", "FORGE_MAIL_FROM": from}, wantErr: "FORGE_MAIL_URL must be"},
		{name: "ses scheme", env: map[string]string{"FORGE_MAIL_URL": "ses://key:secret@us-east-1", "FORGE_MAIL_FROM": from}, wantErr: "FORGE_MAIL_URL must be"},
		{name: "missing from", env: map[string]string{"FORGE_MAIL_URL": "smtp://u:p@h:587"}, wantErr: "FORGE_MAIL_FROM is required"},
		{name: "bad from", env: map[string]string{"FORGE_MAIL_URL": "smtp://u:p@h:587", "FORGE_MAIL_FROM": "not an address"}, wantErr: "FORGE_MAIL_FROM must be"},
		{name: "from with header injection", env: map[string]string{"FORGE_MAIL_URL": "smtp://u:p@h:587", "FORGE_MAIL_FROM": "a@b.com\r\nBcc: x@y.com"}, wantErr: "FORGE_MAIL_FROM"},
		{name: "no host", env: map[string]string{"FORGE_MAIL_URL": "smtp://:587", "FORGE_MAIL_FROM": from}, wantErr: "one host"},
		{name: "multiple hosts", env: map[string]string{"FORGE_MAIL_URL": "smtp://a:1,b:2", "FORGE_MAIL_FROM": from}, wantErr: "FORGE_MAIL_URL"},
		{name: "bad port", env: map[string]string{"FORGE_MAIL_URL": "smtp://u:p@h:99999", "FORGE_MAIL_FROM": from}, wantErr: "port"},
		{name: "user without password", env: map[string]string{"FORGE_MAIL_URL": "smtp://user@h:587", "FORGE_MAIL_FROM": from}, wantErr: "credentials"},
		{name: "query rejected", env: map[string]string{"FORGE_MAIL_URL": "smtp://u:p@h:587?tls=skip-verify", "FORGE_MAIL_FROM": from}, wantErr: "no parameters"},
		{name: "path rejected", env: map[string]string{"FORGE_MAIL_URL": "smtp://u:p@h:587/inbox", "FORGE_MAIL_FROM": from}, wantErr: "path"},
		{name: "ambiguous fragment", env: map[string]string{"FORGE_MAIL_URL": "smtp://u:p@h:587#x", "FORGE_MAIL_FROM": from}, wantErr: "ambiguous"},
		{name: "file with host", env: map[string]string{"FORGE_MAIL_URL": "file://host/dir", "FORGE_MAIL_FROM": from}, wantErr: "file:///absolute"},
		{name: "file root", env: map[string]string{"FORGE_MAIL_URL": "file:///", "FORGE_MAIL_FROM": from}, wantErr: "file:///absolute"},
		{name: "bad plaintext flag", env: map[string]string{"FORGE_MAIL_ALLOW_PLAINTEXT": "maybe"}, wantErr: "FORGE_MAIL_ALLOW_PLAINTEXT"},
		{name: "bad timeout", env: map[string]string{"FORGE_MAIL_TIMEOUT": "soon"}, wantErr: "FORGE_MAIL_TIMEOUT"},
		{name: "negative timeout", env: map[string]string{"FORGE_MAIL_TIMEOUT": "-1s"}, wantErr: "FORGE_MAIL_TIMEOUT"},
		{name: "huge timeout", env: map[string]string{"FORGE_MAIL_TIMEOUT": "2h"}, wantErr: "FORGE_MAIL_TIMEOUT"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := loadMail(tc.env)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatal("expected an error")
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("error %q does not mention %q", err, tc.wantErr)
			}
			if err != nil {
				for _, value := range tc.env {
					if strings.Contains(value, ":p@") || strings.Contains(value, "secret@") {
						if strings.Contains(err.Error(), value) {
							t.Fatalf("error echoes the URL: %v", err)
						}
					}
				}
			}
		})
	}
}

func TestMailLoadedValuesAndAccessors(t *testing.T) {
	t.Parallel()
	cfg, err := loadMail(map[string]string{
		"FORGE_MAIL_URL":             "smtps://mailer:p%40ss%2Fword@smtp.example.com",
		"FORGE_MAIL_FROM":            "App <app@example.com>",
		"FORGE_MAIL_TIMEOUT":         "12s",
		"FORGE_MAIL_TLS_CA_FILE":     "/etc/ssl/relay.pem",
		"FORGE_MAIL_ALLOW_PLAINTEXT": "false",
	})
	if err != nil {
		t.Fatal(err)
	}
	mail := cfg.Mail
	if mail.Transport() != config.MailImplicitTLS || mail.From != "App <app@example.com>" || mail.Timeout != 12*time.Second || mail.CAFile != "/etc/ssl/relay.pem" || mail.AllowPlaintext {
		t.Fatalf("mail = %+v", mail)
	}
	endpoint, ok := mail.SMTP()
	if !ok || endpoint.Host != "smtp.example.com" || endpoint.Port != 465 || !endpoint.ImplicitTLS || endpoint.Username != "mailer" || endpoint.Password.Reveal() != "p@ss/word" {
		t.Fatalf("endpoint = %+v ok=%v", endpoint, ok)
	}
	if mail.FilePath() != "" {
		t.Fatal("FilePath for an smtp URL")
	}

	starttls, err := loadMail(map[string]string{"FORGE_MAIL_URL": "smtp://u:p@[::1]", "FORGE_MAIL_FROM": "a@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	endpoint, _ = starttls.Mail.SMTP()
	if endpoint.Host != "::1" || endpoint.Port != 587 || endpoint.ImplicitTLS {
		t.Fatalf("endpoint = %+v", endpoint)
	}
	if starttls.Mail.EffectiveTimeout() != 30*time.Second {
		t.Fatalf("default timeout = %v", starttls.Mail.EffectiveTimeout())
	}

	for url, want := range map[string]string{"file:///var/tmp/forge-mail": "/var/tmp/forge-mail", "file:tmp/mails": "tmp/mails"} {
		cfg, err := loadMail(map[string]string{"FORGE_MAIL_URL": url, "FORGE_MAIL_FROM": "a@example.com"})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Mail.Transport() != config.MailFile || cfg.Mail.FilePath() != want {
			t.Fatalf("%s: transport=%q path=%q", url, cfg.Mail.Transport(), cfg.Mail.FilePath())
		}
		if _, ok := cfg.Mail.SMTP(); ok {
			t.Fatal("SMTP for a file URL")
		}
	}
	empty, _ := loadMail(map[string]string{})
	if empty.Mail.Transport() != config.MailDisabled {
		t.Fatal("mail should be disabled without a URL")
	}
}

func TestMailURLIsRedactedEverywhere(t *testing.T) {
	t.Parallel()
	const password = "hunter2-mail-password"
	cfg, err := loadMail(map[string]string{
		"FORGE_MAIL_URL":  "smtp://mailer:" + password + "@smtp.example.com:587",
		"FORGE_MAIL_FROM": "app@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	formatted := fmt.Sprintf("%v|%+v|%#v|%s", cfg.Mail, cfg.Mail, cfg.Mail, cfg.Mail.URL)
	if strings.Contains(formatted, password) {
		t.Fatalf("formatting leaks the password: %s", formatted)
	}
	endpoint, _ := cfg.Mail.SMTP()
	if strings.Contains(fmt.Sprintf("%v %+v %#v", endpoint, endpoint, endpoint), password) {
		t.Fatal("endpoint formatting leaks the password")
	}
	var logs strings.Builder
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	logger.Info("config", "mail", cfg.Mail, "url", cfg.Mail.URL)
	if strings.Contains(logs.String(), password) {
		t.Fatalf("slog leaks the password: %s", logs.String())
	}
	text, _ := cfg.Mail.URL.MarshalText()
	if strings.Contains(string(text), password) {
		t.Fatal("MarshalText leaks the password")
	}
}

func TestMalformedMailURLDoesNotLeakPassword(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"smtp://user:pa%zzss@host:587",
		"smtp://user:pass word@host:99999999999999999999",
		"smtp://user:topsecret@ho st:587",
		"%%topsecret",
	} {
		_, err := loadMail(map[string]string{"FORGE_MAIL_URL": raw, "FORGE_MAIL_FROM": "a@example.com"})
		if err == nil {
			t.Errorf("%q accepted", raw)
			continue
		}
		for _, leak := range []string{"topsecret", "pa%zzss", "pass word"} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("error for %q leaks %q: %v", raw, leak, err)
			}
		}
	}
}

func TestDefaultMailDoesNotAffectExistingConfigs(t *testing.T) {
	t.Parallel()
	if err := config.Default().Validate(); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Mail.AllowPlaintext = true // meaningless without a URL, harmless
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}
