package auth

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// sharedThrottlerStub stands for a throttler every replica shares, such as
// auth/postgres.LoginThrottler.
type sharedThrottlerStub struct{}

func (sharedThrottlerStub) Attempt(context.Context, LoginThrottleKey) (LoginThrottleDecision, error) {
	return LoginThrottleDecision{Allowed: true}, nil
}

func (sharedThrottlerStub) Success(context.Context, LoginThrottleKey) error { return nil }

// TestProcessLocalThrottlesWarnInProduction: in production, a sign-in, MFA
// or password-reset service that counts attempts in memory (the default, or
// an explicit MemoryLoginThrottler) logs one warning when it is built, and
// still builds; a shared throttler, or another environment, logs nothing.
// Not parallel: it sets FORGE_ENV and the default logger.
func TestProcessLocalThrottlesWarnInProduction(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	cipher, err := NewAESGCMSecretCipher([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	memory := func() LoginThrottler {
		throttler, err := NewMemoryLoginThrottler(DefaultLoginThrottleConfig())
		if err != nil {
			t.Fatal(err)
		}
		return throttler
	}
	var recoveryLogs bytes.Buffer
	recoveryLogger := slog.New(slog.NewTextHandler(&recoveryLogs, nil))
	build := map[string]func(LoginThrottler) error{
		"sign-in": func(throttler LoginThrottler) error {
			var options []LoginOption
			if throttler != nil {
				options = append(options, WithLoginThrottler(throttler))
			}
			_, err := NewLoginService(&loginStoreStub{}, &sessionStub{}, options...)
			return err
		},
		"two-factor": func(throttler LoginThrottler) error {
			var options []MFAOption
			if throttler != nil {
				options = append(options, WithMFAThrottler(throttler))
			}
			_, err := NewMFAService(&mfaStoreStub{}, &sessionStub{}, cipher, options...)
			return err
		},
		"password-reset": func(throttler LoginThrottler) error {
			options := []RecoveryOption{WithRecoveryLogger(recoveryLogger)}
			if throttler != nil {
				options = append(options, WithRecoveryThrottler(throttler))
			}
			_, err := NewRecoveryService(&recoveryStoreStub{}, &recoverySenderStub{}, options...)
			return err
		},
	}
	output := func(service string) *bytes.Buffer {
		if service == "password-reset" {
			return &recoveryLogs // the service's own logger
		}
		return &logs
	}

	for _, environment := range []string{"development", "test", "staging", "production"} {
		t.Setenv("FORGE_ENV", environment)
		for service, construct := range build {
			for name, throttler := range map[string]LoginThrottler{"default": nil, "explicit memory": memory(), "shared": sharedThrottlerStub{}} {
				logs.Reset()
				recoveryLogs.Reset()
				if err := construct(throttler); err != nil {
					t.Fatalf("%s %s in %s: %v", service, name, environment, err)
				}
				written := output(service).String()
				wantWarning := environment == "production" && name != "shared"
				if got := strings.Count(written, "level=WARN"); got != map[bool]int{true: 1, false: 0}[wantWarning] {
					t.Fatalf("%s with the %s throttler in %s logged %d warnings:\n%s%s", service, name, environment, got, logs.String(), recoveryLogs.String())
				}
				if wantWarning && (!strings.Contains(written, "service="+service) ||
					!strings.Contains(written, "auth/postgres.NewLoginThrottler") ||
					!strings.Contains(written, "every restart or deploy resets it")) {
					t.Fatalf("%s warning does not explain itself:\n%s", service, written)
				}
				if service == "password-reset" && logs.Len() != 0 {
					t.Fatalf("the password-reset warning bypassed WithRecoveryLogger:\n%s", logs.String())
				}
			}
		}
	}
}
