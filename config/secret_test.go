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
