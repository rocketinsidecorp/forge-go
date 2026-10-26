package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rocketinsidecorp/forge-go/fault"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

// ErrNoFile is returned by Receive when the request carries no file in the
// expected field (for example a browser form submitted without choosing one).
// The Received value still holds the form fields.
var ErrNoFile = fault.New("upload_missing", "no file was uploaded", fault.CategoryInvalid, http.StatusBadRequest)

func invalidUpload(message string) *fault.Error {
	return fault.New("invalid_upload", message, fault.CategoryInvalid, http.StatusBadRequest)
}

// UploadOptions configures Receive.
type UploadOptions struct {
	// Field is the name of the multipart field that carries the file(s).
	// Files in any other field are rejected.
	Field string
	// AllowedTypes is the allow-list of content types, such as "image/png".
	// The type that decides is the one detected from the content, not the one
	// the client claims; docs/STORAGE.md has the rules. At least one is
	// required: there is no allow-everything default.
	AllowedTypes []string
	// MaxFileBytes bounds each file (required, positive).
	MaxFileBytes int64
	// MaxFiles bounds the files in Field. Zero means 1.
	MaxFiles int
	// Prefix is a key prefix for the stored files (for example "avatars"),
	// validated like a key. Keys are Prefix + "/" + a fresh UUIDv7.
	Prefix string
	// MaxFields bounds the other (text) field values in total, counting every
	// repetition of a name. Zero means 32.
	MaxFields int
	// MaxFieldBytes bounds each text field. Zero means 64 KiB.
	MaxFieldBytes int64
}

// Received is the outcome of Receive.
type Received struct {
	// Files are the stored files, in order.
	Files []Info
	// Fields are the text fields of the form (including a CSRF token field:
	// multipart forms are not read by csrf.Protection, so send the token in
	// the X-CSRF-Token header, or verify it from these fields).
	Fields url.Values
}

const (
	sniffLength    = 512
	multipartSlack = 64 << 10 // headers and boundaries of a legitimate request
)

// Receive reads a multipart/form-data upload from r and stores its files in
// store, rejecting anything outside opts: wrong content type of the request,
// too many or too large files or fields, a file in another field, content whose
// detected type is not allowed, malformed or re-encoded parts. The request
// body is capped before it is read, files are streamed (only a 512-byte
// sniffing window of each is held), text fields are held in memory but bounded
// in count (MaxFields values in total, repeated names included) and in bytes
// (MaxFields x MaxFieldBytes in total), and a rejected upload leaves no stored
// files behind.
//
// The server-wide FORGE_HTTP_MAX_BODY_BYTES still applies and must be larger
// than the upload: Receive can lower the limit for this request but not raise
// it. Authenticate and authorize before calling Receive, and CSRF-protect the
// route.
//
// Errors are faults: 415 for a disallowed type, 413 for size, 400 for
// malformed or surplus parts, and ErrNoFile when no file was sent.
func Receive(w http.ResponseWriter, r *http.Request, store Storage, opts UploadOptions) (Received, error) {
	cfg, err := opts.normalize()
	if err != nil {
		return Received{}, err
	}
	if store == nil {
		return Received{}, errors.New("storage upload needs a store")
	}
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		return Received{}, fault.New("unsupported_media_type", "request body must be multipart/form-data", fault.CategoryInvalid, http.StatusUnsupportedMediaType)
	}
	limit := int64(cfg.maxFiles)*cfg.maxFileBytes + int64(cfg.maxFields)*cfg.maxFieldBytes + multipartSlack
	if r.ContentLength > limit {
		return Received{}, ErrTooLarge
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	reader := multipart.NewReader(r.Body, params["boundary"])

	result := Received{Fields: url.Values{}}
	fieldCount, fieldBytes := 0, int64(0)
	ctx := r.Context()
	fail := func(err error) (Received, error) {
		cleanup(ctx, store, result.Files)
		return Received{}, translateReadError(err)
	}
	for {
		part, err := reader.NextRawPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fail(err)
		}
		if encoding := part.Header.Get("Content-Transfer-Encoding"); encoding != "" &&
			!strings.EqualFold(encoding, "binary") && !strings.EqualFold(encoding, "8bit") && !strings.EqualFold(encoding, "7bit") {
			return fail(invalidUpload("encoded multipart parts are not supported"))
		}
		name := part.FormName()
		if name == "" || len(name) > 256 {
			return fail(invalidUpload("multipart part without a valid field name"))
		}
		if part.FileName() == "" {
			// The budget counts values, not distinct names: one name repeated
			// many times is many fields, and all of them stay in memory.
			fieldCount++
			if fieldCount > cfg.maxFields {
				return fail(invalidUpload("too many form fields"))
			}
			value, err := io.ReadAll(io.LimitReader(part, cfg.maxFieldBytes+1))
			if err != nil {
				return fail(err)
			}
			fieldBytes += int64(len(value))
			if int64(len(value)) > cfg.maxFieldBytes || fieldBytes > int64(cfg.maxFields)*cfg.maxFieldBytes {
				return fail(ErrTooLarge)
			}
			result.Fields.Add(name, string(value))
			continue
		}
		if name != cfg.field {
			return fail(invalidUpload("unexpected file field"))
		}
		if len(result.Files) >= cfg.maxFiles {
			return fail(invalidUpload("too many files"))
		}
		info, err := cfg.store(r.Context(), store, part)
		if err != nil {
			return fail(err)
		}
		result.Files = append(result.Files, info)
	}
	if len(result.Files) == 0 {
		return result, ErrNoFile
	}
	return result, nil
}

