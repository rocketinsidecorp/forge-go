package config_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/config"
)

func TestSecretIsRedactedEverywhere(t *testing.T) {
	t.Parallel()
	const value = "postgres://app:hunter2@db:5432/app"
	secret := config.NewSecret(value)
	cfg := config.Default()
	cfg.Database.URL = secret

	var logs bytes.Buffer
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("config", "database_url", secret, "config", cfg)
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	outputs := map[string]string{
		"%v":         fmt.Sprintf("%v", secret),
		"%q":         fmt.Sprintf("%q", secret),
		"%#v":        fmt.Sprintf("%#v", secret),
		"%+v":        fmt.Sprintf("%+v", cfg),
		"%#v config": fmt.Sprintf("%#v", cfg),
		"json":       string(encoded),
		"slog":       logs.String(),
	}
	for name, output := range outputs {
		if strings.Contains(output, "hunter2") {
			t.Errorf("%s leaked the secret: %s", name, output)
		}
	}
	if secret.Reveal() != value {
		t.Fatal("Reveal must return the original value")
	}
}

type holder struct {
	secret config.Secret
	Public config.Secret
}

type embeddedHolder struct{ config.Secret }

// TestSecretIsRedactedInUnexportedFields: fmt cannot call methods on
// unexported fields, so the representation itself must not hold the text.
func TestSecretIsRedactedInUnexportedFields(t *testing.T) {
	t.Parallel()
	const value = "hunter2-unexported"
	secret := config.NewSecret(value)
	for name, v := range map[string]any{
		"unexported field": holder{secret: secret, Public: secret},
		"pointer":          &holder{secret: secret},
		"embedded":         embeddedHolder{secret},
		"slice":            []config.Secret{secret},
		"map":              map[string]config.Secret{"k": secret},
	} {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
			if out := fmt.Sprintf(verb, v); strings.Contains(out, value) {
				t.Errorf("%s with %s leaked the secret: %s", name, verb, out)
			}
		}
	}
	if !secret.Equal(config.NewSecret(value)) || secret.Equal(config.NewSecret("other")) || !config.NewSecret("").Equal(config.Secret{}) {
		t.Error("Equal is wrong")
	}
	if !config.NewSecret("").IsZero() || secret.IsZero() || secret.Reveal() != value {
		t.Error("zero or reveal is wrong")
	}
}
