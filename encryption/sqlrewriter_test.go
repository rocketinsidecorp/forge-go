package encryption_test

import (
	"testing"

	"github.com/rocketinsidecorp/forge-go/encryption"
)

func TestSQLColumnRejectsUnsafeIdentifiers(t *testing.T) {
	t.Parallel()
	for _, spec := range []encryption.SQLColumn{
		{Table: "people; DROP TABLE people", Column: "ssn"},
		{Table: "people", Column: "ssn", IDColumn: "id OR 1=1"},
		{Table: "People", Column: "ssn"},
		{Table: "people", Column: "ssn", IndexColumn: "a-b"},
		{Table: "", Column: "ssn"},
	} {
		if _, err := spec.Validate(); err == nil {
			t.Errorf("%+v accepted", spec)
		}
	}
}

func TestSQLColumnQuotesIdentifiersPerDialect(t *testing.T) {
	t.Parallel()
	spec := encryption.SQLColumn{Table: "order", Column: "user", IDColumn: "group", IndexColumn: "select"}
	cases := []struct {
		name         string
		spec         encryption.SQLColumn
		first, after string
		replace      string
	}{
		{"mysql, mariadb and sqlite", spec,
			"SELECT `group`, `user` FROM `order` WHERE `user` IS NOT NULL ORDER BY `group` LIMIT 5",
			"SELECT `group`, `user` FROM `order` WHERE `group` > ? AND `user` IS NOT NULL ORDER BY `group` LIMIT 5",
			"UPDATE `order` SET `user` = ?, `select` = ? WHERE `group` = ? AND `user` = ?"},
		{"postgresql", encryption.SQLColumn{Table: "order", Column: "user", IDColumn: "group", IndexColumn: "select", DollarPlaceholders: true},
			`SELECT "group", "user" FROM "order" WHERE "user" IS NOT NULL ORDER BY "group" LIMIT 5`,
			`SELECT "group", "user" FROM "order" WHERE "group" > $1 AND "user" IS NOT NULL ORDER BY "group" LIMIT 5`,
			`UPDATE "order" SET "user" = $1, "select" = $2 WHERE "group" = $3 AND "user" = $4`},
	}
	for _, c := range cases {
		if got, err := c.spec.PageSQL(5, false); err != nil || got != c.first {
			t.Errorf("%s: first page %q, %v", c.name, got, err)
		}
		if got, err := c.spec.PageSQL(5, true); err != nil || got != c.after {
			t.Errorf("%s: next page %q, %v", c.name, got, err)
		}
		if got, err := c.spec.ReplaceSQL(true); err != nil || got != c.replace {
			t.Errorf("%s: replace %q, %v", c.name, got, err)
		}
	}
	// The default id column is quoted too.
	if got, err := (encryption.SQLColumn{Table: "people", Column: "ssn"}).PageSQL(1, false); err != nil || got != "SELECT `id`, `ssn` FROM `people` WHERE `ssn` IS NOT NULL ORDER BY `id` LIMIT 1" {
		t.Errorf("default id: %q, %v", got, err)
	}
}

// PageSQL and ReplaceSQL are exported: they validate on their own instead of
// relying on a prior Validate call.
func TestSQLColumnStatementsRefuseInvalidIdentifiers(t *testing.T) {
	t.Parallel()
	for _, spec := range []encryption.SQLColumn{
		{Table: "people; DROP TABLE people", Column: "ssn"},
		{Table: "people", Column: "ssn", IDColumn: "id OR 1=1"},
		{Table: "people", Column: "ssn\"; --", DollarPlaceholders: true},
		{Table: "people", Column: "ssn", IndexColumn: "a`b"},
		{Table: "", Column: "ssn"},
		{Table: "People", Column: "ssn"},
	} {
		if sql, err := spec.PageSQL(10, true); err == nil || sql != "" {
			t.Errorf("PageSQL(%+v) = %q, %v", spec, sql, err)
		}
		if sql, err := spec.ReplaceSQL(spec.IndexColumn != ""); err == nil || sql != "" {
			t.Errorf("ReplaceSQL(%+v) = %q, %v", spec, sql, err)
		}
	}
}