type uploadConfig struct {
	field         string
	allowed       map[string]bool
	maxFileBytes  int64
	maxFiles      int
	prefix        string
	maxFields     int
	maxFieldBytes int64
}

func (o UploadOptions) normalize() (uploadConfig, error) {
	c := uploadConfig{field: o.Field, maxFileBytes: o.MaxFileBytes, maxFiles: o.MaxFiles, prefix: o.Prefix, maxFields: o.MaxFields, maxFieldBytes: o.MaxFieldBytes}
	if c.field == "" || len(c.field) > 256 {
		return c, errors.New("storage upload field is required")
	}
	if c.maxFileBytes < 1 {
		return c, errors.New("storage upload max file bytes must be positive")
	}
	if len(o.AllowedTypes) == 0 {
		return c, errors.New("storage upload needs at least one allowed content type")
	}
	c.allowed = make(map[string]bool, len(o.AllowedTypes))
	for _, t := range o.AllowedTypes {
		normalized, err := NormalizeContentType(t)
		if err != nil || t == "" || strings.HasSuffix(normalized, "/*") {
			return c, errors.New("storage upload allowed content type is invalid")
		}
		c.allowed[normalized] = true
	}
	if c.maxFiles == 0 {
		c.maxFiles = 1
	}
	if c.maxFields == 0 {
		c.maxFields = 32
	}
	if c.maxFieldBytes == 0 {
		c.maxFieldBytes = 64 << 10
	}
	if c.maxFiles < 1 || c.maxFiles > 1000 || c.maxFields < 0 || c.maxFieldBytes < 0 {
		return c, errors.New("storage upload limits are invalid")
	}
	if c.prefix != "" {
		if err := ValidateKey(c.prefix + "/x"); err != nil {
			return c, errors.New("storage upload prefix is invalid")
		}
	}
	return c, nil
}

