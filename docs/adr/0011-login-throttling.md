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

The HTTP adapter derives the source only from `RemoteAddr`. It does not trust
`X-Forwarded-For` or similar headers until Forge has an explicit trusted-proxy
policy.

## Consequences

- Unknown accounts and real accounts follow the same throttle path.
- A 429 response includes `Retry-After`.
- Throttle keys do not expose login identifiers in the storage layer.
- The default in-memory backend is process-local and therefore not sufficient
  as the only control for horizontally scaled deployments.
- PostgreSQL deployments can share throttle state immediately without Redis.
- Expired PostgreSQL buckets must be pruned periodically; Phase 4 scheduling
  can automate that later.
- Trusted reverse-proxy address handling remains a separate explicit feature.

## Addendum (2026-10-20): per login and source counters

The two dimensions above let one source lock a known login out for everybody
(five wrong attempts from anywhere exhausted the login's counter), and a full
memory table refused every new attempt. The policy is now three counters per
attempt, each with its own limit in the 15-minute window:

1. the canonical login and the source together: 5 attempts (`PairLimit`);
2. the canonical login across sources: 50 attempts (`AccountLimit`);
3. the source across logins: 300 attempts (`SourceLimit`).

The tenant is no longer part of the account key, so rotating it does not
reset a count; the login is canonical ASCII (`auth.CanonicalLogin`, ADR
0026), so neither letter case nor Unicode spelling does. IPv6 sources count
per `/64`; IPv4-mapped addresses count as IPv4. Refused attempts are not
counted. Success clears the pair and login counters, not the source's. A zero
`PairLimit` keeps the previous two-counter behavior. When the memory table is
full it evicts the oldest counters that block nothing and refuses only if all
block; the PostgreSQL throttler (scope 3, migration
`20261020120000_login_throttle_pair_scope`) locks counters in a fixed order,
rolls refused attempts back and prunes a few expired counters on every
attempt, replacing the "prune periodically" requirement above.
