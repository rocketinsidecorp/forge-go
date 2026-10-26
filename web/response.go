package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"

	"github.com/rocketinsidecorp/forge-go/fault"
)

// ErrorBody is the only error format Forge APIs return.
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail is the content of an ErrorBody: a stable code, a safe message and the request ID for
// correlation.
type ErrorDetail struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}

// JSON writes value as a JSON response. The value is encoded into a buffer
// first, so the status line and body are written only when encoding
// succeeded. A value that cannot be encoded (NaN, a channel, a failing
// MarshalJSON) is logged and answered with a generic 500 instead of a
// truncated body under the requested status.
func JSON(w http.ResponseWriter, status int, value any) {
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(value); err != nil {
		slog.Error("json response encoding failed", "status", status, "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, encodeFailureBody)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// The status line is already sent; a write failure can only mean the
	// client went away.
	_, _ = w.Write(body.Bytes())
}

// encodeFailureBody is the ErrorBody of a JSON response that could not be
// encoded. It is a constant because encoding an ErrorBody is what just failed
// for another value, and the request ID is not available here.
const encodeFailureBody = `{"error":{"code":"internal_error","message":"internal server error"}}` + "\n"

// Error writes err as an ErrorBody. Only *fault.Error code and message reach
// the client; anything else becomes a generic 500. Server-side failures are
// logged with their full cause chain and the request ID.
func Error(w http.ResponseWriter, r *http.Request, err error) {
	public := fault.From(err)
	if public == nil {
		public = fault.New("internal_error", "internal server error", fault.CategoryInternal, http.StatusInternalServerError)
	}
	status := public.Status()
	switch {
	case r.Context().Err() != nil && errors.Is(err, context.Canceled):
		// The client went away; nobody reads this response and it is not a
		// server failure worth an error-level entry.
		Logger(r.Context()).Debug("request canceled by client", "method", r.Method, "path", r.URL.Path)
	case status >= http.StatusInternalServerError:
		// *fault.Error prints only its public code and message, so the
		// underlying cause is logged explicitly.
		attrs := []any{"method", r.Method, "path", r.URL.Path, "status", status, "code", public.Code, "error", err}
		if public.Cause != nil {
			attrs = append(attrs, "cause", public.Cause)
		}
		Logger(r.Context()).Error("request failed", attrs...)
	}
	JSON(w, status, ErrorBody{Error: ErrorDetail{Code: public.Code, Message: public.Message, RequestID: RequestID(r.Context())}})
}

// DecodeJSON strictly decodes a single JSON object into dst: the content type
// must be JSON, unknown fields are rejected and trailing data is an error.
// Size limits are enforced by the server's body limit.
func DecodeJSON(r *http.Request, dst any) error {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return fault.New("unsupported_media_type", "request body must be application/json", fault.CategoryInvalid, http.StatusUnsupportedMediaType)
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return decodeError(err)
	}
	// Anything but end of input after the value is an error, including a
	// stray "}" or "]" that decoder.More would not report.
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return decodeError(err)
		}
		return invalidJSON("request body must contain a single JSON object", err)
	}
	return nil
}

func decodeError(err error) error {
	var tooLarge *http.MaxBytesError
	var syntax *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	switch {
	case errors.As(err, &tooLarge):
		return fault.New("body_too_large", "request body too large", fault.CategoryInvalid, http.StatusRequestEntityTooLarge).WithCause(err)
	case errors.Is(err, io.EOF):
		return invalidJSON("request body is empty", err)
	case errors.Is(err, io.ErrUnexpectedEOF):
		return invalidJSON("request body is not valid JSON", err)
	case errors.As(err, &syntax):
		return invalidJSON("request body is not valid JSON", err)
	case errors.As(err, &typeErr):
		return invalidJSON(fmt.Sprintf("field %q has the wrong type", typeErr.Field), err)
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		// The message only names a field the client sent.
		return invalidJSON(strings.TrimPrefix(err.Error(), "json: "), err)
	default:
		// Read failures (timeouts, resets) carry addresses and are not the
		// client's JSON at fault: keep the detail in the cause only.
		return fault.New("unreadable_body", "the request body could not be read", fault.CategoryInvalid, http.StatusBadRequest).WithCause(err)
	}
}

func invalidJSON(message string, cause error) error {
	return fault.New("invalid_json", message, fault.CategoryInvalid, http.StatusBadRequest).WithCause(cause)
}
