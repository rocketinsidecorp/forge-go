package postgrestest

import (
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/migrate"
)

func TestUUIDv7UnsupportedNamesTheRequirementAndTheServer(t *testing.T) {
	t.Parallel()
	for version, name := range map[int]string{160011: "16.11", 170007: "17.7", 179999: "17.99"} {
		reason := uuidv7Unsupported(version, name)
		if !strings.Contains(reason, "requires PostgreSQL 18") || !strings.Contains(reason, "PostgreSQL "+name) {
			t.Errorf("%d: reason = %q", version, reason)
		}
	}
	for _, version := range []int{180000, 180003, 190000} {
		if reason := uuidv7Unsupported(version, "x"); reason != "" {
			t.Errorf("%d: unexpected reason %q", version, reason)
		}
	}
}

func up(sql string) migrate.Migration { return migrate.Migration{Up: migrate.Script{SQL: sql}} }

func TestNeedsUUIDv7(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		set  []migrate.Migration
		want bool
	}{
		"no migrations":      {nil, false},
		"no uuid":            {[]migrate.Migration{up("CREATE TABLE t (id uuid PRIMARY KEY)")}, false},
		"default":            {[]migrate.Migration{up("CREATE TABLE t (id uuid PRIMARY KEY DEFAULT uuidv7())")}, true},
		"upper case, spaced": {[]migrate.Migration{up("SELECT UUIDV7 ( )")}, true},
		"a look-alike":       {[]migrate.Migration{up("SELECT my_uuidv7_helper()")}, false},
		"polyfilled":         {[]migrate.Migration{up("CREATE OR REPLACE FUNCTION public.uuidv7() RETURNS uuid AS $$ SELECT gen_random_uuid() $$ LANGUAGE sql;"), up("CREATE TABLE t (id uuid DEFAULT uuidv7())")}, false},
	} {
		if got := needsUUIDv7(tc.set); got != tc.want {
			t.Errorf("%s: needsUUIDv7 = %v, want %v", name, got, tc.want)
		}
	}
}
