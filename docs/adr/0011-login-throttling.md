# ADR 0011: Login throttling by account and source

Status: accepted (Phase 3)

## Decision

Forge rate-limits password login on two independent dimensions:

1. the normalized tenant + email identity;
2. the network source when one is available.

The core derives SHA-256 keys before passing them to a throttler, so throttle
stores do not need plaintext email addresses or IP addresses.

`LoginService` uses a bounded in-memory throttler by default. Its default
policy is five attempts per account and 300 attempts per source in a 15-minute
window, with at most 20,000 in-memory buckets. This protects a single process
without introducing an external dependency.

Applications with multiple replicas should replace the default through
`WithLoginThrottler` and use `auth/postgres.LoginThrottler`. The PostgreSQL
adapter stores the same hashed account/source buckets in
`forge_login_throttle`, increments counters atomically in one transaction,
and exposes `PruneExpired` for bounded cleanup work.

A successful login clears the account bucket but does not clear the source
bucket. This lets a legitimate user recover from prior failed passwords while
still limiting password-spraying attempts from one source across many
accounts.

The HTTP adapter derives the source from the socket peer by default. If
`FORGE_HTTP_TRUSTED_PROXIES` is configured, it uses only the validated client
address produced by the HTTP server's trusted-proxy middleware; malformed
forwarding data falls back to the socket peer.

## Consequences

- Unknown accounts and real accounts follow the same throttle path.
- A 429 response includes `Retry-After`.
- Throttle keys do not expose login identifiers in the storage layer.
- The default in-memory backend is process-local and therefore not sufficient
  as the only control for horizontally scaled deployments.
- PostgreSQL deployments can share throttle state immediately without Redis.
- Expired PostgreSQL buckets must be pruned periodically; Phase 4 scheduling
  can automate that later.
- Forwarded headers remain ignored unless the operator explicitly configures
  proxy CIDRs and ensures the proxy sanitizes or appends forwarding headers.
