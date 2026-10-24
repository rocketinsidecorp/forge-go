package mail_test

import (
	"bytes"
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/config"
	forgemail "github.com/rocketinsidecorp/forge-go/mail"
)

func configFor(t *testing.T, env map[string]string) config.Config {
	t.Helper()
	cfg, err := config.LoadWithLookup(func(key string) (string, bool) { value, ok := env[key]; return value, ok })
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestFromConfigNotConfigured(t *testing.T) {
	t.Parallel()
	sender, err := forgemail.FromConfig(config.Default())
	if !errors.Is(err, forgemail.ErrNotConfigured) || sender != nil {
		t.Fatalf("sender=%v err=%v", sender, err)
	}
}

func TestFromConfigFileSenderInDevelopment(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "mails")
	cfg := configFor(t, map[string]string{"FORGE_MAIL_URL": "file://" + dir, "FORGE_MAIL_FROM": "app@example.com"})
	sender, err := forgemail.FromConfig(cfg, forgemail.WithLogger(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))))
	if err != nil {
		t.Fatal(err)
	}
	fileSender, ok := sender.(*forgemail.FileSender)
	if !ok {
		t.Fatalf("sender = %T", sender)
	}
	t.Cleanup(func() { _ = fileSender.Close() })
	if err := sender.Send(context.Background(), baseMessage()); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("entries = %v", entries)
	}
}

func TestFromConfigRefusesFileSenderInProductionEvenWhenValidationIsBypassed(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "mails")
	cfg := config.Default()
	cfg.Environment = config.Production
	cfg.Mail = config.Mail{URL: config.NewSecret("file://" + dir), From: "app@example.com"}
	sender, err := forgemail.FromConfig(cfg)
	if err == nil || sender != nil {
		t.Fatalf("sender=%v err=%v", sender, err)
	}
	if _, statErr := os.Stat(dir); statErr == nil {
		t.Fatal("the production refusal still created the directory")
	}
}

func TestFromConfigSMTPWithPrivateCA(t *testing.T) {
	t.Parallel()
	cert, _ := testCert(t)
	server := newFakeSMTPWithCert(t, cert, nil)
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"FORGE_MAIL_URL":         fmt.Sprintf("smtp://%s:%s@%s:%d", testUser, testPass, server.host(), server.port()),
		"FORGE_MAIL_FROM":        "App <app@example.com>",
		"FORGE_MAIL_TLS_CA_FILE": caFile,
	}
	sender, err := forgemail.FromConfig(configFor(t, env))
	if err != nil {
		t.Fatal(err)
	}
	if err := sender.Send(context.Background(), baseMessage()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if session := server.Sessions()[0]; !session.TLS || !session.AuthSucceeded {
		t.Fatalf("session = %+v", session)
	}

	// Without the CA the same configuration must fail verification.
	delete(env, "FORGE_MAIL_TLS_CA_FILE")
	untrusted, err := forgemail.FromConfig(configFor(t, env))
	if err != nil {
		t.Fatal(err)
	}
	if err := untrusted.Send(context.Background(), baseMessage()); !errors.Is(err, forgemail.ErrTLSFailed) {
		t.Fatalf("err = %v, want ErrTLSFailed", err)
	}
}

func TestFromConfigPlaintextOverride(t *testing.T) {
	t.Parallel()
	server := newFakeSMTP(t, func(s *fakeSMTP) { s.offerSTARTTLS = false; s.authOnPlain = true })
	env := map[string]string{
		"FORGE_ENV":            "production",
		"FORGE_HTTP_TRANSPORT": "trusted-proxy",
		"FORGE_MAIL_URL":       fmt.Sprintf("smtp://%s:%s@%s:%d", testUser, testPass, server.host(), server.port()),
		"FORGE_MAIL_FROM":      "app@example.com",
	}
	strict, err := forgemail.FromConfig(configFor(t, env))
	if err != nil {
		t.Fatal(err)
	}
	if err := strict.Send(context.Background(), baseMessage()); !errors.Is(err, forgemail.ErrTLSRequired) {
		t.Fatalf("production default must refuse plaintext, err = %v", err)
	}
	env["FORGE_MAIL_ALLOW_PLAINTEXT"] = "true"
	relaxed, err := forgemail.FromConfig(configFor(t, env))
	if err != nil {
		t.Fatal(err)
	}
	if err := relaxed.Send(context.Background(), baseMessage()); err != nil {
		t.Fatalf("explicit override failed: %v", err)
	}
}

func TestFromConfigWarnsAboutPlaintextWithoutCredentialsInProduction(t *testing.T) {
	t.Parallel()
	base := func() map[string]string {
		return map[string]string{
			"FORGE_ENV":                  "production",
			"FORGE_HTTP_TRANSPORT":       "trusted-proxy",
			"FORGE_MAIL_URL":             "smtp://relay.example.com:25",
			"FORGE_MAIL_FROM":            "app@example.com",
			"FORGE_MAIL_ALLOW_PLAINTEXT": "true",
		}
	}
	logged := func(env map[string]string) string {
		var out bytes.Buffer
		if _, err := forgemail.FromConfig(configFor(t, env), forgemail.WithLogger(slog.New(slog.NewTextHandler(&out, nil)))); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	if out := logged(base()); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "strip STARTTLS") {
		t.Fatalf("no warning: %q", out)
	}
	withoutFlag := base()
	delete(withoutFlag, "FORGE_MAIL_ALLOW_PLAINTEXT")
	development := base()
	delete(development, "FORGE_ENV")
	delete(development, "FORGE_HTTP_TRANSPORT")
	credentials := base()
	credentials["FORGE_MAIL_URL"] = "smtp://user:pass@localhost:25"
	for name, env := range map[string]map[string]string{"no flag": withoutFlag, "development": development, "credentials": credentials} {
		if out := logged(env); strings.Contains(out, "STARTTLS") || strings.Contains(out, "pass") {
			t.Errorf("%s: unexpected output %q", name, out)
		}
	}
}

func TestFromConfigBadCAFileDoesNotLeak(t *testing.T) {
	t.Parallel()
	bad := filepath.Join(t.TempDir(), "bad.pem")
	if err := os.WriteFile(bad, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{bad, filepath.Join(t.TempDir(), "missing.pem")} {
		cfg := configFor(t, map[string]string{
			"FORGE_MAIL_URL":         "smtp://mailer:topsecret@smtp.example.com:587",
			"FORGE_MAIL_FROM":        "app@example.com",
			"FORGE_MAIL_TLS_CA_FILE": path,
		})
		_, err := forgemail.FromConfig(cfg)
		if err == nil {
			t.Fatalf("%s accepted", path)
		}
		if strings.Contains(err.Error(), "topsecret") {
			t.Fatalf("error leaks the password: %v", err)
		}
	}
}

func TestDefaultFrom(t *testing.T) {
	t.Parallel()
	cfg := configFor(t, map[string]string{"FORGE_MAIL_URL": "smtp://u:p@h:587", "FORGE_MAIL_FROM": "Forge App <app@example.com>"})
	from, err := forgemail.DefaultFrom(cfg)
	if err != nil || from.Name != "Forge App" || from.Email != "app@example.com" {
		t.Fatalf("from = %+v, %v", from, err)
	}
	if _, err := forgemail.DefaultFrom(config.Default()); err == nil {
		t.Fatal("missing FORGE_MAIL_FROM accepted")
	}
	bad := config.Default()
	bad.Mail.From = "quoted\"local\"@example.com"
	if _, err := forgemail.DefaultFrom(bad); err == nil {
		t.Fatal("invalid sender accepted")
	}
}