func (c uploadConfig) store(ctx context.Context, store Storage, part *multipart.Part) (Info, error) {
	head := make([]byte, sniffLength)
	n, err := io.ReadFull(part, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return Info{}, err
	}
	head = head[:n]
	if n == 0 {
		return Info{}, invalidUpload("the uploaded file is empty")
	}
	contentType, err := c.resolveType(part.Header.Get("Content-Type"), http.DetectContentType(head))
	if err != nil {
		return Info{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Info{}, err
	}
	key := id.String()
	if c.prefix != "" {
		key = c.prefix + "/" + key
	}
	return store.Put(ctx, key, io.MultiReader(bytes.NewReader(head), part), PutOptions{
		ContentType: contentType,
		Filename:    SanitizeFilename(part.FileName()),
		MaxBytes:    c.maxFileBytes,
	})
}

// resolveType decides the content type to store. The sniffed type is what the
// bytes are; the declared one is what the client says they are. A file whose
// bytes identify a specific type (an image, a PDF, an archive, HTML...) is
// stored as that type, which must be allowed; the client's claim then does not
// matter, so an HTML page labelled image/png is rejected unless text/html is
// allowed, and the label never upgrades anything. A file with no signature
// (plain text, CSV, JSON, SVG source...) keeps its declared type if that is
// allowed, except that a type which must have a signature is refused and a text
// type needs text content.
func (c uploadConfig) resolveType(declared, sniffed string) (string, error) {
	detected, err := NormalizeContentType(sniffed)
	if err != nil {
		return "", ErrUnsupportedType
	}
	if detected == "text/xml" && strings.EqualFold(strings.TrimSpace(strings.SplitN(declared, ";", 2)[0]), "image/svg+xml") && c.allowed["image/svg+xml"] {
		// SVG with an XML prolog is sniffed as XML. It is stored as SVG, which
		// the handler never displays inline.
		return "image/svg+xml", nil
	}
	if detected != "text/plain" && detected != OctetStream {
		if !c.allowed[detected] {
			return "", ErrUnsupportedType
		}
		return detected, nil
	}
	claimed := OctetStream
	if strings.TrimSpace(declared) != "" {
		if claimed, err = NormalizeContentType(declared); err != nil {
			return "", ErrUnsupportedType
		}
	}
	if claimed == OctetStream {
		claimed = detected
	}
	if hasSignature(claimed) || !c.allowed[claimed] {
		return "", ErrUnsupportedType
	}
	if isTextual(claimed) && detected != "text/plain" {
		return "", ErrUnsupportedType // binary content under a text label
	}
	return claimed, nil
}

// isTextual reports whether files of the type are text, so binary content
// cannot be one.
func isTextual(contentType string) bool {
	return strings.HasPrefix(contentType, "text/") || contentType == "application/json" ||
		contentType == "application/xml" || strings.HasSuffix(contentType, "+json") || strings.HasSuffix(contentType, "+xml")
}

// hasSignature reports whether files of the type can be recognized by their
// first bytes, so a file without the signature is not of that type.
func hasSignature(contentType string) bool {
	switch {
	case contentType == "image/svg+xml":
		return false
	case strings.HasPrefix(contentType, "image/"), strings.HasPrefix(contentType, "audio/"),
		strings.HasPrefix(contentType, "video/"), strings.HasPrefix(contentType, "font/"):
		return true
	}
	switch contentType {
	case "application/pdf", "application/zip", "application/gzip", "application/x-gzip", "application/wasm",
		"application/x-rar-compressed", "application/ogg", "application/postscript", "text/html", "text/xml":
		return true
	}
	return false
}

// translateReadError maps failures of reading the request to faults.
func translateReadError(err error) error {
	var maxBytes *http.MaxBytesError
	switch {
	case fault.From(err) != nil:
		return err
	case errors.As(err, &maxBytes), errors.Is(err, multipart.ErrMessageTooLarge):
		return ErrTooLarge
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	default:
		return invalidUpload("the multipart body is malformed").WithCause(err)
	}
}

// cleanup removes files stored by a rejected upload. It keeps the request's
// values but not its cancellation: the request's context may be the reason the
// upload failed.
func cleanup(parent context.Context, store Storage, files []Info) {
	if len(files) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
	defer cancel()
	for _, f := range files {
		_ = store.Delete(ctx, f.Key)
	}
}
