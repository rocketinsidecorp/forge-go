package dbtasks

import (
	"context"
	"net/url"
	"testing"

	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/dbtest"
)

// TestMySQLCreateExistsDrop runs against FORGE_TEST_MYSQL_URL (MySQL or
// MariaDB) and is skipped without it.
func TestMySQLCreateExistsDrop(t *testing.T) {
	t.Parallel()
	server := dbtest.ServerURL(t, config.AdapterMySQL)
	parsed, err := url.Parse(server)
	if err != nil {
		t.Fatal("the MySQL test URL is not valid")
	}
	parsed.Path = "/" + dbtest.DatabaseName()
	target, err := NewTarget(t.TempDir(), lookupOf(map[string]string{"FORGE_DATABASE_URL": parsed.String()}))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), dbtest.SetupTimeout)
	defer cancel()
	t.Cleanup(func() { _, _ = target.Drop(context.WithoutCancel(ctx)) })

	if exists, err := target.Exists(ctx); err != nil || exists {
		t.Fatalf("Exists before create = %v, %v", exists, err)
	}
	if created, err := target.Create(ctx); err != nil || !created {
		t.Fatalf("Create = %v, %v", created, err)
	}
	if created, err := target.Create(ctx); err != nil || created {
		t.Fatalf("second Create = %v, %v", created, err)
	}
	if exists, err := target.Exists(ctx); err != nil || !exists {
		t.Fatalf("Exists after create = %v, %v", exists, err)
	}
	if dropped, err := target.Drop(ctx); err != nil || !dropped {
		t.Fatalf("Drop = %v, %v", dropped, err)
	}
	if dropped, err := target.Drop(ctx); err != nil || dropped {
		t.Fatalf("second Drop = %v, %v", dropped, err)
	}
}

func TestQuoteMySQL(t *testing.T) {
	t.Parallel()
	if got := quoteMySQL("a`b"); got != "`a``b`" {
		t.Fatalf("quoteMySQL = %s", got)
	}
}
