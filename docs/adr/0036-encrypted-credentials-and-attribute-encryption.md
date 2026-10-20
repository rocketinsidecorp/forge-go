# ADR 0036: Encrypted credentials and attribute encryption

- **Status:** Accepted
- **Date:** 2026-10-20

## Context

Rails keeps per-environment secrets in `config/credentials/*.yml.enc`, edited
with `credentials:edit`, and encrypts model attributes with Active Record
Encryption. Both are good ideas with known weak spots: the master key is one
environment variable shared by everything, the edit flow leaves plaintext in
temporary files and editor swap files, an encrypted file from one environment
can be dropped into another, deterministic encryption is a documented option
that leaks equality, and ciphertexts are not tied to the row they belong to.

Forge already encrypts MFA seeds with AES-256-GCM (ADR 0014,
`auth.AESGCMSecretCipher`) with a single key and no key id. The core is
standard library only (ADR 0006), configuration is environment-driven and fails
fast (ADR 0005), and `config.Secret` redacts values.

## Decision

### Package layout

| Package | Role |
|---------|------|
| `encryption` (core, stdlib only) | AES-256-GCM, HKDF-SHA256 key derivation, key rings, `Cipher`, blind index, `Value[T]`, batch re-encryption |
| `credentials` (core, stdlib only) | encrypted credentials file format, key loading, typed `Store` |
| `postgres.EncryptedColumn` | pgx `encryption.Rewriter`; `encryption.SQLRewriter` serves database/sql |
| `forge credentials` | `init`, `edit`, `show`, `rotate-key` |

`auth.AESGCMSecretCipher` now builds on `encryption.NewAEAD`,
`SealPrefixed` and `OpenPrefixed`. Its stored format (nonce, ciphertext, tag, no
associated data) and API are unchanged, and a test pins the format, so existing
MFA rows stay readable. Applications that need rotation for MFA seeds can
implement `auth.SecretCipher` on `encryption.Cipher`.

### Credentials file

`config/credentials/<env>.json.enc` is a small JSON envelope:

```json
{
  "version": 1,
  "algorithm": "AES-256-GCM",
  "key_id": "9f2c41aa07d3b6e1",
  "nonce": "<base64url, 12 bytes>",
  "ciphertext": "<base64url, includes the 16-byte tag>"
}
```

- The AEAD key is derived from the master key with HKDF-SHA256 and the purpose
  `credentials/aead`. `key_id` is the first 8 bytes of another HKDF output
  (`credentials/key-id`), a fingerprint that names the key without revealing it.
- Associated data is the length-prefixed tuple `("forge/credentials", version,
  environment, key_id)`. The environment comes from the caller, not the file,
  so a staging file installed as `production.json.enc` fails authentication.
  Version and key id are covered too, which blocks downgrade and relabeling.
- The plaintext is one JSON object, at most 1 MiB, with no duplicate keys at
  any depth (parsers disagree on which duplicate wins).
- The master key is `FORGE_MASTER_KEY` (wins) or `config/credentials/<env>.key`
  (mode 0600 enforced when read, `os.Root` so a symlink cannot leave the
  directory). Either may hold several comma- or line-separated keys: the first
  is active, the rest only decrypt, selected by `key_id`.
- Outside `development` and `test`, a missing file, a missing key or a missing
  `Options.Required` path stops startup. In every environment an existing file
  that does not decrypt is an error: credentials fail closed.

JSON, not YAML, because the standard library parses it and a strict parser is
short; the encrypted file is opaque anyway.

### Editing

`forge credentials edit` decrypts into a file of mode 0600 inside a fresh 0700
directory (`/dev/shm` on Linux, so it is memory-backed, otherwise the system
temporary directory), runs `$VISUAL` or `$EDITOR` without a shell, validates
the JSON, refuses to overwrite a file that changed while the editor was open,
and replaces the encrypted file atomically (temporary file, `fsync`, `rename`).
The whole directory, including editor swap files, is overwritten with zeros and
removed on success, editor failure, invalid input, write failure, `SIGTERM` and
`SIGHUP`. Vim and Neovim are started without swap, backup, undo and viminfo
files. `SIGKILL` and power loss can leave the directory behind; this is
documented.

