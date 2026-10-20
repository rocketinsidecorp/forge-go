package mysql_test

import (
	"context"
	"log/slog"
	"testing"

	"github.com/rocketinsidecorp/forge-go/migrate"
	"github.com/rocketinsidecorp/forge-go/migrate/migratetest"
	"github.com/rocketinsidecorp/forge-go/mysql"
	"github.com/rocketinsidecorp/forge-go/mysql/mysqltest"
)

func TestFrameworkMigrationOwnership(t *testing.T) {
	t.Parallel()
	migratetest.RunFrameworkOwnership(t, func(t testing.TB) migratetest.Database {
		db, cfg := mysqltest.NewWithConfig(t)
		return migratetest.Database{
			NewMigrator: func(migrations []migrate.Migration, logger *slog.Logger) (*migrate.Migrator, error) {
				return mysql.NewMigrator(cfg, migrations, logger)
			},
			Exec: func(ctx context.Context, query string) error {
				_, err := db.ExecContext(ctx, query)
				return err
			},
			TableExists: func(ctx context.Context, table string) (bool, error) {
				var count int
				err := db.QueryRowContext(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ?", table).Scan(&count)
				return count == 1, err
			},
		}
	})
}
