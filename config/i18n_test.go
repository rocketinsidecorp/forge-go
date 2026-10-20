package config_test

import (
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/config"
)

func TestI18nDefaultLocale(t *testing.T) {
	t.Parallel()
	load := func(value string) (config.Config, error) {
		return config.LoadWithLookup(func(key string) (string, bool) {
			if key == "FORGE_I18N_DEFAULT_LOCALE" {
				return value, true
			}
			return "", false
		})
	}
	for _, valid := range []string{"", "en", "pt-BR", "zh-Hant-TW", "es-419"} {
		cfg, err := load(valid)
		if err != nil || cfg.I18n.DefaultLocale != valid {
			t.Errorf("FORGE_I18N_DEFAULT_LOCALE=%q: %v, %q", valid, err, cfg.I18n.DefaultLocale)
		}
	}
	for value, want := range map[string]string{
		"pt-br":       "must be spelled pt-BR",
		"pt_BR":       "must be a locale",
		"../../etc":   "must be a locale",
		"en/../x":     "must be a locale",
		"english-USA": "must be a locale",
	} {
		if _, err := load(value); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("FORGE_I18N_DEFAULT_LOCALE=%q: error %v, want %q", value, err, want)
		}
	}
}

func TestShowsMissingTranslations(t *testing.T) {
	t.Parallel()
	for environment, want := range map[config.Environment]bool{
		config.Development: true, config.Test: true, config.Staging: false, config.Production: false,
	} {
		cfg := config.Default()
		cfg.Environment = environment
		if cfg.ShowsMissingTranslations() != want {
			t.Errorf("%s: ShowsMissingTranslations() = %v", environment, !want)
		}
	}
}
