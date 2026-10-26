package postgres_test

import (
	"context"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/rocketinsidecorp/forge-go/auth"
	authpostgres "github.com/rocketinsidecorp/forge-go/auth/postgres"
	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/migrate"
	"github.com/rocketinsidecorp/forge-go/postgres"
	"github.com/rocketinsidecorp/forge-go/postgres/postgrestest"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

const sessionIdleMigration = "20261026090000_session_idle_timeout"

func TestSessionIdleTimeoutOptionIsValidated(t *testing.T) {
	t.Parallel()
	if config.Default().Auth.SessionIdleTimeout != authpostgres.DefaultSessionIdleTimeout {
		t.Fatalf("config default %s and repository default %s differ", config.Default().Auth.SessionIdleTimeout, authpostgres.DefaultSessionIdleTimeout)
	}
	for timeout, valid := range map[time.Duration]bool{
		0: false, -time.Hour: false, 4*time.Minute + 59*time.Second: false, 30*24*time.Hour + time.Second: false,
		5 * time.Minute: true, 2 * time.Hour: true, 30 * 24 * time.Hour: true,
	} {
		repository := new(authpostgres.Repository)
		err := authpostgres.WithSessionIdleTimeout(timeout)(repository)
		if (err == nil) != valid {
			t.Errorf("WithSessionIdleTimeout(%s): err=%v, want valid=%v", timeout, err, valid)
		}
		if valid && repository.SessionIdleTimeout() != timeout {
			t.Errorf("WithSessionIdleTimeout(%s) set %s", timeout, repository.SessionIdleTimeout())
		}
	}
	if _, err := authpostgres.New(nil, authpostgres.WithSessionIdleTimeout(time.Hour)); err == nil {
		t.Fatal("New without a database succeeded")
	}
}

// idleFixture is a migrated database with one active membership.
func idleFixture(t *testing.T, options ...authpostgres.Option) (*postgres.DB, *authpostgres.Repository, uuid.UUID) {
	t.Helper()
	db := postgrestest.NewMigrated(t, authpostgres.Migrations())
	repo, err := authpostgres.New(db, options...)
	if err != nil {
		t.Fatal(err)
	}
	userID, orgID, tenantID, membershipID := uuid.MustNew(), uuid.MustNew(), uuid.MustNew(), uuid.MustNew()
	insertIdentityFixture(t, db, userID, orgID, tenantID, membershipID, "idle@example.com", "idle@example.com", "Idle", "idle")
	return db, repo, membershipID
}

// lastSeen moves the session's last use to ago before the database clock
// (ago 0 leaves it alone) and returns the stored value.
func lastSeen(t *testing.T, db *postgres.DB, token auth.Token, ago time.Duration) time.Time {
	t.Helper()
	digest := token.Digest()
	ctx := context.Background()
	if ago != 0 {
		if _, err := db.Exec(ctx, `UPDATE forge_sessions SET last_seen_at = now() - $2::bigint * interval '1 microsecond' WHERE token_digest = $1`, digest[:], ago.Microseconds()); err != nil {
			t.Fatal(err)
		}
	}
	var seen time.Time
	if err := db.QueryRow(ctx, `SELECT last_seen_at FROM forge_sessions WHERE token_digest = $1`, digest[:]).Scan(&seen); err != nil {
		t.Fatal(err)
	}
	return seen
}

// TestSessionsEndAfterTheIdleTimeout: a session unused for the idle timeout
// no longer resolves (and is not revived by the attempt), while one used
// within it resolves and records the use, before its absolute expiry.
func TestSessionsEndAfterTheIdleTimeout(t *testing.T) {
	t.Parallel()
	db, repo, membershipID := idleFixture(t, authpostgres.WithSessionIdleTimeout(30*time.Minute))
	ctx := context.Background()
	newSession := func() auth.Token {
		t.Helper()
		token, err := repo.CreateSession(ctx, membershipID, time.Now().Add(24*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		return token
	}

	fresh := newSession()
	if _, found, err := repo.ResolveSession(ctx, fresh.Digest()); err != nil || !found {
		t.Fatalf("a new session: found=%v err=%v", found, err)
	}

	idle := newSession()
	before := lastSeen(t, db, idle, 30*time.Minute+time.Second)
	for range 2 {
		if _, found, err := repo.ResolveSession(ctx, idle.Digest()); err != nil || found {
			t.Fatalf("a session idle past the timeout: found=%v err=%v", found, err)
		}
	}
	if after := lastSeen(t, db, idle, 0); !after.Equal(before) {
		t.Fatalf("resolving an idle session recorded a use: %s -> %s", before, after)
	}

	// Through the middleware the idle session is the same 401 as a revoked one.
	middleware, err := auth.NewMiddleware(repo)
	if err != nil {
		t.Fatal(err)
	}
	if status := bearerStatus(t, middleware, idle); status != http.StatusUnauthorized {
		t.Fatalf("idle session through the middleware: status %d, want 401", status)
	}

	active := newSession()
	before = lastSeen(t, db, active, 29*time.Minute)
	if _, found, err := repo.ResolveSession(ctx, active.Digest()); err != nil || !found {
		t.Fatalf("a session used within the timeout: found=%v err=%v", found, err)
	}
	if after := lastSeen(t, db, active, 0); !after.After(before.Add(28 * time.Minute)) {
		t.Fatalf("the use was not recorded: %s -> %s", before, after)
	}
	if status := bearerStatus(t, middleware, active); status != http.StatusOK {
		t.Fatalf("active session through the middleware: status %d, want 200", status)
	}

	// A revoked session is not found and its last use is left alone.
	if err := repo.RevokeSession(ctx, active); err != nil {
		t.Fatal(err)
	}
	before = lastSeen(t, db, active, 10*time.Minute)
	if _, found, err := repo.ResolveSession(ctx, active.Digest()); err != nil || found {
		t.Fatalf("revoked session: found=%v err=%v", found, err)
	}
	if after := lastSeen(t, db, active, 0); !after.Equal(before) {
		t.Fatalf("resolving a revoked session recorded a use: %s -> %s", before, after)
	}
}

func TestSessionsUseTheDefaultIdleTimeout(t *testing.T) {
	t.Parallel()
	db, repo, membershipID := idleFixture(t)
	ctx := context.Background()
	if repo.SessionIdleTimeout() != 2*time.Hour {
		t.Fatalf("default idle timeout %s", repo.SessionIdleTimeout())
	}
	token, err := repo.CreateSession(ctx, membershipID, time.Now().Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	lastSeen(t, db, token, 2*time.Hour-time.Minute)
	if _, found, err := repo.ResolveSession(ctx, token.Digest()); err != nil || !found {
		t.Fatalf("used 1h59m ago: found=%v err=%v", found, err)
	}
	lastSeen(t, db, token, 2*time.Hour+time.Second)
	if _, found, err := repo.ResolveSession(ctx, token.Digest()); err != nil || found {
		t.Fatalf("used 2h ago: found=%v err=%v", found, err)
	}
}

// TestSessionUseIsWrittenAtMostOncePerMinute: resolutions within a minute of
// the recorded use only read; a stale use is recorded once, even by many
// concurrent requests.
func TestSessionUseIsWrittenAtMostOncePerMinute(t *testing.T) {
	t.Parallel()
	db, repo, membershipID := idleFixture(t)
	ctx := context.Background()
	// Count every write to forge_sessions in this test's own database.
	for _, statement := range []string{
		`CREATE TABLE session_writes (n bigint NOT NULL)`,
		`CREATE FUNCTION count_session_write() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN INSERT INTO session_writes VALUES (1); RETURN NEW; END $$`,
		`CREATE TRIGGER count_session_writes AFTER UPDATE ON forge_sessions
		FOR EACH ROW EXECUTE FUNCTION count_session_write()`,
	} {
		if _, err := db.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	writes := func() int {
		t.Helper()
		var n int
		if err := db.QueryRow(ctx, `SELECT count(*) FROM session_writes`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	resolveConcurrently := func(token auth.Token) {
		t.Helper()
		var wg sync.WaitGroup
		for range 16 {
			wg.Go(func() {
				if _, found, err := repo.ResolveSession(ctx, token.Digest()); err != nil || !found {
					t.Errorf("ResolveSession: found=%v err=%v", found, err)
				}
			})
		}
		wg.Wait()
	}

	token, err := repo.CreateSession(ctx, membershipID, time.Now().Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	created := lastSeen(t, db, token, 0)
	resolveConcurrently(token)
	if n := writes(); n != 0 {
		t.Fatalf("resolving a session used just now wrote %d times", n)
	}
	if seen := lastSeen(t, db, token, 0); !seen.Equal(created) {
		t.Fatalf("last use moved without a write: %s -> %s", created, seen)
	}

	// Well inside the minute, so a slow run cannot cross it.
	lastSeen(t, db, token, 45*time.Second)
	writesBefore := writes()
	resolveConcurrently(token)
	if n := writes() - writesBefore; n != 0 {
		t.Fatalf("a use 45s old was rewritten %d times", n)
	}

	stale := lastSeen(t, db, token, 61*time.Second)
	writesBefore = writes()
	resolveConcurrently(token)
	if n := writes() - writesBefore; n != 1 {
		t.Fatalf("16 concurrent resolutions of a stale use wrote %d times, want 1", n)
	}
	if seen := lastSeen(t, db, token, 0); !seen.After(stale.Add(time.Minute)) {
		t.Fatalf("the stale use was not recorded: %s -> %s", stale, seen)
	}
	writesBefore = writes()
	resolveConcurrently(token)
	if n := writes() - writesBefore; n != 0 {
		t.Fatalf("resolving right after the recorded use wrote %d times", n)
	}
}

// TestSessionIdleMigrationKeepsExistingSessions upgrades a database that has
// a session older than the idle timeout: the migration counts it as used now,
// so the upgrade signs nobody out, and the down migration drops the column.
func TestSessionIdleMigrationKeepsExistingSessions(t *testing.T) {
	t.Parallel()
	db, cfg := postgrestest.NewWithConfig(t)
	ctx := context.Background()
	migrator := func(withIdle bool) *migrate.Migrator {
		t.Helper()
		files := fstest.MapFS{}
		err := fs.WalkDir(authpostgres.Migrations(), ".", func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() || (!withIdle && strings.HasPrefix(path, sessionIdleMigration)) {
				return err
			}
			content, err := fs.ReadFile(authpostgres.Migrations(), path)
			files[path] = &fstest.MapFile{Data: content}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		loaded, err := migrate.LoadMigrations(files)
		if err != nil {
			t.Fatal(err)
		}
		m, err := postgres.NewMigrator(cfg, loaded, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = m.Close() })
		return m
	}
	if _, err := migrator(false).Up(ctx); err != nil {
		t.Fatal(err)
	}
	userID, orgID, tenantID, membershipID := uuid.MustNew(), uuid.MustNew(), uuid.MustNew(), uuid.MustNew()
	insertIdentityFixture(t, db, userID, orgID, tenantID, membershipID, "old@example.com", "old@example.com", "Old", "old")
	token, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	digest := token.Digest()
	if _, err := db.Exec(ctx, `
		INSERT INTO forge_sessions (token_digest, membership_id, credential_version, expires_at, created_at)
		VALUES ($1, $2, 1, now() + interval '20 days', now() - interval '3 days')
	`, digest[:], membershipID); err != nil {
		t.Fatal(err)
	}

	full := migrator(true)
	applied, err := full.Up(ctx)
	if err != nil || len(applied) != 1 || applied[0].Version != 20261026090000 {
		t.Fatalf("applied %v, err %v", applied, err)
	}
	repo, err := authpostgres.New(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := repo.ResolveSession(ctx, digest); err != nil || !found {
		t.Fatalf("a session from before the upgrade: found=%v err=%v", found, err)
	}

	reverted, err := full.DownFramework(ctx, 1)
	if err != nil || len(reverted) != 1 || reverted[0].Version != 20261026090000 {
		t.Fatalf("reverted %v, err %v", reverted, err)
	}
	var columns int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_name = 'forge_sessions' AND column_name = 'last_seen_at'`).Scan(&columns); err != nil || columns != 0 {
		t.Fatalf("last_seen_at after the down migration: %d columns, err %v", columns, err)
	}
}

// bearerStatus is the status a bearer request with token gets through
// middleware.Authenticate.
func bearerStatus(t *testing.T, middleware *auth.Middleware, token auth.Token) int {
	t.Helper()
	handler := middleware.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	request := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	request.Header.Set("Authorization", "Bearer "+token.Reveal())
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder.Code
}
