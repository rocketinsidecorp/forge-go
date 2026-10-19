package storage

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rocketinsidecorp/forge-go/fault"
	"github.com/rocketinsidecorp/forge-go/web"
)

// HandlerOptions configures NewHandler.
type HandlerOptions struct {
	// PathValue is the name of the route wildcard that carries the key. The
	// default is "key", for a route such as "GET /files/{key...}".
	PathValue string
	// Binding returns the value links must have been signed with (see
	// SignOptions.Binding), typically the authenticated user's ID. Nil means
	// links are not bound.
	Binding func(r *http.Request) string
	// InlineTypes adds content types that links may display inline, beyond the
	// raster images of InlineSafe. Add only types that cannot run code in the
	// application's origin.
	InlineTypes []string
}

type handler struct {
	store  Storage
	signer *Signer
	opts   HandlerOptions
	inline map[string]bool
}

// maxPrivateCache caps how long a browser may reuse a response without asking
// again, whatever the remaining life of the link.
const maxPrivateCache = time.Hour

var errMethodNotAllowed = fault.New("method_not_allowed", "method not allowed", fault.CategoryInvalid, http.StatusMethodNotAllowed)

// NewHandler returns an http.Handler that serves blobs to holders of a valid
// signed link (Signer.URL). Mount it on a route with a key wildcard:
//
//	h, err := storage.NewHandler(store, signer, storage.HandlerOptions{})
//	err = r.Handle("GET /files/{key...}", h)
//
// Requests without a valid, unexpired signature for exactly the requested key
// are answered 403 before the store is touched, so the handler reveals nothing
// about which keys exist. Responses carry the stored content type, but a file
// is only displayed inline when its link asked for it and its type is safe
// (images); every other file is a download with the name stored at upload
// time. All responses set X-Content-Type-Options: nosniff and a sandboxing
// Content-Security-Policy, and are marked private. Byte ranges and conditional
// requests work when the store's reader is seekable (the disk adapter's is).
func NewHandler(store Storage, signer *Signer, opts HandlerOptions) (http.Handler, error) {
	if store == nil || signer == nil {
		return nil, errors.New("storage handler needs a store and a signer")
	}
	if opts.PathValue == "" {
		opts.PathValue = "key"
	}
	h := &handler{store: store, signer: signer, opts: opts, inline: map[string]bool{}}
	for t := range inlineSafe {
		h.inline[t] = true
	}
	for _, t := range opts.InlineTypes {
		normalized, err := NormalizeContentType(t)
		if err != nil || normalized == OctetStream {
			return nil, errors.New("storage handler inline type is invalid")
		}
		h.inline[normalized] = true
	}
	return h, nil
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		web.Error(w, r, errMethodNotAllowed)
		return
	}
	key := r.PathValue(h.opts.PathValue)
	binding := ""
	if h.opts.Binding != nil {
		binding = h.opts.Binding(r)
	}
	inline, err := h.signer.Verify(key, r.URL.Query(), binding)
	if err != nil {
		web.Error(w, r, err)
		return
	}
	body, info, err := h.store.Open(r.Context(), key)
	if err != nil {
		web.Error(w, r, err)
		return
	}
	defer func() { _ = body.Close() }()

	contentType, err := NormalizeContentType(info.ContentType)
	if err != nil {
		contentType = OctetStream
	}
	header := w.Header()
	header.Set("Content-Type", contentType)
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Content-Security-Policy", "default-src 'none'; sandbox; frame-ancestors 'none'")
	header.Set("Cache-Control", "private, max-age="+strconv.FormatInt(h.cacheLifetime(r), 10))
	if info.SHA256 != "" {
		header.Set("ETag", `"`+info.SHA256+`"`)
	}
	if inline && h.inline[contentType] {
		header.Set("Content-Disposition", "inline")
	} else {
		header.Set("Content-Disposition", attachment(info, key))
	}

	if seeker, ok := body.(io.ReadSeeker); ok {
		http.ServeContent(w, r, "", info.ModTime, seeker)
		return
	}
	header.Set("Content-Length", strconv.FormatInt(info.Size, 10))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, body)
	}
}

// cacheLifetime is how long, in seconds, the response may be cached: no longer
// than the link lives, and never more than maxPrivateCache.
func (h *handler) cacheLifetime(r *http.Request) int64 {
	expires, _, _, ok := parseLink(r.URL.Query())
	if !ok {
		return 0
	}
	remaining := time.Unix(expires, 0).Sub(h.signer.now())
	return int64(max(min(remaining, maxPrivateCache), 0) / time.Second)
}

// attachment builds the Content-Disposition of a download.
func attachment(info Info, key string) string {
	name := SanitizeFilename(info.Filename)
	if name == "" {
		name = key[strings.LastIndexByte(key, '/')+1:]
	}
	if value := mime.FormatMediaType("attachment", map[string]string{"filename": name}); value != "" {
		return value
	}
	return "attachment"
}
