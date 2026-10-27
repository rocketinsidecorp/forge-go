package mysql_test

import (
	"context"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/auth/authtest"
	authmysql "github.com/rocketinsidecorp/forge-go/auth/mysql"
	"github.com/rocketinsidecorp/forge-go/mysql/mysqltest"
	"github.com/rocketinsidecorp/forge-go/sqldb"
)

// TestIdentityContract runs the identity store contract (auth/authtest),
// which auth/postgres and auth/mysql pass too.
func TestIdentityContract(t *testing.T) {
	t.Parallel()
	authtest.Run(t, func(t testing.TB) authtest.Harness {
		db := mysqltest.NewMigrated(t, authmysql.Migrations())
		return Harness(t, db)
	})
}

// Harness is the contract's harness on db.
func Harness(t testing.TB, db *sqldb.DB) authtest.Harness {
	t.Helper()
	repository, err := authmysql.New(db)
	if err != nil {
		t.Fatal(err)
	}
	return authtest.Harness{
		Store: repository,
		WithIdleTimeout: func(idle time.Duration) authtest.Store {
			store, err := authmysql.New(db, authmysql.WithSessionIdleTimeout(idle))
			if err != nil {
				t.Fatal(err)
			}
			return store
		},
		NewThrottler: func(config auth.LoginThrottleConfig) authtest.Throttler {
			throttler, err := authmysql.NewLoginThrottler(db, config)
			if err != nil {
				t.Fatal(err)
			}
			return throttler
		},
		Provision: func(ctx context.Context, organization, slug, email, password string) (authtest.Owner, error) {
			owner, err := repository.ProvisionOwner(ctx, authmysql.OwnerAccount{Organization: organization, TenantSlug: slug, Email: email, Password: password})
			return authtest.Owner{TenantID: owner.TenantID, UserID: owner.UserID, MembershipID: owner.MembershipID, RoleID: owner.RoleID, Permissions: owner.Permissions}, err
		},
		ErrAlreadyProvisioned: authmysql.ErrAlreadyProvisioned,
		ErrOwnerEmailTaken:    authmysql.ErrOwnerEmailTaken,
		Exec: func(ctx context.Context, query string, args ...any) error {
			_, err := db.ExecContext(ctx, query, args...)
			return err
		},
		Count: func(ctx context.Context, query string, args ...any) (int64, error) {
			var n int64
			err := db.QueryRowContext(ctx, query, args...).Scan(&n)
			return n, err
		},
		Time:  func(t time.Time) any { return t.UTC() },
		Close: func() { _ = db.Close() },
	}
}
