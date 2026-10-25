package deploy

import (
	"strings"
	"testing"
)

func TestParseConfigRejectsMoreInvalidConfigurations(t *testing.T) {
	t.Parallel()
	replace := func(old, new string) string {
		if !strings.Contains(validConfig, old) {
			t.Fatalf("fixture lacks %q", old)
		}
		return strings.Replace(validConfig, old, new, 1)
	}
	base := "service: shop\nimage: registry.example.com/acme/shop\n"
	cases := map[string]struct{ config, want string }{
		"username whitespace":     {replace("username: acme", "username: 'a b'"), "registry.username contains whitespace"},
		"no roles":                {base + "servers: {}\n", "servers must define at least one role"},
		"bad role name":           {base + "servers:\n  Web_1:\n    hosts: [192.0.2.10]\n", "role name must match"},
		"role clear is secret":    {replace("      clear:\n        WORKERS: \"4\"", "      clear:\n        FORGE_DATABASE_URL: x"), "is also a secret in env.secret"},
		"role secret is clear":    {replace("        WORKERS: \"4\"", "        WORKERS: \"4\"\n      secret: [FORGE_ENV]"), "is also in env.clear"},
		"bad accessory name":      {replace("  db:\n    image: postgres", "  DB_1:\n    image: postgres"), "name must match"},
		"accessory without image": {replace("    image: postgres:18-alpine\n", ""), "image is required"},
		"builder option":          {replace("builder:\n", "builder:\n  context: -evil\n"), "must not start with '-'"},
		"builder arg":             {replace("GO_VERSION: \"1.27\"", "\"BAD-KEY\": x"), "builder.args.BAD-KEY is invalid"},
		"ssh port negative":       {replace("port: 2222", "port: -1"), "ssh.port must be between"},
		"ssh port too high":       {replace("port: 2222", "port: 70000"), "ssh.port must be between"},
		"proxy host twice":        {replace("hosts: [Shop.Example.com]", "hosts: [shop.example.com, SHOP.example.com]"), "is listed twice"},
		"acme email":              {replace("  ssl: true", "  ssl: true\n  acme_email: not-an-email"), "acme_email"},
		"app port":                {replace("  ssl: true", "  ssl: true\n  app_port: 70000"), "app_port"},
		"healthy count":           {replace("interval: 500ms", "interval: 500ms\n    healthy_count: 500"), "healthy_count"},
		"negative body":           {replace("  ssl: true", "  ssl: true\n  max_body_bytes: -5"), "max_body_bytes"},
		"secret name":             {replace("secret: [FORGE_DATABASE_URL]", "secret: [bad-name]"), "not a valid secret name"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseConfig([]byte(tc.config))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestParseConfigDefaultsRetainAndPrimaryRole(t *testing.T) {
	t.Parallel()
	cfg, err := ParseConfig([]byte(strings.Replace(validConfig, "retain: 3\n", "", 1)))
	if err != nil || cfg.Retain != DefaultRetain {
		t.Fatalf("retain = %d, %v", cfg.Retain, err)
	}
	// Without a proxied role or a "web" role, the first role by name leads.
	cfg = &Config{Servers: map[string]Role{"zeta": {}, "alpha": {}}}
	if got := cfg.PrimaryRole(); got != "alpha" {
		t.Errorf("PrimaryRole = %q", got)
	}
	if got := (&Config{}).PrimaryRole(); got != "" {
		t.Errorf("PrimaryRole of no roles = %q", got)
	}
}

func TestPublishSpecRefusesBadPortsAndAddresses(t *testing.T) {
	t.Parallel()
	for _, port := range []string{"0", "65536", "80:0", "0:80", "bad:80", "300.1.1.1:80:80", "10.0.0.5:0:80", "10.0.0.5:80:0", ":80:80"} {
		if got, err := publishSpec(port); err == nil {
			t.Errorf("publishSpec(%q) = %q", port, got)
		}
	}
}
