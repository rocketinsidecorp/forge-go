package realtime

import (
	"net/http"

	"github.com/fabriciobonjorno/forge-go/fault"
)

// Errors returned by the broker. They are *fault.Error values, so web.Error
// renders them with a stable code and without internal detail.
var (
	// ErrSubscriptionDenied covers an undeclared topic, a missing permission
	// and a rejected Channel.Authorize hook alike, so a caller cannot probe
	// which channels exist.
	ErrSubscriptionDenied = fault.New("realtime_subscription_denied", "subscription denied", fault.CategoryForbidden, http.StatusForbidden)
	ErrUnauthenticated    = fault.New("realtime_unauthenticated", "authentication is required", fault.CategoryUnauthorized, http.StatusUnauthorized)
	ErrTopicInvalid       = fault.New("realtime_topic_invalid", "topic name is invalid", fault.CategoryInvalid, http.StatusBadRequest)
	ErrEventTypeInvalid   = fault.New("realtime_event_type_invalid", "event type is invalid", fault.CategoryInvalid, http.StatusBadRequest)
	ErrPayloadTooLarge    = fault.New("realtime_payload_too_large", "event payload is too large", fault.CategoryInvalid, http.StatusRequestEntityTooLarge)
	ErrPayloadInvalid     = fault.New("realtime_payload_invalid", "event payload is not valid JSON", fault.CategoryInvalid, http.StatusBadRequest)
	ErrLimitReached       = fault.New("realtime_limit_reached", "realtime connection or subscription limit reached", fault.CategoryRateLimited, http.StatusTooManyRequests)
	ErrClosed             = fault.New("realtime_closed", "realtime is shutting down", fault.CategoryUnavailable, http.StatusServiceUnavailable)

	// ErrSlowConsumer is the reason (Conn.Err) a connection was closed
	// because its queue filled up.
	ErrSlowConsumer = fault.New("realtime_slow_consumer", "connection closed: consumer too slow", fault.CategoryUnavailable, http.StatusServiceUnavailable)
	// ErrSessionExpired is the reason a connection was closed after its
	// maximum lifetime or a failed revalidation. Clients reconnect and
	// authenticate again.
	ErrSessionExpired = fault.New("realtime_session_expired", "connection closed: re-authentication required", fault.CategoryUnauthorized, http.StatusUnauthorized)
	// ErrTransport wraps a failure to hand an event to other replicas. The
	// event was still delivered to local subscribers.
	ErrTransport = fault.New("realtime_transport_failed", "event was not forwarded to other replicas", fault.CategoryUnavailable, http.StatusServiceUnavailable)
)
