package web_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/fault"
	"github.com/rocketinsidecorp/forge-go/web"
)

func TestErrorExposesOnlyPublicFaultFields(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	ctx := web.WithRequestID(context.Background(), "req-1")
	ctx = web.WithLogger(ctx, slog.New(slog.NewJSONHandler(&logs, nil)))

	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
		wantLogged bool
	}{
		{name: "fault", err: fault.New("task_not_found", "task not found", fault.CategoryNotFound, 0).WithCause(errors.New("sql: secret table detail")), wantStatus: http.StatusNotFound, wantCode: "task_not_found"},
		{name: "wrapped fault", err: errors.Join(errors.New("context"), fault.New("stale", "stale version", fault.CategoryConflict, 0)), wantStatus: http.StatusConflict, wantCode: "stale"},
		{name: "unknown error", err: errors.New("dial tcp 10.0.0.5: password=hunter2"), wantStatus: http.StatusInternalServerError, wantCode: "internal_error", wantLogged: true},
	}
	for _, test := range tests {
		logs.Reset()
		recorder := httptest.NewRecorder()
		web.Error(recorder, httptest.NewRequest(http.MethodGet, "/x", nil).WithContext(ctx), test.err)
		if recorder.Code != test.wantStatus {
			t.Fatalf("%s: status=%d want=%d", test.name, recorder.Code, test.wantStatus)
		}
		var body web.ErrorBody
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Error.Code != test.wantCode || body.Error.RequestID != "req-1" {
			t.Fatalf("%s: body=%+v", test.name, body)
		}
		if strings.Contains(recorder.Body.String(), "secret") || strings.Contains(recorder.Body.String(), "hunter2") {
			t.Fatalf("%s: internal detail leaked: %s", test.name, recorder.Body.String())
		}
		if logged := strings.Contains(logs.String(), "request failed"); logged != test.wantLogged {
			t.Fatalf("%s: logged=%v want=%v", test.name, logged, test.wantLogged)
		}
	}
}

func TestDecodeJSON(t *testing.T) {
	t.Parallel()
	type payload struct {
		Title string `json:"title"`
		Count int    `json:"count"`
	}
	tests := []struct {
		name        string
		contentType string
		body        string
		wantStatus  int
	}{
		{name: "valid", contentType: "application/json; charset=utf-8", body: `{"title":"a","count":1}`},
		{name: "wrong media type", contentType: "text/plain", body: `{}`, wantStatus: http.StatusUnsupportedMediaType},
		{name: "unknown field", contentType: "application/json", body: `{"title":"a","admin":true}`, wantStatus: http.StatusBadRequest},
		{name: "wrong type", contentType: "application/json", body: `{"count":"x"}`, wantStatus: http.StatusBadRequest},
		{name: "syntax", contentType: "application/json", body: `{"title":`, wantStatus: http.StatusBadRequest},
		{name: "empty", contentType: "application/json", body: ``, wantStatus: http.StatusBadRequest},
		{name: "trailing object", contentType: "application/json", body: `{"title":"a"}{"title":"b"}`, wantStatus: http.StatusBadRequest},
	}
	for _, test := range tests {
		request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(test.body))
		request.Header.Set("Content-Type", test.contentType)
		var dst payload
		err := web.DecodeJSON(request, &dst)
		if test.wantStatus == 0 {
			if err != nil {
				t.Fatalf("%s: %v", test.name, err)
			}
			continue
		}
		public := fault.From(err)
		if public == nil || public.Status() != test.wantStatus {
			t.Fatalf("%s: err=%v want status %d", test.name, err, test.wantStatus)
		}
	}
}

func TestDecodeJSONBodyTooLarge(t *testing.T) {
	t.Parallel()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"title":"`+strings.Repeat("a", 100)+`"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Body = http.MaxBytesReader(recorder, request.Body, 16)
	var dst map[string]string
	if public := fault.From(web.DecodeJSON(request, &dst)); public == nil || public.Status() != http.StatusRequestEntityTooLarge {
		t.Fatalf("want 413, got %+v", public)
	}
}

