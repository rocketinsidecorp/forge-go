# ADR 0030: `forge deploy` and forge-proxy

- **Status:** Accepted
- **Date:** 2026-10-19

## Context

Generated applications ship a production image, health endpoints, a
`migrate` subcommand and graceful shutdown (ADRs 0003, 0005, 0008), but
getting that image onto servers was left to the operator. Rails 8 answers
this with Kamal: `config/deploy.yml`, `kamal setup|deploy|rollback`, and
kamal-proxy for zero-downtime switches. Kamal's weak spots are the ones we
want to close:

- Secrets reach containers through env files, but `.kamal/secrets` allows
  arbitrary `$(command)` substitution, and dry runs are limited.
- A failed health check on one host leaves other hosts on the new version.
- The proxy is a separate project and image the operator cannot review in
  the framework repository.
- Lock bypass, interrupted deploys and partially reverted states are not
  reported through stable exit codes.

The `forge` CLI and core packages are stdlib-only (ADR 0006). The standard
library has no YAML parser and no ACME client.

## Decision

`forge deploy`, `forge logs` and `forge app` (package `internal/deploy`) and
a reverse proxy, `forge-proxy` (`internal/proxy`, `cmd/forge-proxy`, with its
own distroless image), ship in the framework repository.

**Transport.** Every remote action is a POSIX shell command run with the
system `ssh` client (`sh -c` regardless of the login shell), so
`~/.ssh/config`, the agent, `known_hosts` and `ProxyJump` apply. Host keys are
always verified; `BatchMode=yes` prevents password prompts in non-interactive
commands. One `ControlMaster` connection per host is shared by all commands
of an invocation and closed at the end. Commands are built from validated
configuration and quoted words only; secrets travel on stdin. A narrow
`Runner` interface (Run, Stream, Interactive) is the test seam.

**Configuration.** `config/deploy.yml` is decoded strictly: unknown and
duplicate keys, several documents, non-string durations, tags or digests in
`image`, inline registry passwords, option-like hosts and more than one
proxied role are errors. Defaults are applied once in `ParseConfig`.

**Secrets.** `.forge/secrets` holds `NAME=value` lines or `NAME=$VARIABLE`
passthrough from the local environment. There is no command substitution.
The file must be mode 0600 (load fails otherwise); `forge deploy init`
creates it so and adds it to `.gitignore` and `.forge` to `.dockerignore`.
Containers get secrets through `~/.forge/SERVICE/env/ROLE.env`, written with
`umask 077` from stdin and renamed atomically, never through argv. All
progress, dry-run and log output passes through a redactor for every secret
value of four or more characters.

**Deploy algorithm.** Lock (an atomic `mkdir` on every host, with holder and
message; released only while the record is still the deploy's own, and a lock without a record file older than a minute is taken over in place) → optional `pre-build` hook, local `docker build` and `push` →
`pre-deploy` hook → in parallel (bounded by `parallelism`): registry login,
`pull`, env files → `migrate` once in a one-off container on the first host
of the migrate role (failure aborts with exit 5 before any container
starts) → inventory → per host, primary role first: `docker run` the new
container with labels; for the proxied role, `forge-proxy deploy` health-gates
it (N consecutive passes) and switches atomically, draining the old target;
other roles wait for Docker health or a steady running state → only when
every host succeeded: stop previous containers (`docker stop -t
stop_timeout`, SIGTERM then wait), prune stopped containers beyond `retain`
and their images, append a JSON entry to `~/.forge/SERVICE/deploys.log`,
`post-deploy` hook → release the lock (also on failure).

If any host fails, every host that switched is routed back to its previous
container (still running) and the new containers are stopped and removed:
the deploy is all-or-nothing (exit 4). A role with no previous container
keeps its healthy new one, since there is nothing to return to. A failed
revert exits 7. `rollback VERSION` restarts the retained containers of that
version through the same switch, with the environment they were created
with; migrations are not reverted.

**forge-proxy.** `net/http/httputil.ReverseProxy` with `Rewrite`, so inbound
`Forwarded`/`X-Forwarded-*` headers are dropped and set from the connection.
Routing by Host to one target per service; targets are swapped with an
atomic pointer and a per-target in-flight count, so a request never reaches a
retired target and none is dropped during a switch. Control is a Unix socket
(directory 0700, restricted at start if it exists, socket 0600) inside the
container, reached with `docker exec`; there is no network control surface.
State is persisted atomically (file and directory synced) to a volume so a
restarted proxy resumes routing. Request bodies are capped (413) and small
bodies are buffered with a read deadline before forwarding. `-idle-timeout`
bounds keep-alive waits, each write to a client and silence on an upgraded
connection, so a client that stops reading cannot hold a target open.
Maintenance mode serves a static 503 page. Access logs are JSON and omit the
query string. TLS certificates come from Let's Encrypt through
`autocert`, only for hosts registered with `tls`; its cache directory is
kept 0700 and its writes are synced.

**Exit codes** (also in `forge help --json`): 0 success, 1 failure, 3 lock
held, 4 health check failed and reverted, 5 migration failed, 6 hook failed,
7 inconsistent after a failed revert.

**Dependencies admitted.** Each is used only outside the core packages, so
`architecture_test.go` still holds and applications that do not import them
do not link them:

| Module | Used by | Why | Review |
| ------ | ------- | --- | ------ |
| `go.yaml.in/yaml/v3` v3.0.5 | `internal/deploy` (the `forge` CLI binary) | `deploy.yml` stays YAML for Kamal familiarity; strict decoding (`KnownFields`, duplicate-key errors) is available. | Maintained by the YAML organization as the continuation of `gopkg.in/yaml.v3`; no dependencies of its own; Apache-2.0/MIT. |
| `golang.org/x/crypto` v0.58.0 (`acme`, `acme/autocert`) | `internal/proxy` (forge-proxy binary only) | Automatic certificates are what makes `ssl: true` safe by default; implementing ACME ourselves would be custom security code. | Go team; BSD-3-Clause; covered by govulncheck. |
| `golang.org/x/net` v0.60.0 (transitively, `idna`) | `autocert` | Host name normalization. | Go team; BSD-3-Clause. |

`x/crypto` raised the already-indirect `golang.org/x/sys`, `x/text` and
`x/sync` to their current minor versions.

## Consequences

- Deploys need only ssh and Docker on the hosts; the docker CLI is needed
  locally only when building.
- `--dry-run` prints every mutating remote and local command with stdin
  described but not shown; read-only queries (`docker ps`, image list) still
  run so the plan names the exact containers that would be stopped or pruned.
- The orchestration is integration-tested over real `sshd` with a docker shim
  (`internal/deploy/internal/fakedocker`); the docker CLI semantics the shim
  models (flags, `--format '{{json .}}'` fields) are an assumption verified
  only against Docker documentation, not a daemon, in CI.
- The proxy image is built from this repository by the operator and should be
  referenced by digest; `forge deploy setup` warns otherwise. Changing it
  requires removing the `forge-proxy` container and running setup again
  (routing state survives in the volume; requests fail while it restarts).
- Remote state lives in `~/.forge/SERVICE` of the SSH user, which must be
  able to run `docker` (that is root-equivalent on the host).
- Only one role per service can be proxied, and certificates come only from
  ACME; static certificate files are not supported yet.
