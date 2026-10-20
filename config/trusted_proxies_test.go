package config_test

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/config"
)

func loadTrustedProxies(value string, set bool) (config.Config, error) {
	return config.LoadWithLookup(func(key string) (string, bool) {
		if key == "FORGE_HTTP_TRUSTED_PROXIES" {
			return value, set
		}
		return "", false
	})
}

func TestTrustedProxiesDefaultToNobody(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		value string
		set   bool
	}{{"", false}, {"", true}, {"  ", true}} {
		cfg, err := loadTrustedProxies(tc.value, tc.set)
		if err != nil {
			t.Fatal(err)
		}
		if len(cfg.HTTP.TrustedProxies) != 0 {
			t.Fatalf("%q trusts %v, want nobody", tc.value, cfg.HTTP.TrustedProxies)
		}
	}
}

func TestTrustedProxiesParse(t *testing.T) {
	t.Parallel()
	cfg, err := loadTrustedProxies(" 172.18.5.9/16, fd00:1::/64,10.0.0.7 ,2001:db8::1", true)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"172.18.0.0/16", "fd00:1::/64", "10.0.0.7/32", "2001:db8::1/128"}
	if len(cfg.HTTP.TrustedProxies) != len(want) {
		t.Fatalf("prefixes = %v, want %v", cfg.HTTP.TrustedProxies, want)
	}
	for i, prefix := range cfg.HTTP.TrustedProxies {
		if prefix.String() != want[i] {
			t.Fatalf("prefix %d = %s, want %s", i, prefix, want[i])
		}
	}
}

func TestTrustedProxiesAcceptTheMinimumPrefixLengths(t *testing.T) {
	t.Parallel()
	cfg, err := loadTrustedProxies("10.0.0.0/8, fc00::/16", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.HTTP.TrustedProxies) != 2 {
		t.Fatalf("prefixes = %v, want 2", cfg.HTTP.TrustedProxies)
	}
}

func TestTrustedProxiesRejectInvalidEntries(t *testing.T) {
	t.Parallel()
	for _, value := range []string{
		"172.18.0.0/33",
		"not-a-network",
		"172.18.0.0/16,",
		"172.18.0.0/16,,10.0.0.0/8",
		"0.0.0.0/0",
		"::/0",
		"0.0.0.0/1,128.0.0.0/1",
		"10.0.0.0/7",
		"::/1",
		"fc00::/15",
		"10.0.0.0/8, 0.0.0.0/0",
		"::ffff:10.0.0.0/104",
		"::ffff:10.0.0.1",
		"fe80::1%eth0",
		"fe80::%eth0/64",
		"172.18.0.0/16 10.0.0.0/8",
		"*",
	} {
		_, err := loadTrustedProxies(value, true)
		if err == nil {
			t.Errorf("FORGE_HTTP_TRUSTED_PROXIES=%q accepted", value)
			continue
		}
		if !strings.Contains(err.Error(), "FORGE_HTTP_TRUSTED_PROXIES") {
			t.Errorf("%q: error %q does not name the variable", value, err)
		}
		if strings.Contains(err.Error(), value) {
			t.Errorf("%q: error %q echoes the value", value, err)
		}
	}
}

func TestValidateRejectsUnsafeTrustedProxies(t *testing.T) {
	t.Parallel()
	for _, prefix := range []netip.Prefix{{}, netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("::/0"), netip.MustParsePrefix("0.0.0.0/1"), netip.MustParsePrefix("10.0.0.0/7"), netip.MustParsePrefix("::/1"), netip.MustParsePrefix("fc00::/15"), netip.MustParsePrefix("::ffff:10.0.0.0/104")} {
		cfg := config.Default()
		cfg.HTTP.TrustedProxies = []netip.Prefix{prefix}
		if err := cfg.Validate(); err == nil {
			t.Errorf("Validate accepted trusted proxy %v", prefix)
		}
	}
}
