package auth

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// TestSecretCarriersNeverRevealTheirSecret formats every type that carries a
// credential or MFA secret the ways diagnostics do, alone and inside a struct
// or slice, and writes it through real slog handlers. None of the output may
// contain the secret.
func TestSecretCarriersNeverRevealTheirSecret(t *testing.T) {
	t.Parallel()
	session, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := NewRecoveryToken()
	if err != nil {
		t.Fatal(err)
	}
	challenge, err := NewMFAChallengeToken()
	if err != nil {
		t.Fatal(err)
	}
	enrollment := newTOTPEnrollment([]byte("12345678901234567890"), "Forge", "ada", time.Unix(0, 0))
	backup, _, err := newMFABackupCodes(2)
	if err != nil {
		t.Fatal(err)
	}
	type holder struct {
		Session   Token
		Recovery  RecoveryToken
		Challenge MFAChallengeToken
		TOTP      TOTPEnrollment
		Codes     TOTPConfirmation
	}
	carrier := holder{session, recovery, challenge, enrollment, TOTPConfirmation{BackupCodes: backup}}

	cases := []struct {
		name    string
		value   any
		secrets []string
	}{
		{"session token", session, []string{session.Reveal()}},
		{"recovery token", recovery, []string{recovery.Reveal()}},
		{"MFA challenge token", challenge, []string{challenge.Reveal()}},
		{"TOTP secret", enrollment.Secret, []string{enrollment.Secret.Reveal()}},
		{"provisioning URI", enrollment.ProvisioningURI, []string{enrollment.ProvisioningURI.Reveal(), enrollment.Secret.Reveal()}},
		{"backup code", backup[0], []string{backup[0].Reveal(), strings.ReplaceAll(backup[0].Reveal(), "-", "")}},
		{"backup code slice", backup, []string{backup[0].Reveal(), backup[1].Reveal()}},
		{"TOTP enrollment", enrollment, []string{enrollment.Secret.Reveal(), enrollment.ProvisioningURI.Reveal()}},
		{"everything in one struct", carrier, []string{
			session.Reveal(), recovery.Reveal(), challenge.Reveal(), enrollment.Secret.Reveal(),
			enrollment.ProvisioningURI.Reveal(), backup[0].Reveal(), backup[1].Reveal(),
		}},
		{"everything by pointer", &carrier, []string{session.Reveal(), recovery.Reveal(), challenge.Reveal(), enrollment.Secret.Reveal(), backup[0].Reveal()}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var outputs []string
			for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
				outputs = append(outputs, fmt.Sprintf(verb, tc.value))
			}
			outputs = append(outputs, fmt.Sprint(tc.value), fmt.Sprintf("%v", []any{tc.value}), fmt.Sprintf("%#v", map[string]any{"v": tc.value}))
			for name, handler := range map[string]func(*bytes.Buffer) slog.Handler{
				"json": func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
				"text": func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
			} {
				var buffer bytes.Buffer
				logger := slog.New(handler(&buffer))
				logger.Info("secret", "value", tc.value)
				logger.With("bound", tc.value).Info("bound", slog.Group("group", slog.Any("inner", tc.value)))
				if buffer.Len() == 0 {
					t.Fatalf("%s handler wrote nothing", name)
				}
				outputs = append(outputs, buffer.String())
			}
			for _, output := range outputs {
				for _, secret := range tc.secrets {
					if secret != "" && strings.Contains(output, secret) {
						t.Errorf("output leaks %q: %s", secret, output)
					}
				}
			}
		})
	}
}
