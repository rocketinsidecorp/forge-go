# Attribute encryption

Encrypt sensitive columns (national ids, tokens, notes) with AES-256-GCM, with
key rotation and without deterministic encryption: Forge's equivalent of Active
Record Encryption. Package `encryption`, standard library only. Design:
[ADR 0036](adr/0036-encrypted-credentials-and-attribute-encryption.md).

## Setup

`forge new` and `forge credentials init` put a key ring in the credentials:

```json
{ "encryption": { "active": "k1", "keys": { "k1": "<base64url, 32 random bytes>" } } }
```

```go
creds, _ := credentials.Open(string(cfg.Environment), credentials.Options{Required: []string{"encryption"}})
ring, err := creds.KeyRing("encryption")
if err != nil { return err }
cipher, err := encryption.NewCipher(ring)
```

Without credentials, build a ring yourself (`encryption.GenerateKey`,
`encryption.NewKeyRing`, `encryption.ParseKeyRing`). Keys are 32 bytes; their
identifiers are 1-32 letters, digits, `_` or `-` and are stored in every
ciphertext. Use names you can read in a year: `2026-10`, not `1`.

## Encrypting a column

Each value is bound to its table, column and record id. The record id should be
the primary key, known before the insert (UUIDv7 is generated in Go).

```go
ssn := encryption.NewValue[string](cipher, "people", "ssn").ForRecord(id.String())
ssn.Set("123-45-6789")
_, err = db.Exec(ctx, `INSERT INTO people (id, ssn) VALUES ($1, $2)`, id, ssn)
```

`ssn` is a `driver.Valuer`; the column is `TEXT` (the value looks like
`fg1.2026-10.<base64url>`). To read:

```go
var id uuid.UUID
ssn := encryption.NewValue[string](cipher, "people", "ssn")
err := db.QueryRow(ctx, `SELECT id, ssn FROM people WHERE id = $1`, want).Scan(&id, ssn)
plain, present, err := ssn.ForRecord(id.String()).Get()
```

`Scan` only stores the ciphertext; `Get` decrypts it for the row you name, so a
value copied to another row, column or table fails with `encryption.ErrDecrypt`.
Strings and `[]byte` are stored as they are, any other `T` as JSON. A NULL column
reads as `present == false`, and `Clear()` writes NULL. Decryption errors never
include the plaintext.

The lower-level calls are `cipher.Encrypt(plaintext, aad)` and
`cipher.Decrypt(text, aad)` with `encryption.AAD{Table, Column, RecordID}`.

Encrypted columns cannot be searched, sorted or indexed by value. Do not
encrypt a column the database must filter on unless you add a blind index.

## Searching: blind index (opt-in)

Encryption is randomized: encrypting the same value twice gives different
text. For exact-match lookups add a second column holding a keyed digest:

```sql
ALTER TABLE people ADD COLUMN ssn_idx text;
CREATE INDEX people_ssn_idx ON people (ssn_idx);
```

```go
index, _ := cipher.BlindIndex("people", "ssn", []byte(normalize(ssn)))
// INSERT ... (ssn, ssn_idx) VALUES ($1, $2)

wanted, _ := cipher.BlindIndexes("people", "ssn", []byte(normalize(input))) // one per ring key
rows, err := db.Query(ctx, `SELECT id, ssn FROM people WHERE ssn_idx = ANY($1)`, wanted)
```

`normalize` (trim, lowercase, strip punctuation) is yours: the index matches
exact bytes. The index is `HMAC-SHA256` under a key derived from the master key
for this purpose and bound to the table and column; without the key it reveals
nothing about the value, but anyone who can read the table sees which rows are
equal and how often each value occurs. Use it only for columns that need it,
prefer high-entropy values, and never for booleans or small enumerations (a
guessing attacker with the key would invert it, and the frequency distribution
alone identifies the values). There is no deterministic encryption mode.

## Rotating keys

1. Add the new key and make it active; keep the old one:

   ```sh
   forge credentials edit --environment production
   ```
   ```json
   { "encryption": { "active": "2026-10", "keys": { "2026-10": "<new>", "2025-03": "<old>" } } }
   ```

   Deploy. New writes use `2026-10`; old values still decrypt with `2025-03`.
   Lookups use `BlindIndexes`, which returns a digest per key.

2. Re-encrypt existing rows, in bounded batches, safe to interrupt and rerun:

   ```go
   rewriter, _ := postgres.EncryptedColumn(db, encryption.SQLColumn{
   	Table: "people", Column: "ssn", IndexColumn: "ssn_idx", // IndexColumn only with a blind index
   })
   report, err := cipher.Reencrypt(ctx, rewriter, encryption.RotateOptions{BatchSize: 500, BlindIndex: true})
   // on cancellation or error: report.LastID is the checkpoint
   report, err = cipher.Reencrypt(ctx, rewriter, encryption.RotateOptions{After: report.LastID, BlindIndex: true})
   ```

   MySQL, MariaDB and SQLite use `encryption.NewSQLRewriter(db, spec)`. Run it
   from a job or an application command, one column at a time. Rows already on
   the active key are skipped; a row changed by the application during the
   run is left alone (counted in `report.Conflicts`) and picked up by the next
   run. A row that cannot be decrypted stops the run with its id: investigate,
   do not skip. Primary keys must sort bytewise (UUID text does).

3. When `Reencrypt` reports `Rotated == 0` and `Conflicts == 0` on a second run,
   remove the old key from the credentials and deploy. A value still sealed with
   a removed key fails with `ErrUnknownKey`.

Rotate before about 2^32 values have been encrypted under one key (random
96-bit nonces; NIST SP 800-38D), and on any suspicion of exposure.

## Guarantees and limits

- Confidentiality and integrity per value (AES-256-GCM); the key id, table,
  column and record id are authenticated.
- Not hidden: that a value exists (NULL vs not), its approximate length, equal
  values within a blind-indexed column, the key id.
- A ciphertext is not versioned against replay: an old valid ciphertext for the
  same row and column can be restored by someone who can write the table. Use
  an updated-at/version column and optimistic locking if that matters.
- Whoever has both the database and the key reads everything. Keep the key out
  of the database host and its backups.
- `auth.AESGCMSecretCipher` (MFA seeds) keeps its own single-key format
  ([MFA.md](MFA.md)); it uses the same primitives.