`show` redacts every value unless `--reveal` is given, and `--reveal` refuses to
run unless standard output is a terminal, so secrets do not reach pipes, CI
logs or redirected files by accident.

`rotate-key` generates a new key, and re-encrypts only if the new key has a
destination (the key file, or `--print-key` for a secret manager). The key file
first gains the new key beside the old ones, then the encrypted file changes,
then the old keys are dropped unless `--keep-old` was given, so the pair on disk
is readable at every instant.

### Attribute encryption

- Each master key in a ring yields two unrelated subkeys with HKDF: one for
  AES-256-GCM, one for the blind index. One master key can therefore live in
  credentials (`Store.KeyRing`) without key reuse across primitives.
- A ciphertext is text: `fg1.<key id>.<base64url(nonce || ciphertext || tag)>`,
  usable in any TEXT column. Nonces are random 96-bit values; encryption is
  always randomized.
- Associated data is `("forge/attribute/v1", key id, table, column, record
  id)`. Moving a value to another row, column or table, or renaming its key
  id, fails with `ErrDecrypt`. Record ids are UUIDv7 in generated applications.
- Equality lookups use a blind index, `HMAC-SHA256(indexKey, ("forge/blind-index/v1",
  table, column) || plaintext)`, opt-in per column and stored in a separate
  column. There is no deterministic encryption mode.
- The ring has one active key and any number of previous keys. `Decrypt` selects
  by key id; `BlindIndexes` returns one digest per key for lookups during a
  rotation.
- `Cipher.Reencrypt` rotates a column in bounded pages through a `Rewriter`,
  applies each change as a compare-and-set on the old ciphertext (a concurrent
  application write is never overwritten), stops on the first undecryptable row,
  and returns a `LastID` checkpoint so an interrupted run resumes.
- `encryption.Value[T]` implements `sql.Scanner` and `driver.Valuer`. It needs
  `ForRecord(id)` before use: a scanned value is decrypted lazily by `Get`,
  because the row id is usually scanned in the same call.

### Not decided here

`forge generate scaffold ... ssn:string:encrypted` is not part of this change:
it touches the domain, persistence and view templates of three generators and
needs its own review. See the roadmap.

## Alternatives considered

- **One key for everything, no key ids (the existing MFA cipher).** Cannot
  rotate. Kept for MFA seeds for compatibility, built on the same primitives.
- **A key committed in the repository.** Defeats the purpose.
- **Deterministic AES (Rails' `deterministic: true`).** Equal plaintexts give
  equal ciphertexts, which reveals equality, frequency and set membership to
  anyone who reads the table, and tempts people to use it for low-entropy values.
  The blind index gives lookups with a separate, optional, keyed column.
- **Per-record data keys / envelope encryption through a KMS.** Valuable, but it
  needs provider SDKs; `encryption.Cipher` can be replaced by an adapter that
  does it. Out of scope for the standard-library core.
- **Dedicated key-derivation salts stored per value.** Random nonces under a
  derived subkey are sufficient at the documented volume limit (2^32 values per
  key, see ENCRYPTION.md); rotation resets it.

## Consequences

- A repository can hold production secrets safely: an attacker with the repo
  but not the key learns file sizes and key ids.
- Losing every copy of a key makes the file, and anything encrypted under keys
  stored in it, unrecoverable. This is intended and documented; back keys up.
- A key in `FORGE_MASTER_KEY` is as exposed as any environment variable (process
  listings of the container runtime, crash dumps, child processes). Deployments
  that cannot accept this mount a key file from a secret store instead.
- Application code that reads credentials depends on `credentials.Open`;
  `config.Load` does not read them, to keep `config` free of the credential
  machinery and key handling.
- Go cannot reliably zero memory. Keys and plaintexts are cleared on a best
  effort basis and may remain in memory, swap and core dumps.
