# Credentials

Encrypted, per-environment secrets that are safe to commit: Forge's equivalent
of Rails' `credentials:edit`. Design and threat analysis:
[ADR 0036](adr/0036-encrypted-credentials-and-attribute-encryption.md) and the
[threat model](THREAT_MODEL.md#credentials-and-attribute-encryption).
For encrypting database columns see [ENCRYPTION.md](ENCRYPTION.md).

```
config/credentials/
  development.json.enc   committed, AES-256-GCM
  development.key        NOT committed, mode 0600, git- and docker-ignored
  production.json.enc    committed
  production.key         kept on the machine that edits production, never deployed
```

`forge new` creates the development pair. For another environment:

```sh
forge credentials init --environment production
```

`init` writes a random 256-bit key (`crypto/rand`) to
`config/credentials/production.key` with mode 0600 (creating
`config/credentials/` with mode 0700 when it is missing; an existing directory
keeps its mode, and one that group or others can write is refused), an
encrypted skeleton, and
adds `/config/credentials/*.key` to `.gitignore` and `config/credentials/*.key`
to `.dockerignore` (creating either file when missing). It never overwrites a key or an existing file, and it never prints
the key.

## Daily workflow

```sh
forge credentials edit                            # development (or $FORGE_ENV)
forge credentials edit --environment production   # opens $VISUAL, else $EDITOR
forge credentials show --environment production   # structure only, values redacted
forge credentials show --environment production --reveal   # terminal only
git add config/credentials/production.json.enc && git commit
```

The document is one JSON object:

```json
{
  "stripe": { "api_key": "sk_live_..." },
  "database": { "url": "postgres://app:...@db.internal:5432/app?sslmode=verify-full" },
  "encryption": { "active": "k1", "keys": { "k1": "<base64url 32 bytes>" } }
}
```

`edit` behaves as follows:

- the plaintext exists only in a file of mode 0600 in a fresh 0700 directory
  (memory-backed `/dev/shm` on Linux), which is overwritten and removed on every
  exit path: saving, an editor error, invalid JSON, a failed write, `SIGTERM`,
  `SIGHUP`. `SIGKILL` or a power loss can leave it behind; on Linux it is in
  memory, elsewhere look for `forge-credentials-*` in your temporary directory;
- the editor command is split on spaces and run without a shell
  (`EDITOR="code --wait"` works, quoting and pipelines do not). The editor does
  not inherit any `FORGE_*` variable, so `FORGE_MASTER_KEY` and the other
  secrets stay out of its plugins and subprocesses. Vim-family editors are
  recognized by name and by what the name resolves to (`vi`, `editor`,
  `vim.basic`, `vim.nox`, `gvim`, `nvim`, `view`, symlinks to them) and started
  without swap, backup, undo and viminfo files. Other editors (a busybox `vi`,
  nano, Emacs, GUI editors) are started unchanged and may keep their own
  copies, in their own directories; check their backup and history settings.
  Those copies, and the plaintext file itself after `SIGQUIT` (`Ctrl-\`,
  also when typed inside the editor; Go's default for it ends the process
  with a goroutine dump, without any cleanup), are outside Forge's reach:
  after an abnormal exit look in `/dev/shm` (Linux) or your temporary
  directory for `forge-credentials-*`, and in the editor's backup directory;
- termination (`SIGTERM`, `SIGHUP`) is caught from the start of the command, not
  only while the editor runs, and acted on at checkpoints, so the cleanup also
  covers validation and encryption;
- invalid JSON, a duplicate key (at any depth) or a document that is not an
  object is rejected, your edits are discarded and the file is untouched; the
  error gives a line and column, never the content;
- if the encrypted file changed while the editor was open (a merge, another
  person), nothing is overwritten;
- unchanged content does not rewrite the file, so no spurious diff.

`show --reveal` refuses to run when standard output is not a terminal, so values
do not end up in pipes, CI logs or files.

## Reading credentials in the application

```go
creds, err := credentials.Open(string(cfg.Environment), credentials.Options{
	Required: []string{"stripe.api_key", "encryption"}, // enforced outside development and test
})
if err != nil {
	return err // missing file, missing key, wrong key, missing credential: fail at startup
}
stripeKey, err := creds.Secret("stripe", "api_key") // config.Secret: redacted when printed
if err != nil { return err }
client := stripe.New(stripeKey.Reveal())

var db struct{ URL string `json:"url"` }
err = creds.Decode(&db, "database")

ring, err := creds.KeyRing("encryption")       // for encryption.NewCipher
```

`credentials.Load(env)` is `Open` with default options. Rules:

| Situation | development, test | any other environment (`production`, `staging`, ...) |
| --------- | ----------------- | ---------------------------------------------------- |
| no file | empty `Store`, `Exists()` is false | error `ErrNotFound` |
| file, no key | error `ErrKeyMissing` | error `ErrKeyMissing` |
| file, wrong key or modified file or another environment's file | error `ErrDecrypt` / `ErrNoMatchingKey` | same |
| `Options.Required` path missing or empty | ignored | error naming every missing path |

A `Store` prints as `credentials.Store(production, [REDACTED])` through `fmt`,
`slog` and JSON. `String` and `Secret` errors name the path, never the value.
`config.Load` does not read credentials: call `credentials.Open` in your
composition root (bootstrap) and pass what the components need.

To ship the encrypted file inside the binary instead of copying it into the
image:

```go
//go:embed config/credentials/*.enc
var credentialFiles embed.FS

creds, err := credentials.Open("production", credentials.Options{FS: credentialFiles})
```

The key must then come from `FORGE_MASTER_KEY`; `*.key` files are never embedded.
Otherwise copy `config/credentials/production.json.enc` into the image (the
generated Dockerfile copies only the binary) at the same relative path to the
working directory.

## Deploying

The server needs the key and nothing else of the credentials system:

```sh
# the key stays on your machine and in your secret store
cat config/credentials/production.key          # copy into the secret store
docker run --rm -p 8080:8080 \
  -e FORGE_ENV=production \
  -e FORGE_MASTER_KEY="$(cat config/credentials/production.key)" \
  myapp
```

`FORGE_MASTER_KEY` is canonical unpadded base64url of 32 bytes, optionally
several separated by commas (first one active). It wins over a key file. A key
file must be a regular file (not a FIFO or device) of mode 0600 or stricter,
in a directory that group and others cannot write; anything else is refused
with the fix in the message:

```sh
chmod 700 config/credentials
chmod 600 config/credentials/*.key
```

A fresh clone made with umask 002 has a group-writable `config/credentials/`;
run the first command once.

In CI, store the key as a secret and expose it only to the job that needs it
(`FORGE_MASTER_KEY: ${{ secrets.FORGE_MASTER_KEY_PRODUCTION }}`). Tests and
builds do not need production credentials; a `development` or `test` key is
enough.

**Relation to deploy secrets.** The deploy tooling (`forge deploy`, being added
separately) keeps its own, local and git-ignored secrets file
(`.forge/secrets`) for values the servers receive as environment variables. Treat the two as different layers: credentials hold the
application's secrets (API keys, database URLs, data-encryption keys) in the
repository, encrypted; the deploy secrets file or your CI secret store holds the
one value that unlocks them, `FORGE_MASTER_KEY`, plus any variable you prefer
not to commit. Do not copy the contents of credentials into the deploy file.

## Rotating the key

Rotate when someone who had the key leaves, when a key may have leaked, or on a
schedule.

```sh
forge credentials rotate-key --environment production --keep-old --print-key
```

It decrypts the file, generates a new key, re-encrypts, and writes the new key
list to `production.key` (and prints it, for a secret manager; with
`FORGE_MASTER_KEY` as the source there is no key file, so `--print-key` or
`--write-key-file` is required, and nothing is re-encrypted before the new key
has a destination). A safe sequence without downtime:

1. `rotate-key --keep-old --print-key`; set `FORGE_MASTER_KEY` in the secret
   store to the printed `new,old` list. Deployments with the old file keep
   working (old key present), deployments with the new file work (new key
   present).
2. Commit and deploy the re-encrypted `production.json.enc`.
3. When no deployment or branch uses the old file, drop the old key: set
   `FORGE_MASTER_KEY` to the new key alone and delete the second line of
   `production.key`.

A key list holds at most 8 keys (`credentials.MaxKeys`). With a full list,
`--keep-old` is refused before anything is printed or written; drop old keys
first. Without `--keep-old` a full list rotates normally.

Without `--keep-old` the old key is discarded at once, which is right after a
leak once the new file is deployed. Rotating the master key does not change the
values inside; after a leak also rotate the secrets themselves (`edit`).

The data-encryption keys inside the document (`encryption.keys`) rotate
separately: [ENCRYPTION.md](ENCRYPTION.md#rotating-keys).

## Losing a key

If every copy of `production.key` and every `FORGE_MASTER_KEY` is gone, the
file cannot be decrypted. There is no recovery path by design. Keep the key in a
password manager or secret store, and store the key list used by each running
deployment. To start over: delete `production.json.enc` and run `init` again,
then re-enter the secrets. Anything encrypted under data-encryption keys that
lived only in that file is lost with it; back those up independently if the
data matters.

## Not covered

- Secrets are decrypted into process memory at startup and stay there; Go gives
  no reliable way to wipe them (the garbage collector moves and copies values,
  and strings are immutable), so Forge-Go does not try beyond clearing a few
  byte slices it owns; Forge-Rust zeroizes its own copies. Core dumps and swap
  can contain them, including the memory of `forge credentials edit` while
  the editor runs.
- Whoever can run the application with the key can read every credential.
  Credentials separate secrets from the repository and from other environments;
  they are not an access-control system.
- An older but valid `production.json.enc` (for example from git history)
  decrypts under the same key. Rotate the key if you must invalidate it.
