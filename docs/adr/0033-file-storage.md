# ADR 0033: File storage

- **Status:** Accepted
- **Date:** 2026-10-19

## Context

Rails' Active Storage attaches files to records, stores them on a service
(disk, S3, ...), and serves them through signed, expiring URLs. The dangerous
parts of that feature are well known: path traversal in the store, serving a
user-supplied HTML or SVG file from the application origin (stored XSS),
trusting the client's content type, unbounded uploads, partial files after a
crash, and URLs that outlive the permission that created them.

Forge's roadmap listed "multipart forms and file uploads". The core has no
third-party dependencies (ADR 0006), so the first adapter is the local disk, and
the port has to be small enough that an object-store adapter fits behind it.

## Decision

### Package layout

| Package | Role |
|---------|------|
| `storage` (core) | `Storage` port, `Info`, key and content-type rules, `Signer`, `Handler`, `Receive` (multipart upload) |
| `storage/disk` (core, stdlib only) | disk adapter |
| `storage/storagetest` (core) | conformance suite |

### The port

```go
type Storage interface {
    Put(ctx, key, body io.Reader, PutOptions{ContentType, Filename, MaxBytes}) (Info, error)
    Open(ctx, key) (io.ReadCloser, Info, error)
    Stat(ctx, key) (Info, error)
    Delete(ctx, key) error
}
```

`MaxBytes` is required: there is no unbounded `Put`. A failed or oversized
`Put` leaves no blob and does not replace an existing one. `Open`'s reader may
implement `io.Seeker`, in which case the handler serves byte ranges and
conditional requests. Nothing else is required of an adapter, so an object
store adapter implements the four methods and reuses `Signer`, `Handler` and
`Receive` unchanged.

**No S3 adapter ships.** A correct SigV4 implementation with the standard
library is feasible but is a signing protocol (canonical requests, regional
scopes, streaming and multipart edge cases, presigned URLs, endpoints of
S3-compatible vendors) that cannot be verified here against a real service. A
placeholder adapter is forbidden by the project rules, so the port is documented
and the adapter is a future item.

### Keys

A key is slash-separated segments of `[A-Za-z0-9._-]`: at most 512 bytes,
8 segments, 128 bytes per segment, no empty segment and **no segment starting
with a dot**. `ValidateKey` therefore makes `..`, `.`, hidden files, absolute
paths, backslashes, drive letters, percent escapes and non-ASCII impossible by
construction, in every adapter, before any file system call. Keys for user
content are UUIDv7 (`Receive`); the original file name is metadata, never part
of the key.

### Disk adapter

- The root directory is opened once as an `os.Root`: the kernel-assisted
  traversal-resistant API refuses paths that leave the root through `..`,
  absolute paths **or symbolic links** (defense in depth behind `ValidateKey`).
- Writes are atomic: stream to `tmp/<random>` in the root with `O_EXCL`, hash
  and count while copying, `fsync`, then `rename` onto `blobs/<key>`. Readers
  see the old file or the new one. A crash leaves at most an orphan in `tmp/`,
  which `PruneTemp(ctx, olderThan)` removes (refusing ages under a minute so it
  cannot delete uploads in progress).
- Content type, file name, size and SHA-256 are stored in `meta/<key>.json`.
  The metadata is renamed into place before the blob. It is trusted only while
  its recorded size equals the file's size; otherwise the blob is
  `application/octet-stream` (a download).
- Files are `0600`, directories `0750`.
- Disk storage is for one host (or hosts sharing a volume with working
  `rename`/`fsync`, not NFS).

### Signed links

`Signer` signs `HMAC-SHA256(key, "forge.storage.url.v1\n" key "\n" expires
"\n" disposition "\n" binding)` with `crypto/hmac`: no custom cryptography, the
signature is compared with `hmac.Equal` (constant time) against every
configured key, and the whole link, key and parameters are fixed. A link carries
`expires` (unix seconds), `sig` (unpadded base64url) and optionally
`disposition=inline`. Verification is strict: exactly one of each parameter,
decimal expiry, canonical 43-character signature. `ErrExpiredLink` is only
returned for an *authentic* expired link, so expiry cannot be used to learn
anything about forged ones.

