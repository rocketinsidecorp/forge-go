package storage_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/fault"
	"github.com/rocketinsidecorp/forge-go/storage"
)

// rawUpload sends body, a hand-written multipart message, to Receive.
func rawUpload(t *testing.T, store storage.Storage, opts storage.UploadOptions, body string) (storage.Received, error) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/upload", strings.NewReader(body))
	r.Header.Set("Content-Type", "multipart/form-data; boundary=XX")
	return storage.Receive(httptest.NewRecorder(), r, store, opts)
}

func faultCode(err error) string {
	if f := fault.From(err); f != nil {
		return f.Code
	}
	return ""
}

func TestReceiveRefusesRequestsThatAnnounceTooMuchBeforeReadingThem(t *testing.T) {
	store := newDisk(t)
	body, contentType := multipartBody(t, []part{{field: "avatar", filename: "a.png", contentType: "image/png", content: pngBytes}})
	r := httptest.NewRequest(http.MethodPost, "/upload", body)
	r.Header.Set("Content-Type", contentType)
	r.ContentLength = 1 << 40
	if _, err := storage.Receive(httptest.NewRecorder(), r, store, defaultOpts()); !errors.Is(err, storage.ErrTooLarge) {
		t.Errorf("a body announcing a terabyte: %v", err)
	}
	assertNoBlobs(t, store)
}

func TestReceiveRefusesMalformedMultipartBodies(t *testing.T) {
	const png = "image/png"
	nameless := "--XX\r\nContent-Disposition: form-data; name=\"\"\r\n\r\nvalue\r\n--XX--\r\n"
	truncatedField := "--XX\r\nContent-Disposition: form-data; name=\"name\"\r\n\r\nvalue that never ends"
	truncatedFile := "--XX\r\nContent-Disposition: form-data; name=\"avatar\"; filename=\"a.png\"\r\nContent-Type: " + png + "\r\n\r\n\x89PNG\r\n\x1a\n and then the body stops"
	notMultipart := "this is not a multipart message at all"
	for name, body := range map[string]string{
		"a part without a field name": nameless,
		"a field cut off":             truncatedField,
		"a file cut off":              truncatedFile,
		"no multipart at all":         notMultipart,
	} {
		store := newDisk(t)
		got, err := rawUpload(t, store, defaultOpts(), body)
		if err == nil {
			t.Errorf("%s: accepted %+v", name, got)
			continue
		}
		if faultCode(err) == "" {
			t.Errorf("%s: %v is not a fault, so it would be a 500", name, err)
		}
		if strings.Contains(err.Error(), "value that never ends") {
			t.Errorf("%s: the body leaked into the error: %v", name, err)
		}
		assertNoBlobs(t, store)
	}
}

func TestReceiveJudgesDeclaredTypesThatHaveNoSignature(t *testing.T) {
	allowed := storage.UploadOptions{
		Field: "doc", MaxFileBytes: 4096,
		AllowedTypes: []string{"text/csv", "application/json", "application/xml", "image/svg+xml", "application/octet-stream"},
	}
	cases := map[string]struct {
		declared string
		content  []byte
		wantType string // empty: refused as an unsupported type
	}{
		"csv as declared":               {"text/csv", []byte("a,b\n1,2\n"), "text/csv"},
		"json as declared":              {"application/json", []byte(`{"a":1}`), "application/json"},
		"an unparsable declared type":   {"not a media type", []byte("a,b\n"), ""},
		"a type that is not allowed":    {"text/markdown", []byte("# title"), ""},
		"binary content under text":     {"text/csv", []byte{0x00, 0x01, 0x02, 0x03, 0xff, 0xfe}, ""},
		"svg source without a prolog":   {"image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`), "image/svg+xml"},
		"svg source with an xml prolog": {"image/svg+xml", []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"></svg>`), "image/svg+xml"},
		"a type that needs a signature": {"image/png", []byte("plain text, not a PNG"), ""},
	}
	for name, tc := range cases {
		store := newDisk(t)
		got, err := upload(t, store, allowed, part{field: "doc", filename: "f", contentType: tc.declared, content: tc.content})
		if tc.wantType == "" {
			if !errors.Is(err, storage.ErrUnsupportedType) {
				t.Errorf("%s: err = %v, want ErrUnsupportedType", name, err)
			}
			assertNoBlobs(t, store)
			continue
		}
		if err != nil || len(got.Files) != 1 || got.Files[0].ContentType != tc.wantType {
			t.Errorf("%s: %+v, %v; want stored as %s", name, got, err, tc.wantType)
		}
	}
}
