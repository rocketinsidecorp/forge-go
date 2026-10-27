package sqlidentity

import (
	"database/sql"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/sqldb"

	_ "modernc.org/sqlite"
)

func TestTimestampScan(t *testing.T) {
	t.Parallel()
	var stamp Timestamp
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
	raw, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
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
	if err := WithClock(nil)(r); err == nil {
		t.Fatal("nil clock accepted")
	}
	now := func() time.Time { return time.Unix(0, 0) }
	if err := WithClock(now)(r); err != nil || r.now().Unix() != 0 {
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