- Lifetime defaults to 15 minutes, at most 7 days (`FORGE_STORAGE_LINK_TTL`
  is the application's default to pass to `SignOptions.TTL`).
- Keys are at least 32 bytes; rotation is `NewSigner(newKey, oldKey...)`:
  signs with the first, accepts all.
- `SignOptions.Binding` ties a link to the principal (session or user ID): the
  handler recomputes it with `HandlerOptions.Binding`, so a forwarded link
  stops working for anyone else.
- There is **no default key**: `NewSignerFromConfig` fails when
  `FORGE_STORAGE_SIGNING_KEY` is missing, and `config` makes it mandatory in
  production whenever storage is configured.

### Handler

`NewHandler` serves `GET`/`HEAD` for a route such as
`GET /files/{key...}`. Order of work: method, **signature, expiry, binding**,
and only then the store, so an unauthenticated caller cannot tell whether a key
exists. Responses:

- `Content-Type` is the stored, normalized type; `X-Content-Type-Options:
  nosniff`; `Content-Security-Policy: default-src 'none'; sandbox;
  frame-ancestors 'none'`; `Cache-Control: private, max-age=<remaining link life,
  at most 1h>`; `ETag` from the stored digest.
- **Inline only for raster images** (PNG, JPEG, GIF, WebP, AVIF, BMP, ICO) and
  only when the link asked for it. HTML, SVG, XML, scripts, PDF, plain text and
  everything unknown are `Content-Disposition: attachment; filename=...`
  (sanitized, RFC 6266 encoded by `mime.FormatMediaType`). Applications can
  extend the inline set with types they know are inert.
- Range and conditional requests via `http.ServeContent` for seekable readers.

### Uploads

`Receive(w, r, store, UploadOptions)` is deliberately strict:

- request `Content-Type` must be `multipart/form-data` (415 otherwise);
- the body is capped (`MaxFiles x MaxFileBytes + MaxFields x MaxFieldBytes +
  64 KiB`) before reading, both from `Content-Length` and with
  `http.MaxBytesReader`; **files are streamed** (only a 512-byte sniffing window
  of each is held) and each has its own `MaxBytes` in `Put`; **text fields are
  held in memory** but bounded: at most `MaxFields` values in total (every
  repetition of a name counts) and at most `MaxFields x MaxFieldBytes` bytes
  (default 32 values, 2 MiB);
- one expected file field; files in other fields, extra files, empty files,
  `Content-Transfer-Encoding` other than identity, unnamed parts and surplus or
  oversized text fields are rejected (400/413);
- `AllowedTypes` is required and has no wildcard. The decision uses the type
  **detected from the bytes** (`http.DetectContentType`): a file whose content
  is recognizably an image, PDF, archive, HTML or XML must be one of the
  allowed types (the client's label is irrelevant, so an HTML page labelled
  `image/png` is refused); a file with no signature keeps its declared type if
  allowed, except that types that must carry a signature (images, audio, video,
  fonts, PDF, archives) and text types (which need text content) are refused
  when the content does not match;
- a rejected upload removes the files it had already stored;
- keys are UUIDv7 under an optional validated prefix; the client's file name is
  sanitized (no directories, control or bidi characters, quotes, leading dots)
  and kept only as metadata.

Limits that cannot be fixed by the library: the server's
`FORGE_HTTP_MAX_BODY_BYTES` applies to every route and can only be lowered per
request, not raised; `config` therefore refuses `FORGE_STORAGE_MAX_UPLOAD_BYTES`
above the body limit when storage is configured. Multipart bodies are not read
by `csrf.Protection` (it reads urlencoded forms): send the token in the
`X-CSRF-Token` header (a `fetch`/Stimulus submit) or verify the field returned
in `Received.Fields`.

### Configuration

`FORGE_STORAGE_ROOT`, `FORGE_STORAGE_SIGNING_KEY` (a `config.Secret`, unpadded
base64url of at least 32 bytes, never echoed in errors),
`FORGE_STORAGE_MAX_UPLOAD_BYTES` (default 512 KiB), `FORGE_STORAGE_LINK_TTL`
(default 15m). In production, with a root: an absolute path and a signing key
are required (ADR 0005).

## Consequences

- A local-disk Active Storage equivalent with the dangerous defaults closed, in
  the standard-library core.
- Files are never an XSS vector from the application origin: active content is
  always a sandboxed download.
- Signed links are capabilities with a replay window equal to their lifetime;
  short TTLs and bindings narrow it, they cannot close it (see the threat model).
- Disk storage is one host; horizontal scale needs a future object-store
  adapter, which the port allows without core changes.
- There are no variants/transformations (image resizing, previews) and no
  virus scanning hook; both are separate, dependency-heavy features.
