package sqldb

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"time"

	"github.com/rocketinsidecorp/forge-go/telemetry"
)

// MetricDBOperationDuration is the histogram of database statement durations.
const MetricDBOperationDuration = "db.client.operation.duration"

// String returns the snake_case name of k, used as the error.type of
// database telemetry. KindUnknown is "other".
func (k Kind) String() string {
	switch k {
	case KindNotFound:
		return "not_found"
	case KindUniqueViolation:
		return "unique_violation"
	case KindForeignKeyViolation:
		return "foreign_key_violation"
	case KindExclusionViolation:
		return "exclusion_violation"
	case KindInvalidData:
		return "invalid_data"
	case KindConflict:
		return "conflict"
	case KindTimeout:
		return "timeout"
	case KindUnavailable:
		return "unavailable"
	}
	return "other"
}

// Observer traces and measures database statements for one database system.
// It records the statement's operation (SELECT, INSERT...), never its text or
// arguments. Every statement is measured in db.client.operation.duration
// (labels: db.system.name, db.operation.name, error.type); a span is created
// only when the statement runs inside a traced operation, so pollers and
// startup queries do not create root spans. A nil *Observer does nothing.
type Observer struct {
	telemetry *telemetry.Telemetry
	tracer    *telemetry.Tracer
	duration  *telemetry.Histogram
	system    string
	classify  Classifier
}

// NewObserver returns an observer for system (the OpenTelemetry
// db.system.name: "postgresql", "mysql", "sqlite"), or nil when t is nil.
func NewObserver(t *telemetry.Telemetry, system string, classify Classifier) (*Observer, error) {
	if t == nil {
		return nil, nil
	}
	if system == "" || classify == nil {
		return nil, errors.New("sqldb: database system and classifier are required")
	}
	duration, err := t.Metrics().Histogram(MetricDBOperationDuration, "Duration of database client operations.", "s", telemetry.DurationBuckets,
		"db.system.name", "db.operation.name", "error.type")
	if err != nil {
		return nil, err
	}
	return &Observer{telemetry: t, tracer: t.Tracer(telemetry.ScopeName + "/sqldb"), duration: duration, system: system, classify: classify}, nil
}

// DefaultPoolName is the pool name reported when the application gives none.
const DefaultPoolName = "default"

// ObservePool exports db's connection statistics (open, in use, idle, limit,
// waits and time waited) under the pool name, DefaultPoolName when empty:
// db.client.connection.count, db.client.connection.max, forge.db.pool.waits
// and forge.db.pool.wait.duration. Registering a name again replaces the
// pool, so reopening a database in a process does not duplicate series. A nil
// Observer does nothing.
func (o *Observer) ObservePool(name string, db *sql.DB) error {
	if o == nil {
		return nil
	}
	if name == "" {
		name = DefaultPoolName
	}
	return o.telemetry.ObservePool(o.system, name, func() telemetry.PoolStats {
		stats := db.Stats()
		return telemetry.PoolStats{
			InUse:    stats.InUse,
			Idle:     stats.Idle,
			Max:      stats.MaxOpenConnections,
			Waits:    stats.WaitCount,
			WaitTime: stats.WaitDuration,
		}
	})
}

// Observation is one statement in progress; End it exactly once.
type Observation struct {
	observer  *Observer
	ctx       context.Context
	operation string
	start     time.Time
}

// Start begins observing query. It does not allocate.
func (o *Observer) Start(ctx context.Context, query string) Observation {
	if o == nil {
		return Observation{}
	}
	return Observation{observer: o, ctx: ctx, operation: Operation(query), start: time.Now()}
}

// End records the statement's outcome. driver.ErrSkip, which only tells
// database/sql to take another path, is not recorded.
func (obs Observation) End(err error) {
	o := obs.observer
	if o == nil || errors.Is(err, driver.ErrSkip) {
		return
	}
	errorType := o.errorType(err)
	o.duration.Record(time.Since(obs.start).Seconds(), o.system, obs.operation, errorType)
	if !telemetry.SpanContextFromContext(obs.ctx).IsValid() {
		return
	}
	_, span := o.tracer.StartAt(obs.ctx, obs.start, obs.operation, telemetry.SpanKindClient)
	if span.IsRecording() {
		span.SetAttributes(telemetry.String("db.system.name", o.system), telemetry.String("db.operation.name", obs.operation))
		if errorType != "" {
			span.SetAttributes(telemetry.String("error.type", errorType))
			span.SetStatus(telemetry.StatusError, errorType)
		}
	}
	span.End()
}

func (o *Observer) errorType(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	return o.classify(err).Kind.String()
}

// operations are the statement verbs reported as db.operation.name; any
// other first keyword is reported as OTHER, so statement text never becomes
// a label.
var operations = []string{
	"SELECT", "INSERT", "UPDATE", "DELETE", "WITH", "MERGE", "REPLACE", "UPSERT",
	"BEGIN", "START", "COMMIT", "ROLLBACK", "SAVEPOINT", "RELEASE",
	"CREATE", "ALTER", "DROP", "TRUNCATE", "GRANT", "REVOKE",
	"SET", "SHOW", "CALL", "DO", "EXPLAIN", "VALUES", "PRAGMA", "LOCK",
	"COPY", "LISTEN", "NOTIFY", "UNLISTEN", "ANALYZE", "VACUUM", "REFRESH",
}

// Operation returns the uppercase first keyword of query, such as "SELECT",
// skipping whitespace, comments and opening parentheses, or "OTHER" when it
// is not a known statement verb. It never returns query text and does not
// allocate.
func Operation(query string) string {
	i := 0
	for i < len(query) {
		switch c := query[i]; {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '(':
			i++
		case strings.HasPrefix(query[i:], "--"):
			end := strings.IndexByte(query[i:], '\n')
			if end < 0 {
				return "OTHER"
			}
			i += end + 1
		case strings.HasPrefix(query[i:], "/*"):
			end := strings.Index(query[i+2:], "*/")
			if end < 0 {
				return "OTHER"
			}
			i += end + 4
		default:
			j := i
			for j < len(query) && j-i < 16 && isLetter(query[j]) {
				j++
			}
			word := query[i:j]
			for _, operation := range operations {
				if strings.EqualFold(word, operation) {
					return operation
				}
			}
			return "OTHER"
		}
	}
	return "OTHER"
}

func isLetter(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }
