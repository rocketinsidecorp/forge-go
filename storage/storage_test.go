package storage_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/router"
	"github.com/rocketinsidecorp/forge-go/storage"
	"github.com/rocketinsidecorp/forge-go/storage/disk"
	"github.com/rocketinsidecorp/forge-go/web"
)

var (
	bg  = context.Background()
	key = bytes.Repeat([]byte{7}, 32)
)

var roots sync.Map // *disk.Store -> its directory

func newDisk(t testing.TB) *disk.Store {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "storage")
	s, err := disk.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	roots.Store(s, dir)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func storageRoot(s *disk.Store) string {
	dir, _ := roots.Load(s)
	return dir.(string)
}

// --- keys and names ---

func TestValidateKey(t *testing.T) {
	for _, ok := range []string{"a", "A-b_c.d", "avatars/0190f1ee-7b1a-7000-8000-000000000000", "a/b/c/d/e/f/g/h", "v1.2.3", "a..b", "file.tar.gz"} {
		if err := storage.ValidateKey(ok); err != nil {
			t.Errorf("ValidateKey(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "..", ".", "a/..", "../a", "/a", "a/", "a//b", ".hidden", "a/.b", "a\\b", "a b", "a?b", "a#b", "a%2fb", "a\x00", "é", strings.Repeat("a", 129), strings.Repeat("a/", 8) + "a"} {
		if err := storage.ValidateKey(bad); !errors.Is(err, storage.ErrInvalidKey) {
			t.Errorf("ValidateKey(%q) = %v, want ErrInvalidKey", bad, err)
		}
	}
}

func TestNormalizeContentType(t *testing.T) {
	for in, want := range map[string]string{
		"": "application/octet-stream", "IMAGE/PNG": "image/png", "text/plain; charset=utf-8": "text/plain", "application/json;x=y": "application/json",
	} {
		if got, err := storage.NormalizeContentType(in); err != nil || got != want {
			t.Errorf("NormalizeContentType(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"nonsense", "text/", "/png", "text/plain\r\nX: y", "a b/c"} {
		if _, err := storage.NormalizeContentType(bad); err == nil {
			t.Errorf("NormalizeContentType(%q) succeeded", bad)
		}
	}
}

func TestSanitizeFilename(t *testing.T) {
	for in, want := range map[string]string{
		"report.pdf":             "report.pdf",
		"../../etc/passwd":       "passwd",
		`C:\Users\x\evil.exe`:    "evil.exe",
		"a\"b.txt":               "ab.txt",
		"line\r\nbreak.txt":      "linebreak.txt",
		".htaccess":              "htaccess",
		"  spaced  ":             "spaced",
		"photo\u202Egnp.exe":     "photognp.exe", // bidi override removed
		"résumé.pdf":             "résumé.pdf",
		"":                       "",
		"..":                     "",
		strings.Repeat("é", 300): strings.Repeat("é", 127),
	} {
		if got := storage.SanitizeFilename(in); got != want {
			t.Errorf("SanitizeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- signer ---

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

func TestSignerRoundTripAndEveryTamperingIsRejected(t *testing.T) {
	signer, err := storage.NewSigner(key)
	if err != nil {
		t.Fatal(err)
	}
	link, err := signer.Sign("docs/a.txt", storage.SignOptions{TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	q := link.Query()
	if _, err := signer.Verify("docs/a.txt", q, ""); err != nil {
		t.Fatalf("Verify of a fresh link = %v", err)
	}

	clone := func(mutate func(url.Values)) url.Values {
		c := url.Values{}
		for k, v := range q {
			c[k] = append([]string(nil), v...)
		}
		mutate(c)
		return c
	}
	flip := func(s string) string {
		b := []byte(s)
		if b[0] == 'A' {
			b[0] = 'B'
		} else {
			b[0] = 'A'
		}
		return string(b)
	}
	cases := map[string]struct {
		key   string
		query url.Values
	}{
		"other key":        {"docs/b.txt", q},
		"prefix of key":    {"docs", q},
		"flipped sig":      {"docs/a.txt", clone(func(v url.Values) { v.Set("sig", flip(v.Get("sig"))) })},
		"later expiry":     {"docs/a.txt", clone(func(v url.Values) { v.Set("expires", "99999999999") })},
		"inline added":     {"docs/a.txt", clone(func(v url.Values) { v.Set("disposition", "inline") })},
		"missing sig":      {"docs/a.txt", clone(func(v url.Values) { v.Del("sig") })},
		"missing expires":  {"docs/a.txt", clone(func(v url.Values) { v.Del("expires") })},
		"duplicate sig":    {"docs/a.txt", clone(func(v url.Values) { v.Add("sig", v.Get("sig")) })},
		"duplicate expiry": {"docs/a.txt", clone(func(v url.Values) { v.Add("expires", v.Get("expires")) })},
		"short sig":        {"docs/a.txt", clone(func(v url.Values) { v.Set("sig", v.Get("sig")[:20]) })},
		"padded sig":       {"docs/a.txt", clone(func(v url.Values) { v.Set("sig", v.Get("sig")+"=") })},
		"non-numeric exp":  {"docs/a.txt", clone(func(v url.Values) { v.Set("expires", "12abc") })},
		"signed exp+":      {"docs/a.txt", clone(func(v url.Values) { v.Set("expires", "+123") })},
		"bad disposition":  {"docs/a.txt", clone(func(v url.Values) { v.Set("disposition", "weird") })},
		"empty":            {"docs/a.txt", url.Values{}},
		"invalid key":      {"../etc/passwd", q},
	}
	for name, c := range cases {
		if _, err := signer.Verify(c.key, c.query, ""); !errors.Is(err, storage.ErrInvalidLink) {
			t.Errorf("%s: Verify = %v, want ErrInvalidLink", name, err)
		}
	}
	if _, err := signer.Verify("docs/a.txt", q, "someone-else"); !errors.Is(err, storage.ErrInvalidLink) {
		t.Errorf("a binding that was not signed = %v, want ErrInvalidLink", err)
	}
}

func TestSignerExpiry(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 10, 19, 12, 0, 0, 500_000_000, time.UTC)}
	signer, _ := storage.NewSigner(key)
	storage.SetClock(signer, clock.Now)
	link, _ := signer.Sign("k", storage.SignOptions{TTL: 10 * time.Second})
	if want := time.Date(2026, 10, 19, 12, 0, 11, 0, time.UTC); !link.Expires.Equal(want) {
		t.Fatalf("Expires = %v, want %v (never earlier than the TTL)", link.Expires, want)
	}
	clock.now = clock.now.Add(9 * time.Second)
	if _, err := signer.Verify("k", link.Query(), ""); err != nil {
		t.Fatalf("before expiry: %v", err)
	}
	clock.now = link.Expires
	if _, err := signer.Verify("k", link.Query(), ""); !errors.Is(err, storage.ErrExpiredLink) {
		t.Fatalf("at expiry: %v, want ErrExpiredLink", err)
	}
	// Moving the expiry forward is a tampered link, not a longer-lived one.
	q := link.Query()
	q.Set("expires", "99999999999")
	if _, err := signer.Verify("k", q, ""); !errors.Is(err, storage.ErrInvalidLink) {
		t.Fatalf("extended expiry: %v, want ErrInvalidLink", err)
	}
}

func TestSignerBindingKeyRotationAndOptions(t *testing.T) {
	signer, _ := storage.NewSigner(key)
	bound, _ := signer.Sign("k", storage.SignOptions{Binding: "user-1"})
	if _, err := signer.Verify("k", bound.Query(), "user-1"); err != nil {
		t.Fatalf("bound link for its user: %v", err)
	}
	if _, err := signer.Verify("k", bound.Query(), "user-2"); !errors.Is(err, storage.ErrInvalidLink) {
		t.Fatalf("bound link for another user: %v", err)
	}
	if _, err := signer.Verify("k", bound.Query(), ""); !errors.Is(err, storage.ErrInvalidLink) {
		t.Fatalf("bound link without a binding: %v", err)
	}

	newKey := bytes.Repeat([]byte{9}, 32)
	oldLink, _ := signer.Sign("k", storage.SignOptions{})
	rotated, _ := storage.NewSigner(newKey, key)
	if _, err := rotated.Verify("k", oldLink.Query(), ""); err != nil {
		t.Fatalf("a link signed with the previous key must still verify: %v", err)
	}
	fresh, _ := rotated.Sign("k", storage.SignOptions{})
	if _, err := signer.Verify("k", fresh.Query(), ""); !errors.Is(err, storage.ErrInvalidLink) {
		t.Fatal("a link signed with the new key must not verify under the old key alone")
	}
	other, _ := storage.NewSigner(newKey)
	if _, err := other.Verify("k", oldLink.Query(), ""); !errors.Is(err, storage.ErrInvalidLink) {
		t.Fatal("a signer without the old key accepted an old link")
	}

	inline, _ := signer.Sign("k", storage.SignOptions{Inline: true})
	if got, err := signer.Verify("k", inline.Query(), ""); err != nil || !got {
		t.Fatalf("inline link = %v, %v", got, err)
	}
	if _, err := signer.Sign("k", storage.SignOptions{TTL: 8 * 24 * time.Hour}); err == nil {
		t.Error("a TTL above 7 days was signed")
	}
	if _, err := signer.Sign("k", storage.SignOptions{TTL: time.Millisecond}); err == nil {
		t.Error("a sub-second TTL was signed")
	}
	if _, err := signer.Sign("../k", storage.SignOptions{}); err == nil {
		t.Error("an invalid key was signed")
	}
}

func TestNewSignerRejectsShortKeys(t *testing.T) {
	if _, err := storage.NewSigner(make([]byte, 31)); err == nil {
		t.Error("31-byte key accepted")
	}
	if _, err := storage.NewSigner(key, make([]byte, 5)); err == nil {
		t.Error("short previous key accepted")
	}
	if _, err := storage.NewSigner(nil); err == nil {
		t.Error("nil key accepted")
	}
}

func TestNewSignerFromConfig(t *testing.T) {
	if _, err := storage.NewSignerFromConfig(config.Storage{}); err == nil {
		t.Fatal("a missing key must not fall back to a default")
	}
	encoded := base64.RawURLEncoding.EncodeToString(key)
	signer, err := storage.NewSignerFromConfig(config.Storage{SigningKey: config.NewSecret(encoded)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := signer.Sign("k", storage.SignOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestSignerURL(t *testing.T) {
	signer, _ := storage.NewSigner(key)
	u, err := signer.URL("/files/", "a/b.png", storage.SignOptions{Inline: true})
	if err != nil || !strings.HasPrefix(u, "/files/a/b.png?") || !strings.Contains(u, "sig=") || !strings.Contains(u, "disposition=inline") {
		t.Fatalf("URL = %q, %v", u, err)
	}
	if abs, err := signer.URL("https://app.example.com/files", "k", storage.SignOptions{}); err != nil || !strings.HasPrefix(abs, "https://app.example.com/files/k?") {
		t.Fatalf("absolute URL = %q, %v", abs, err)
	}
	for _, bad := range []string{"", "files", "//evil.com", "javascript:alert(1)", "/f?x=1", "/f#x", `/f\x`, "ftp://x"} {
		if _, err := signer.URL(bad, "k", storage.SignOptions{}); err == nil {
			t.Errorf("prefix %q accepted", bad)
		}
	}
}

// --- handler ---

type fixture struct {
	store   *disk.Store
	signer  *storage.Signer
	handler http.Handler
	clock   *fakeClock
}

func newFixture(t *testing.T, opts storage.HandlerOptions) *fixture {
	t.Helper()
	store := newDisk(t)
	signer, _ := storage.NewSigner(key)
	clock := &fakeClock{now: time.Now()}
	storage.SetClock(signer, clock.Now)
	h, err := storage.NewHandler(store, signer, opts)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /files/{key...}", h)
	mux.Handle("POST /files/{key...}", h)
	mux.Handle("HEAD /files/{key...}", h)
	return &fixture{store: store, signer: signer, handler: mux, clock: clock}
}

func (f *fixture) put(t *testing.T, k, contentType, filename string, content []byte) {
	t.Helper()
	if _, err := f.store.Put(bg, k, bytes.NewReader(content), storage.PutOptions{ContentType: contentType, Filename: filename, MaxBytes: 1 << 20}); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) get(method, target string, headers ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}

func (f *fixture) url(t *testing.T, k string, opts storage.SignOptions) string {
	t.Helper()
	u, err := f.signer.URL("/files", k, opts)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestHandlerServesSignedLinks(t *testing.T) {
	f := newFixture(t, storage.HandlerOptions{})
	content := []byte("hello world")
	f.put(t, "docs/a.txt", "text/plain", "Quarterly report.txt", content)
	w := f.get("GET", f.url(t, "docs/a.txt", storage.SignOptions{}))
	if w.Code != 200 || w.Body.String() != "hello world" {
		t.Fatalf("GET = %d %q", w.Code, w.Body)
	}
	sum := sha256.Sum256(content)
	for header, want := range map[string]string{
		"Content-Type":            "text/plain",
		"X-Content-Type-Options":  "nosniff",
		"Content-Disposition":     `attachment; filename="Quarterly report.txt"`,
		"ETag":                    `"` + hex.EncodeToString(sum[:]) + `"`,
		"Content-Security-Policy": "default-src 'none'; sandbox; frame-ancestors 'none'",
	} {
		if got := w.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if cc := w.Header().Get("Cache-Control"); !strings.HasPrefix(cc, "private, max-age=") {
		t.Errorf("Cache-Control = %q, want private", cc)
	}
}

func TestHandlerHeadRangeAndConditionalRequests(t *testing.T) {
	f := newFixture(t, storage.HandlerOptions{})
	f.put(t, "r.bin", "application/octet-stream", "", []byte("0123456789"))
	u := f.url(t, "r.bin", storage.SignOptions{})
	if w := f.get("HEAD", u); w.Code != 200 || w.Body.Len() != 0 || w.Header().Get("Content-Length") != "10" {
		t.Fatalf("HEAD = %d, %d body bytes, Content-Length %q", w.Code, w.Body.Len(), w.Header().Get("Content-Length"))
	}
	w := f.get("GET", u, "Range", "bytes=2-4")
	if w.Code != http.StatusPartialContent || w.Body.String() != "234" {
		t.Fatalf("range = %d %q", w.Code, w.Body)
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("range responses must keep nosniff")
	}
	etag := f.get("GET", u).Header().Get("ETag")
	if w := f.get("GET", u, "If-None-Match", etag); w.Code != http.StatusNotModified {
		t.Fatalf("conditional GET = %d, want 304", w.Code)
	}
}

func TestHandlerNeverServesActiveContentInline(t *testing.T) {
	f := newFixture(t, storage.HandlerOptions{})
	cases := map[string]struct {
		contentType string
		wantInline  bool
	}{
		"png":   {"image/png", true},
		"jpeg":  {"image/jpeg", true},
		"html":  {"text/html", false},
		"svg":   {"image/svg+xml", false},
		"xml":   {"text/xml", false},
		"js":    {"application/javascript", false},
		"pdf":   {"application/pdf", false},
		"xhtml": {"application/xhtml+xml", false},
		"octet": {"application/octet-stream", false},
		"plain": {"text/plain", false},
	}
	for name, c := range cases {
		key := "files/" + name
		f.put(t, key, c.contentType, "evil."+name, []byte("<script>alert(document.domain)</script>"))
		// Even a link that explicitly asks for inline display.
		for _, inline := range []bool{true, false} {
			w := f.get("GET", f.url(t, key, storage.SignOptions{Inline: inline}))
			disposition := w.Header().Get("Content-Disposition")
			gotInline := disposition == "inline"
			if want := c.wantInline && inline; gotInline != want {
				t.Errorf("%s (link inline=%v): Content-Disposition = %q, inline want %v", name, inline, disposition, want)
			}
			if !gotInline && !strings.HasPrefix(disposition, "attachment") {
				t.Errorf("%s: Content-Disposition = %q, want attachment", name, disposition)
			}
			if w.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "sandbox") {
				t.Errorf("%s: missing nosniff or sandbox CSP", name)
			}
		}
	}
}

func TestHandlerInlineTypesExtendTheDefaultSet(t *testing.T) {
	f := newFixture(t, storage.HandlerOptions{InlineTypes: []string{"audio/mpeg"}})
	f.put(t, "a.mp3", "audio/mpeg", "", []byte("ID3"))
	if got := f.get("GET", f.url(t, "a.mp3", storage.SignOptions{Inline: true})).Header().Get("Content-Disposition"); got != "inline" {
		t.Fatalf("configured inline type = %q", got)
	}
	if _, err := storage.NewHandler(newDisk(t), f.signer, storage.HandlerOptions{InlineTypes: []string{"bogus"}}); err == nil {
		t.Fatal("invalid inline type accepted")
	}
}

func TestHandlerRejectsInvalidAndExpiredLinksWithoutTouchingTheStore(t *testing.T) {
	f := newFixture(t, storage.HandlerOptions{})
	f.put(t, "secret.txt", "text/plain", "", []byte("TOPSECRETCONTENT"))
	good := f.url(t, "secret.txt", storage.SignOptions{TTL: time.Minute})

	for name, target := range map[string]string{
		"no signature":      "/files/secret.txt",
		"tampered":          strings.Replace(good, "sig=", "sig=A", 1),
		"other key":         strings.Replace(good, "secret.txt", "other.txt", 1),
		"traversal":         "/files/../secret.txt?" + strings.SplitN(good, "?", 2)[1],
		"encoded traversal": "/files/%2e%2e/secret.txt?" + strings.SplitN(good, "?", 2)[1],
		"missing key":       f.url(t, "missing.txt", storage.SignOptions{}) + "x",
	} {
		w := f.get("GET", target)
		if w.Code == 200 || strings.Contains(w.Body.String(), "TOPSECRETCONTENT") {
			t.Errorf("%s: %d %q, want rejection", name, w.Code, w.Body)
		}
	}
	if w := f.get("GET", strings.Replace(good, "sig=", "sig=A", 1)); w.Code != 403 || !strings.Contains(w.Body.String(), "link_invalid") {
		t.Errorf("tampered signature = %d %s, want 403 link_invalid", w.Code, w.Body)
	}
	// A valid signature for a key that does not exist is a plain 404, but only
	// the holder of a valid link learns that.
	if w := f.get("GET", f.url(t, "nothing-here", storage.SignOptions{})); w.Code != 404 {
		t.Errorf("signed missing key = %d, want 404", w.Code)
	}
	if w := f.get("GET", "/files/nothing-here"); w.Code != 403 {
		t.Errorf("unsigned missing key = %d, want 403 (no existence oracle)", w.Code)
	}

	f.clock.now = f.clock.now.Add(2 * time.Minute)
	w := f.get("GET", good)
	if w.Code != 403 || !strings.Contains(w.Body.String(), "link_expired") {
		t.Fatalf("expired link = %d %s, want 403 link_expired", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "TOPSECRETCONTENT") {
		t.Fatal("an expired link leaked content")
	}
}

func TestHandlerBindingStopsForwardedLinks(t *testing.T) {
	var user string
	f := newFixture(t, storage.HandlerOptions{Binding: func(r *http.Request) string { return user }})
	f.put(t, "k", "text/plain", "", []byte("v"))
	user = "alice"
	u := f.url(t, "k", storage.SignOptions{Binding: "alice"})
	if f.get("GET", u).Code != 200 {
		t.Fatal("the bound user must be served")
	}
	user = "mallory"
	if w := f.get("GET", u); w.Code != 403 {
		t.Fatalf("a forwarded link = %d, want 403", w.Code)
	}
}

func TestHandlerMethodsAndOptions(t *testing.T) {
	f := newFixture(t, storage.HandlerOptions{})
	f.put(t, "k", "text/plain", "", []byte("v"))
	w := f.get("POST", f.url(t, "k", storage.SignOptions{}))
	if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("POST = %d Allow %q", w.Code, w.Header().Get("Allow"))
	}
	if _, err := storage.NewHandler(nil, f.signer, storage.HandlerOptions{}); err == nil {
		t.Error("nil store accepted")
	}
	if _, err := storage.NewHandler(f.store, nil, storage.HandlerOptions{}); err == nil {
		t.Error("nil signer accepted")
	}
}

func TestHandlerStoredMetadataCannotInjectHeaders(t *testing.T) {
	f := newFixture(t, storage.HandlerOptions{})
	f.put(t, "k", "text/plain", "a\r\nSet-Cookie: pwn=1\r\n\r\n<script>.txt", []byte("v"))
	w := f.get("GET", f.url(t, "k", storage.SignOptions{}))
	if w.Header().Get("Set-Cookie") != "" || strings.ContainsAny(w.Header().Get("Content-Disposition"), "\r\n") {
		t.Fatalf("header injection: %v", w.Header())
	}
}

// --- upload ---

type part struct {
	field, filename, contentType string
	content                      []byte
	headers                      map[string]string
}

func multipartBody(t *testing.T, parts []part) (*bytes.Buffer, string) {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for _, p := range parts {
		h := textproto.MIMEHeader{}
		disposition := `form-data; name="` + p.field + `"`
		if p.filename != "" {
			disposition += `; filename="` + p.filename + `"`
		}
		h.Set("Content-Disposition", disposition)
		if p.contentType != "" {
			h.Set("Content-Type", p.contentType)
		}
		for k, v := range p.headers {
			h.Set(k, v)
		}
		pw, err := w.CreatePart(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pw.Write(p.content); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return &body, w.FormDataContentType()
}

func upload(t *testing.T, store storage.Storage, opts storage.UploadOptions, parts ...part) (storage.Received, error) {
	t.Helper()
	body, contentType := multipartBody(t, parts)
	r := httptest.NewRequest("POST", "/upload", body)
	r.Header.Set("Content-Type", contentType)
	return storage.Receive(httptest.NewRecorder(), r, store, opts)
}

var (
	pngBytes = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 64)...)
	pdfBytes = append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte("x"), 64)...)
)

func defaultOpts() storage.UploadOptions {
	return storage.UploadOptions{Field: "avatar", AllowedTypes: []string{"image/png", "image/jpeg", "text/csv"}, MaxFileBytes: 4096, Prefix: "avatars"}
}

func assertNoBlobs(t *testing.T, s *disk.Store) {
	t.Helper()
	found := 0
	_ = filepath.WalkDir(storageRoot(s), func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.Contains(filepath.ToSlash(path), "/blobs/") {
			found++
		}
		return nil
	})
	if found != 0 {
		t.Fatalf("%d blob(s) left behind by a rejected upload", found)
	}
}

func TestReceiveStoresAnAllowedFileAndReturnsFields(t *testing.T) {
	store := newDisk(t)
	got, err := upload(t, store, defaultOpts(),
		part{field: "authenticity_token", content: []byte("tok")},
		part{field: "name", content: []byte("Ana")},
		part{field: "avatar", filename: "me.png", contentType: "image/png", content: pngBytes},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Files) != 1 || got.Fields.Get("name") != "Ana" || got.Fields.Get("authenticity_token") != "tok" {
		t.Fatalf("Received = %+v", got)
	}
	info := got.Files[0]
	if !strings.HasPrefix(info.Key, "avatars/") || info.ContentType != "image/png" || info.Filename != "me.png" || info.Size != int64(len(pngBytes)) {
		t.Fatalf("Info = %+v", info)
	}
	rc, _, err := store.Open(bg, info.Key)
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(stored, pngBytes) {
		t.Fatal("stored content differs from the upload")
	}
}

func TestReceiveRejectsDisallowedAndDisguisedTypes(t *testing.T) {
	html := []byte("<!DOCTYPE html><html><script>alert(1)</script></html>")
	svg := []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>`)
	exe := append([]byte("MZ\x90\x00\x03\x00\x00\x00"), bytes.Repeat([]byte{0x00}, 100)...)
	cases := map[string]part{
		"html as png":             {field: "avatar", filename: "a.png", contentType: "image/png", content: html},
		"html as csv":             {field: "avatar", filename: "a.csv", contentType: "text/csv", content: html},
		"html declared html":      {field: "avatar", filename: "a.html", contentType: "text/html", content: html},
		"svg as png":              {field: "avatar", filename: "a.png", contentType: "image/png", content: svg},
		"svg declared svg":        {field: "avatar", filename: "a.svg", contentType: "image/svg+xml", content: svg},
		"pdf not allowed":         {field: "avatar", filename: "a.pdf", contentType: "application/pdf", content: pdfBytes},
		"pdf as png":              {field: "avatar", filename: "a.png", contentType: "image/png", content: pdfBytes},
		"junk claims png":         {field: "avatar", filename: "a.png", contentType: "image/png", content: []byte("this is not a png at all")},
		"exe claims png":          {field: "avatar", filename: "a.png", contentType: "image/png", content: exe},
		"exe claims csv":          {field: "avatar", filename: "a.csv", contentType: "text/csv", content: exe},
		"unlisted declared plain": {field: "avatar", filename: "a.txt", contentType: "text/plain", content: []byte("hi")},
		"no content type, binary": {field: "avatar", filename: "a.bin", content: exe},
	}
	for name, p := range cases {
		store := newDisk(t)
		if _, err := upload(t, store, defaultOpts(), p); !errors.Is(err, storage.ErrUnsupportedType) {
			t.Errorf("%s: err = %v, want ErrUnsupportedType", name, err)
		}
		assertNoBlobs(t, store)
	}
}

func TestReceiveAcceptsUndetectableTypesOnlyWhenDeclaredAndAllowed(t *testing.T) {
	store := newDisk(t)
	got, err := upload(t, store, defaultOpts(), part{field: "avatar", filename: "d.csv", contentType: "text/csv", content: []byte("a,b\n1,2\n")})
	if err != nil || got.Files[0].ContentType != "text/csv" {
		t.Fatalf("csv = %+v, %v", got, err)
	}
	// SVG sniffed as XML is stored as svg when explicitly allowed (and never inline).
	opts := defaultOpts()
	opts.AllowedTypes = []string{"image/svg+xml"}
	svg := []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"/>`)
	got, err = upload(t, store, opts, part{field: "avatar", filename: "x.svg", contentType: "image/svg+xml", content: svg})
	if err != nil || got.Files[0].ContentType != "image/svg+xml" {
		t.Fatalf("svg = %+v, %v", got, err)
	}
}

func TestReceiveRejectsOversizedFilesAndLeavesNothingBehind(t *testing.T) {
	store := newDisk(t)
	big := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{1}, 5000)...)
	_, err := upload(t, store, defaultOpts(), part{field: "avatar", filename: "big.png", contentType: "image/png", content: big})
	if !errors.Is(err, storage.ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	assertNoBlobs(t, store)

	// A file over the limit after a good one: the good one is removed too.
	opts := defaultOpts()
	opts.MaxFiles = 2
	_, err = upload(t, store, opts,
		part{field: "avatar", filename: "ok.png", contentType: "image/png", content: pngBytes},
		part{field: "avatar", filename: "big.png", contentType: "image/png", content: big},
	)
	if !errors.Is(err, storage.ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	assertNoBlobs(t, store)
}

func TestReceiveEnforcesTheRequestBodyCap(t *testing.T) {
	store := newDisk(t)
	opts := defaultOpts()
	opts.MaxFileBytes = 1000
	// A request far bigger than one file, field and framing allowance, from a
	// client that does not declare its length: rejected before it is stored.
	huge := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{1}, 300_000)...)
	body, contentType := multipartBody(t, []part{{field: "avatar", filename: "h.png", contentType: "image/png", content: huge}})
	r := httptest.NewRequest("POST", "/upload", body)
	r.Header.Set("Content-Type", contentType)
	r.ContentLength = -1 // chunked: the declared length cannot be trusted either
	if _, err := storage.Receive(httptest.NewRecorder(), r, store, opts); !errors.Is(err, storage.ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	assertNoBlobs(t, store)
}

func TestReceiveStrictStructure(t *testing.T) {
	png := func(field string) part {
		return part{field: field, filename: "a.png", contentType: "image/png", content: pngBytes}
	}
	cases := map[string]struct {
		parts []part
		want  error
	}{
		"file in another field": {[]part{png("other")}, nil},
		"too many files":        {[]part{png("avatar"), png("avatar")}, nil},
		"empty file":            {[]part{{field: "avatar", filename: "e.png", contentType: "image/png"}}, nil},
		"encoded part": {[]part{{field: "avatar", filename: "a.png", contentType: "image/png", content: pngBytes,
			headers: map[string]string{"Content-Transfer-Encoding": "base64"}}}, nil},
		"no file":              {[]part{{field: "name", content: []byte("x")}}, storage.ErrNoFile},
		"empty file input":     {[]part{{field: "avatar", filename: "", content: nil}}, storage.ErrNoFile},
		"oversized text field": {[]part{{field: "bio", content: bytes.Repeat([]byte("x"), 70_000)}, png("avatar")}, storage.ErrTooLarge},
	}
	for name, c := range cases {
		store := newDisk(t)
		_, err := upload(t, store, defaultOpts(), c.parts...)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if c.want != nil && !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		}
		assertNoBlobs(t, store)
	}

	// Too many text fields.
	var parts []part
	for i := range 40 {
		parts = append(parts, part{field: "f" + string(rune('a'+i%26)) + string(rune('a'+i/26)), content: []byte("v")})
	}
	if _, err := upload(t, newDisk(t), defaultOpts(), append(parts, png("avatar"))...); err == nil {
		t.Error("40 text fields accepted with the default bound of 32")
	}
}

func TestReceiveRejectsWrongRequests(t *testing.T) {
	store := newDisk(t)
	for name, contentType := range map[string]string{
		"json":                 "application/json",
		"urlencoded":           "application/x-www-form-urlencoded",
		"multipart w/o bounds": "multipart/form-data",
		"none":                 "",
	} {
		r := httptest.NewRequest("POST", "/upload", strings.NewReader("x"))
		if contentType != "" {
			r.Header.Set("Content-Type", contentType)
		}
		_, err := storage.Receive(httptest.NewRecorder(), r, store, defaultOpts())
		if !errors.Is(err, storage.ErrUnsupportedType) && (err == nil || !strings.Contains(err.Error(), "unsupported_media_type")) {
			t.Errorf("%s: err = %v, want 415", name, err)
		}
	}
	r := httptest.NewRequest("POST", "/upload", strings.NewReader("--x\r\nbroken"))
	r.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	if _, err := storage.Receive(httptest.NewRecorder(), r, store, defaultOpts()); err == nil {
		t.Error("a malformed body was accepted")
	}
}

func TestReceiveOptionValidation(t *testing.T) {
	store := newDisk(t)
	bad := map[string]func(*storage.UploadOptions){
		"no field":       func(o *storage.UploadOptions) { o.Field = "" },
		"no types":       func(o *storage.UploadOptions) { o.AllowedTypes = nil },
		"wildcard":       func(o *storage.UploadOptions) { o.AllowedTypes = []string{"image/*"} },
		"garbage type":   func(o *storage.UploadOptions) { o.AllowedTypes = []string{"nope"} },
		"no size":        func(o *storage.UploadOptions) { o.MaxFileBytes = 0 },
		"bad prefix":     func(o *storage.UploadOptions) { o.Prefix = "../x" },
		"too many files": func(o *storage.UploadOptions) { o.MaxFiles = 5000 },
	}
	for name, mutate := range bad {
		opts := defaultOpts()
		mutate(&opts)
		if _, err := upload(t, store, opts, part{field: "avatar", filename: "a.png", contentType: "image/png", content: pngBytes}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := storage.Receive(httptest.NewRecorder(), httptest.NewRequest("POST", "/", nil), nil, defaultOpts()); err == nil {
		t.Error("nil store accepted")
	}
}

func TestReceiveMultipleFilesAndUniqueKeys(t *testing.T) {
	store := newDisk(t)
	opts := defaultOpts()
	opts.MaxFiles = 3
	got, err := upload(t, store, opts,
		part{field: "avatar", filename: "1.png", contentType: "image/png", content: pngBytes},
		part{field: "avatar", filename: "2.png", contentType: "image/png", content: pngBytes},
	)
	if err != nil || len(got.Files) != 2 || got.Files[0].Key == got.Files[1].Key {
		t.Fatalf("Received = %+v, %v", got, err)
	}
}

func TestReceiveNeverUsesTheClientFilenameAsAKey(t *testing.T) {
	store := newDisk(t)
	got, err := upload(t, store, defaultOpts(), part{field: "avatar", filename: "../../../etc/cron.d/evil", contentType: "image/png", content: pngBytes})
	if err != nil {
		t.Fatal(err)
	}
	info := got.Files[0]
	if strings.Contains(info.Key, "etc") || strings.Contains(info.Key, "..") || info.Filename != "evil" {
		t.Fatalf("Info = %+v: the client's file name must be sanitized metadata only", info)
	}
}

func TestReceiveCancelledRequestStoresNothing(t *testing.T) {
	store := newDisk(t)
	body, contentType := multipartBody(t, []part{{field: "avatar", filename: "a.png", contentType: "image/png", content: pngBytes}})
	ctx, cancel := context.WithCancel(bg)
	cancel()
	r := httptest.NewRequest("POST", "/upload", body).WithContext(ctx)
	r.Header.Set("Content-Type", contentType)
	if _, err := storage.Receive(httptest.NewRecorder(), r, store, defaultOpts()); err == nil {
		t.Fatal("a cancelled request was accepted")
	}
	assertNoBlobs(t, store)
}

func TestEndToEndThroughTheForgeRouter(t *testing.T) {
	store := newDisk(t)
	signer, _ := storage.NewSigner(key)
	h, err := storage.NewHandler(store, signer, storage.HandlerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rt := router.New()
	if err := rt.Handle("GET /files/{key...}", h); err != nil {
		t.Fatal(err)
	}
	if err := rt.Handle("POST /avatars", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := storage.Receive(w, r, store, defaultOpts())
		if err != nil {
			web.Error(w, r, err)
			return
		}
		link, err := signer.URL("/files", got.Files[0].Key, storage.SignOptions{Inline: true})
		if err != nil {
			web.Error(w, r, err)
			return
		}
		web.JSON(w, http.StatusCreated, map[string]string{"url": link})
	})); err != nil {
		t.Fatal(err)
	}

	body, contentType := multipartBody(t, []part{{field: "avatar", filename: "me.png", contentType: "image/png", content: pngBytes}})
	r := httptest.NewRequest("POST", "/avatars", body)
	r.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	rt.ServeHTTP(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("upload = %d %s", w.Code, w.Body)
	}
	var created struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || created.URL == "" {
		t.Fatalf("body = %s, %v", w.Body, err)
	}

	w = httptest.NewRecorder()
	rt.ServeHTTP(w, httptest.NewRequest("GET", created.URL, nil))
	if w.Code != 200 || w.Header().Get("Content-Disposition") != "inline" || w.Header().Get("Content-Type") != "image/png" || !bytes.Equal(w.Body.Bytes(), pngBytes) {
		t.Fatalf("download = %d %v", w.Code, w.Header())
	}

	// The router itself refuses dot segments before the handler runs.
	w = httptest.NewRecorder()
	rt.ServeHTTP(w, httptest.NewRequest("GET", "/files/../../etc/passwd?expires=1&sig=x", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("traversal through the router = %d, want 400", w.Code)
	}

	// A disallowed upload is a 415 in the JSON error format.
	body, contentType = multipartBody(t, []part{{field: "avatar", filename: "x.png", contentType: "image/png", content: []byte("<html>")}})
	r = httptest.NewRequest("POST", "/avatars", body)
	r.Header.Set("Content-Type", contentType)
	w = httptest.NewRecorder()
	rt.ServeHTTP(w, r)
	if w.Code != http.StatusUnsupportedMediaType || !strings.Contains(w.Body.String(), "unsupported_media_type") {
		t.Fatalf("bad upload = %d %s", w.Code, w.Body)
	}
}

func TestReceiveCountsRepeatedFieldNamesAgainstTheFieldBudget(t *testing.T) {
	opts := defaultOpts()
	opts.MaxFields = 2
	opts.MaxFieldBytes = 4 << 10
	opts.MaxFileBytes = 8 << 20 // a generous file limit also widens the request cap
	// One name repeated: a per-name count would see a single field and buffer
	// every value (here 1500 x 4 KiB, within the request cap) in memory.
	var parts []part
	for range 1500 {
		parts = append(parts, part{field: "note", content: bytes.Repeat([]byte("x"), 4<<10)})
	}
	parts = append(parts, part{field: "avatar", filename: "a.png", contentType: "image/png", content: pngBytes})
	store := newDisk(t)
	if _, err := upload(t, store, opts, parts...); err == nil {
		t.Fatal("1500 values of one field name were accepted with MaxFields=2")
	}
	assertNoBlobs(t, store)

	// Within the budget, repetitions are still fine.
	got, err := upload(t, store, opts,
		part{field: "tag", content: []byte("a")}, part{field: "tag", content: []byte("b")},
		part{field: "avatar", filename: "a.png", contentType: "image/png", content: pngBytes})
	if err != nil || len(got.Fields["tag"]) != 2 {
		t.Fatalf("two repeated values = %+v, %v", got, err)
	}
}
