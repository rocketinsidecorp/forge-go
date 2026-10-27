package postgres_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/auth"
	"github.com/rocketinsidecorp/forge-go/auth/authtest"
	authpostgres "github.com/rocketinsidecorp/forge-go/auth/postgres"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"
)

// numbered rewrites the suite's ? placeholders to PostgreSQL's $n.
func numbered(query string) string {
	var out strings.Builder
	n := 0
	for _, r := range query {
		if r == '?' {
			n++
			out.WriteString("$" + strconv.Itoa(n))
			continue
		}
		out.WriteRune(r)
	}
	return out.String()
}

// TestIdentityContract runs the identity store contract (auth/authtest),
// which auth/sqlite and auth/mysql pass too.
func TestIdentityContract(t *testing.T) {
	t.Parallel()
	authtest.Run(t, func(t testing.TB) authtest.Harness {
		db := postgrestest.NewMigrated(t, authpostgres.Migrations())
		repository, err := authpostgres.New(db)
		if err != nil {
			t.Fatal(err)
		}
		return authtest.Harness{
			Store: repository,
			WithIdleTimeout: func(idle time.Duration) authtest.Store {
				store, err := authpostgres.New(db, authpostgres.WithSessionIdleTimeout(idle))
				if err != nil {
					t.Fatal(err)
				}
				return store
			},
			NewThrottler: func(config auth.LoginThrottleConfig) authtest.Throttler {
				throttler, err := authpostgres.NewLoginThrottler(db, config)
				if err != nil {
					t.Fatal(err)
				}
				return throttler
			},
			Provision: func(ctx context.Context, organization, slug, email, password string) (authtest.Owner, error) {
				owner, err := repository.ProvisionOwner(ctx, authpostgres.OwnerAccount{Organization: organization, TenantSlug: slug, Email: email, Password: password})
				return authtest.Owner{TenantID: owner.TenantID, UserID: owner.UserID, MembershipID: owner.MembershipID, RoleID: owner.RoleID, Permissions: owner.Permissions}, err
			},
			ErrAlreadyProvisioned: authpostgres.ErrAlreadyProvisioned,
			ErrOwnerEmailTaken:    authpostgres.ErrOwnerEmailTaken,
			Exec: func(ctx context.Context, query string, args ...any) error {
				_, err := db.Exec(ctx, numbered(query), args...)
				return err
			},
			Count: func(ctx context.Context, query string, args ...any) (int64, error) {
				var n int64
				err := db.QueryRow(ctx, numbered(query), args...).Scan(&n)
				return n, err
			},
			Time:  func(t time.Time) any { return t.UTC() },
			Close: db.Close,
		}
	})
}