func TestLoggerNeverNil(t *testing.T) {
	t.Parallel()
	web.Logger(context.Background()).Info("safe without a configured logger")
}

func TestDecodeJSONRejectsTrailingDelimiters(t *testing.T) {
	t.Parallel()
	for _, body := range []string{`{"title":"a"}}`, `{"title":"a"}]`, `{"title":"a"} x`, `{"title":"a"}{}`} {
		request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		var dst map[string]string
		if public := fault.From(web.DecodeJSON(request, &dst)); public == nil || public.Code != "invalid_json" {
			t.Errorf("%q accepted or misclassified: %+v", body, public)
		}
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) {
	return 0, errors.New("read tcp 10.1.2.3:8080->203.0.113.9:55555: i/o timeout")
}

func TestDecodeJSONHidesReadErrors(t *testing.T) {
	t.Parallel()
	request := httptest.NewRequest(http.MethodPost, "/", failingReader{})
	request.Header.Set("Content-Type", "application/json")
	var dst map[string]string
	public := fault.From(web.DecodeJSON(request, &dst))
	if public == nil || public.Code != "unreadable_body" || strings.Contains(public.Message, "10.1.2.3") {
		t.Fatalf("read error leaked or misclassified: %+v", public)
	}
}

func TestErrorLogsTheCauseOfServerFailures(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	ctx := web.WithLogger(context.Background(), slog.New(slog.NewJSONHandler(&logs, nil)))
	failure := fault.New("internal_error", "internal server error", fault.CategoryInternal, 0).WithCause(errors.New(`relation "tasks" does not exist`))
	web.Error(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx), failure)
	if !strings.Contains(logs.String(), `relation \"tasks\" does not exist`) {
		t.Fatalf("root cause missing from logs: %s", logs.String())
	}
}

func TestErrorDoesNotLogClientDisconnects(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	ctx, cancel := context.WithCancel(web.WithLogger(context.Background(), slog.New(slog.NewJSONHandler(&logs, nil))))
	cancel()
	failure := fault.New("canceled", "the operation was canceled", fault.CategoryUnavailable, 0).WithCause(context.Canceled)
	web.Error(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx), failure)
	if strings.Contains(logs.String(), `"level":"ERROR"`) {
		t.Fatalf("client disconnect logged as an error: %s", logs.String())
	}
}

type failingJSON struct{}

func (failingJSON) MarshalJSON() ([]byte, error) { return nil, errors.New("boom: secret detail") }

func TestJSONEncodingFailureSends500WithoutPartialBody(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	for name, value := range map[string]any{
		"marshal error": failingJSON{},
		"nan":           map[string]any{"ok": "x", "n": math.NaN()},
		"channel":       make(chan int),
	} {
		logs.Reset()
		recorder := httptest.NewRecorder()
		web.JSON(recorder, http.StatusCreated, value)
		if recorder.Code != http.StatusInternalServerError {
			t.Fatalf("%s: status=%d want 500", name, recorder.Code)
		}
		var body web.ErrorBody
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || body.Error.Code != "internal_error" {
			t.Fatalf("%s: body=%q err=%v", name, recorder.Body.String(), err)
		}
		if strings.Contains(recorder.Body.String(), "secret") || strings.Contains(recorder.Body.String(), `"ok"`) {
			t.Fatalf("%s: partial or internal content leaked: %s", name, recorder.Body.String())
		}
		if !strings.Contains(logs.String(), "json response encoding failed") {
			t.Fatalf("%s: encoding failure not logged: %s", name, logs.String())
		}
	}
}

func TestJSONSuccessKeepsStatusAndBody(t *testing.T) {
	t.Parallel()
	recorder := httptest.NewRecorder()
	web.JSON(recorder, http.StatusCreated, map[string]string{"a": "b"})
	if recorder.Code != http.StatusCreated || recorder.Body.String() != "{\"a\":\"b\"}\n" || recorder.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("code=%d body=%q header=%v", recorder.Code, recorder.Body.String(), recorder.Header())
	}
}
