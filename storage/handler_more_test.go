package storage_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/storage"
	"github.com/rocketinsidecorp/forge-go/storage/disk"
)

// streamOnlyStore serves what the disk store holds as a stream that cannot
// seek, as an object store would, and labels it with a content type the
// handler must not trust.
type streamOnlyStore struct {
	*disk.Store
	label string
}

func (s streamOnlyStore) Open(ctx context.Context, key string) (io.ReadCloser, storage.Info, error) {
	body, info, err := s.Store.Open(ctx, key)
	if err != nil {
		return nil, info, err
	}
	defer func() { _ = body.Close() }()
	data, err := io.ReadAll(body)
	if err != nil {
		return nil, info, err
	}
	info.ContentType = s.label
	return io.NopCloser(bytes.NewReader(data)), info, nil
}

func TestHandlerServesStoresThatCannotSeekAndDistrustsTheirContentType(t *testing.T) {
	t.Parallel()
	local := newDisk(t)
	if _, err := local.Put(bg, "report.bin", bytes.NewReader([]byte("0123456789")), storage.PutOptions{ContentType: "text/plain", Filename: "report.txt", MaxBytes: 1 << 20}); err != nil {
		t.Fatal(err)
	}
	signer, err := storage.NewSigner(key)
	if err != nil {
		t.Fatal(err)
	}
	storage.SetClock(signer, time.Now)
	handler, err := storage.NewHandler(streamOnlyStore{Store: local, label: "not a content type"}, signer, storage.HandlerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /files/{key...}", handler)
	mux.Handle("HEAD /files/{key...}", handler)
	target, err := signer.URL("/files", "report.bin", storage.SignOptions{})
	if err != nil {
		t.Fatal(err)
	}

	get := httptest.NewRecorder()
	mux.ServeHTTP(get, httptest.NewRequest(http.MethodGet, target, nil))
	if get.Code != http.StatusOK || get.Body.String() != "0123456789" || get.Header().Get("Content-Length") != "10" {
		t.Fatalf("GET = %d %q headers %v", get.Code, get.Body.String(), get.Header())
	}
	if got := get.Header().Get("Content-Type"); got != storage.OctetStream {
		t.Errorf("Content-Type = %q, want %s for a label the handler cannot trust", got, storage.OctetStream)
	}
	if get.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("headers = %v", get.Header())
	}

	head := httptest.NewRecorder()
	mux.ServeHTTP(head, httptest.NewRequest(http.MethodHead, target, nil))
	if head.Code != http.StatusOK || head.Body.Len() != 0 || head.Header().Get("Content-Length") != "10" {
		t.Errorf("HEAD = %d body %q headers %v", head.Code, head.Body.String(), head.Header())
	}
}
