# File storage

Forge's `storage` package is the equivalent of Rails' Active Storage for local
disk: a `Storage` port, a disk adapter, HMAC-signed expiring links, an HTTP
handler that serves them safely, and a strict multipart upload helper. The
design and its trade-offs are in [ADR 0028](adr/0028-file-storage.md).

An S3-compatible adapter is **not** included; see
[Writing another adapter](#writing-another-adapter).

## Configuration

| Variable | Default | Meaning |
| -------- | ------- | ------- |
| `FORGE_STORAGE_ROOT` | empty (storage not used) | Directory of the disk adapter. In `production`: an absolute path on a persistent volume. |
| `FORGE_STORAGE_SIGNING_KEY` | empty | HMAC key of signed links: unpadded base64url of 32 to 1024 random bytes. Redacted (`config.Secret`). **Required in `production` whenever `FORGE_STORAGE_ROOT` is set**; there is no default key. |
| `FORGE_STORAGE_MAX_UPLOAD_BYTES` | `524288` | Largest uploaded file; at most `1073741824`, and when storage is configured not above `FORGE_HTTP_MAX_BODY_BYTES` |
| `FORGE_STORAGE_LINK_TTL` | `15m` | Default lifetime of signed links; `1s` to `168h` |

```sh
openssl rand -base64 32 | tr '+/' '-_' | tr -d '='   # a signing key
```

`FORGE_HTTP_MAX_BODY_BYTES` (default 1 MiB) caps **every** request body and an
upload route cannot raise it; raise it with the upload limit (multipart framing
adds a few hundred bytes per part) and remember that it applies to all routes.

## Wiring

```go
store, err := disk.NewFromConfig(cfg.Storage)    // creates blobs/, meta/, tmp/ under the root
signer, err := storage.NewSignerFromConfig(cfg.Storage)  // fails without a signing key
files, err := storage.NewHandler(store, signer, storage.HandlerOptions{})
err = r.Handle("GET /files/{key...}", files)
app.OnShutdown(func(context.Context) error { return store.Close() })
```

Hand out links:

```go
url, err := signer.URL("/files", info.Key, storage.SignOptions{
    TTL:     cfg.Storage.EffectiveLinkTTL(),
    Inline:  true,                       // images only; everything else downloads
    Binding: principal.SubjectID().String(),  // optional: the link works for this user only
})
```

and, if you bind links, give the handler the same value on the request:

```go
storage.HandlerOptions{Binding: func(r *http.Request) string { return userIDOf(r) }}
```

A link is `/files/<key>?expires=<unix>&sig=<base64url>[&disposition=inline]`.
Verification is HMAC-SHA256 (`crypto/hmac`) with constant-time comparison; a
wrong key, tampered parameter, extended expiry, duplicated parameter or
different binding is `403 link_invalid`, an authentic link past its time is
`403 link_expired`. Both are answered **before** the store is consulted, so the
handler does not reveal which keys exist.

### Rotating the signing key

`storage.NewSigner(newKey, oldKey)` signs with the first key and accepts both.
Deploy with both, wait for the longest link lifetime (at most 7 days), then
drop the old key. Rotation is the only way to revoke links before they expire.

## Uploading

```go
func (h *Avatars) Create(w http.ResponseWriter, r *http.Request) {
    got, err := storage.Receive(w, r, h.store, storage.UploadOptions{
        Field:        "avatar",
        AllowedTypes: []string{"image/png", "image/jpeg", "image/webp"},
        MaxFileBytes: h.cfg.Storage.EffectiveMaxUploadBytes(),
        Prefix:       "avatars",
    })
    if err != nil {
        web.Error(w, r, err) // 415, 413, 400 in the JSON error format
        return
    }
    info := got.Files[0] // info.Key is "avatars/<uuidv7>"; persist the key, not a URL
    ...
}
```

Authenticate, authorize and CSRF-check **before** `Receive`. Multipart bodies
are not parsed by `csrf.Protection` (it reads urlencoded forms), so send the
token in the `X-CSRF-Token` header from a `fetch`/Stimulus submit, or check the
field you get back in `got.Fields`.

`Receive` rejects, with a stable fault and without storing anything:

| Case | Result |
| ---- | ------ |
| request is not `multipart/form-data` | `415 unsupported_media_type` |
| body, file or text field over its limit | `413 storage_too_large` |
| content whose detected type is not allowed, or a label that does not match the content | `415 unsupported_media_type` |
| file in another field, more files than `MaxFiles`, empty file, unnamed part, non-identity `Content-Transfer-Encoding`, too many text fields, malformed body | `400 invalid_upload` |
| no file at all | `ErrNoFile` (`400 upload_missing`); `got.Fields` is still filled |

Files already stored by a rejected request are deleted.

### How the content type is decided

The client's `Content-Type` and file name are claims, not facts. `Receive`
sniffs the first 512 bytes with `http.DetectContentType`:

1. If the bytes identify a specific type (image, PDF, archive, HTML, XML...),
   that type is stored and **must** be in `AllowedTypes`. The label is
   irrelevant: an HTML page labelled `image/png` is refused unless
   `text/html` is allowed, and a PDF labelled `image/png` is stored as a PDF
   only if PDF is allowed.
2. If the bytes have no signature (plain text, CSV, JSON, SVG source...), the
   declared type is stored if it is allowed, except that types that must carry
   a signature (images, audio, video, fonts, PDF, archives) are refused, and
   text types (`text/*`, JSON, XML) need text content (binary data labelled
   `text/csv` is refused).
3. SVG with an XML prolog is sniffed as XML and stored as `image/svg+xml` only
   when you allow that type; it is always served as a download.

`AllowedTypes` is mandatory and has no wildcards. Office documents are
`application/zip` to the sniffer: allow `application/zip` explicitly if you
accept them (they are stored under that type).

The stored name is metadata only: the key is a UUIDv7, and
`storage.SanitizeFilename` removes directories, control and bidirectional
characters, quotes and leading dots before it is stored or put in a header.

## Serving: what the browser is allowed to do

Every response has `X-Content-Type-Options: nosniff`, a sandboxing
`Content-Security-Policy` (`default-src 'none'; sandbox; frame-ancestors
'none'`) and `Cache-Control: private`. **Only raster images**
(`image/png|jpeg|gif|webp|avif|bmp|x-icon`) can be shown inline, and only when
the link was signed with `Inline: true`; HTML, SVG, XML, JavaScript, PDF, text
and unknown types are always `Content-Disposition: attachment; filename="..."`.
Extend the inline set with `HandlerOptions.InlineTypes` only for types that
cannot run code on your origin. Byte ranges, `HEAD`, `ETag` and
`If-Modified-Since` work with the disk adapter.

Serve user files from a separate origin (a subdomain or domain that has no
cookies for the application) when you can; the headers above are defense in
depth, not a substitute.

## Disk layout and operations

```text
<root>/blobs/<key>        content
<root>/meta/<key>.json    content type, file name, size, SHA-256
<root>/tmp/               uploads in progress
```

- Writes are atomic (temp file, `fsync`, `rename`); a failed or oversized write
  leaves the previous blob untouched. The root is an `os.Root`, so traversal and
  symlink escapes are refused by the operating system as well as by key
  validation. Files are `0600`, directories `0750`: run the container as the
  user that owns the volume (the generated Dockerfile runs as `nonroot`).
- A crash can leave files in `tmp/`: run
  `store.PruneTemp(ctx, time.Hour)` periodically (for example from
  `jobs.Scheduler`).
- Metadata that is missing, unreadable or disagrees with the file's size is
  ignored and the blob is served as `application/octet-stream` (a download).
- Back up `blobs/` and `meta/` together. Disk storage is for one host, or hosts
  sharing a volume with working `rename` and `fsync` (not NFS).
- Keys of user content are UUIDv7 (`Receive`); keys you choose must satisfy
  `storage.ValidateKey`: slash-separated segments of `[A-Za-z0-9._-]`, no empty
  segment, no segment starting with a dot, at most 512 bytes and 8 segments.

## Writing another adapter

The `Storage` interface has four methods (`Put`, `Open`, `Stat`, `Delete`),
streams and keys only. An adapter for an object store implements them in its
own package, calls `storage.ValidateKey` first, enforces `PutOptions.MaxBytes`
while streaming (failing with `storage.ErrTooLarge`, leaving nothing), keeps
content type and file name as object metadata, maps "no such object" to
`storage.ErrNotFound`, and passes `storage/storagetest.Run`. `Signer`,
`Handler` and `Receive` work unchanged; `Handler` serves ranges when `Open`'s
reader implements `io.Seeker` and otherwise streams the whole object. Forge
does not ship an S3 adapter yet: signing S3 requests (SigV4) is a protocol of
its own that cannot be verified here against a real service, and a placeholder
is not acceptable.

## Testing

`storagetest.Run(t, func(t testing.TB) storage.Storage { ... })` is the
conformance suite (round trips, size limit boundary, unsafe keys, overwrite
atomicity, cancellation, concurrency). `storage_test` shows `Receive`,
`Handler` and `Signer` end to end through `router.Router`.
