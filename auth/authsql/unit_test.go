package authsql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/sqldb"
)

// noConnector is a database/sql connector that never connects: New
// validates its arguments without touching the database, so these unit
// tests need no driver (the store itself is tested against SQLite and MySQL
// in their adapters and in sqlite/internal/integration).
type noConnector struct{}

func (noConnector) Connect(context.Context) (driver.Conn, error) {
	return nil, errors.New("no database in a unit test")
}

func (noConnector) Driver() driver.Driver { return noDriver{} }

type noDriver struct{}

func (noDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("no database in a unit test")
}

func TestTimestampScan(t *testing.T) {
	t.Parallel()
	var stamp timestamp
	at := time.Date(2026, 10, 27, 9, 30, 0, 123456000, time.FixedZone("x", 3600))
	if err := stamp.Scan(at); err != nil || !stamp.Time.Equal(at) || stamp.Time.Location() != time.UTC {
		t.Fatalf("time.Time: %v %v", stamp.Time, err)
	}
	if err := stamp.Scan([]byte("2026-10-27 08:30:00.123456")); err != nil || !stamp.Time.Equal(at) {
		t.Fatalf("[]byte: %v %v", stamp.Time, err)
	}
	if err := stamp.Scan("2026-10-27 08:30:00.123456"); err != nil || !stamp.Time.Equal(at) {
		t.Fatalf("string: %v %v", stamp.Time, err)
	}
	for _, bad := range []any{int64(1), nil, "yesterday"} {
		if err := stamp.Scan(bad); err == nil {
			t.Fatalf("%v scanned", bad)
		}
	}
}

func TestNewValidatesItsArguments(t *testing.T) {
	t.Parallel()
	raw := sql.OpenDB(noConnector{})
	db, err := sqldb.New(raw, config.Default().Database, func(error) sqldb.Classification { return sqldb.Classification{} })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := New(db, &Dialect{Name: "x"}); err == nil {
		t.Fatal("an incomplete dialect was accepted")
	}
}

func TestOptions(t *testing.T) {
	t.Parallel()
	r := &Repository{}
	if err := withClock(nil)(r); err == nil {
		t.Fatal("nil clock accepted")
	}
	now := func() time.Time { return time.Unix(0, 0) }
	if err := withClock(now)(r); err != nil || r.now().Unix() != 0 {
		t.Fatal("clock not set")
	}
	for _, bad := range []time.Duration{time.Minute, 31 * 24 * time.Hour} {
		if err := WithSessionIdleTimeout(bad)(r); err == nil {
			t.Fatalf("idle timeout %v accepted", bad)
		}
	}
	if _, err := New(nil, nil); err == nil {
		t.Fatal("New(nil) succeeded")
	}
	if (*Dialect)(nil).validate() == nil {
		t.Fatal("a nil dialect is valid")
	}
	if _, err := authDigest([]byte("short")); err == nil {
		t.Fatal("a short digest was accepted")
	}
	if nullableString("x") != "x" || nullableString("") != nil {
		t.Fatal("nullableString")
	}
}
