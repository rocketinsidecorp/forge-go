# ADR 0037: Trusted proxies and the client address

Status: accepted (2026-10-20). Refines [ADR 0011](0011-login-throttling.md)
and [ADR 0031](0031-rate-limiting.md); builds on forge-proxy
([ADR 0030](0030-forge-deploy.md)). Fixes forge-go#11.

## Context

Login throttles (ADR 0011), recovery throttles (ADR 0013), rate limits (ADR
0031), the anonymous per-client stream cap (ADR 0029) and security audit
source digests (ADR 0016) all count or record "the client's address". Until now that
was always the TCP peer, `RemoteAddr`, because forwarding headers are
client-controlled unless a proxy you trust overwrites them.

Since `forge deploy` (ADR 0030), forge-proxy is the default production path,
and the peer of every request is the proxy container. Every client therefore
shared one source:

- 300 sign-in attempts from anyone in 15 minutes refused every sign-in;
- 30 password-reset requests from anyone in an hour refused every reset;
- the 5-attempt limit per login *and source*, which ADR 0011's addendum relies
  on so one attacker cannot lock a login out for everybody, became a limit per
  login from anywhere;
- `ratelimit.ByRemoteAddr` put everyone in one bucket.

## Decision

### One resolver, one header, fail closed

`web.TrustedProxies` resolves the client address once per request; the HTTP
server stores it in the request context and `web.ClientIP(r)` returns it. Every
framework consumer reads it: `auth` throttles and audit source digests
(sign-in, MFA, recovery; HTML and JSON adapters), `ratelimit.ByClientIP`, and
the broadcast per-client stream cap. Without the resolver (a handler used outside
`httpserver`) `web.ClientIP` is the peer, which is the same as trusting nobody.

Configuration is `FORGE_HTTP_TRUSTED_PROXIES` (`config.HTTP.TrustedProxies`):
a comma-separated list of CIDR prefixes or bare addresses (a bare address is a
`/32` or `/128`). **The default is empty: trust nobody, use the peer.** The
list is validated at startup; the process refuses to start on a malformed or
empty entry, an overly broad prefix (an IPv4 prefix shorter than `/8` or an
IPv6 prefix shorter than `/16`, `/0` included: they would trust most of the
internet and let almost any client choose its address; two `/1` prefixes
cover all of IPv4), an IPv4-mapped IPv6 prefix (peers are compared in IPv4
form, so it would silently never match) or a zoned address. Bare addresses
are unaffected. Errors name the entry's position, never its text. One
function, `config.CheckTrustedProxy`, holds these rules; `web.NewTrustedProxies`
applies it too.

Resolution, given the peer `P` and the trusted set `T`:

1. If `P` is not in `T`, the client is `P`. Every forwarding header is
   ignored.
2. Otherwise read `X-Forwarded-For` (all header lines, in order, as one list)
   **from the right**. Skip entries in `T`. The first entry not in `T` is the
   client: everything to its left was written by that client or by proxies
   nobody vouches for, and is never read.
3. If an entry reached on that walk is empty or not a bare IP address (a port,
   brackets, `for=` syntax, garbage), the client is `P`: a malformed chain
   never resolves to a value the client may have chosen.
4. If every entry is trusted, the client is the leftmost entry; with no header
   it is `P`. More than 32 trusted entries are not a real deployment; the
   client is `P`.

Addresses are canonical: zone dropped, IPv4-mapped IPv6 as IPv4. The
resolver returns the full address; counters bucket IPv6 by `/64` as before
(`auth` throttles, `ratelimit.ByClientIP`, the stream cap), so a client
rotating through its `/64` still gets no fresh counter.

`Forwarded` (RFC 7239) and `X-Real-IP` are never read. One header keeps the
rule simple to audit; forge-proxy, nginx, Caddy, Traefik and the common cloud
load balancers all set `X-Forwarded-For`. `X-Forwarded-Proto` is still not
interpreted (HSTS and redirects are unchanged); that is a separate decision.

`ratelimit.ByClientIP` is the documented per-client key. `ByRemoteAddr` keeps
its exact meaning (the peer, whatever the policy) for callers that want it.

### forge-proxy is the edge and replaces the header

forge-proxy faces the internet. It uses `httputil.ReverseProxy.Rewrite`, which
drops inbound `Forwarded` and `X-Forwarded-*` headers, then sets
`X-Forwarded-For` to the connecting address alone; it now also drops
`X-Real-IP`. A client therefore cannot plant entries in the chain the
application reads. Putting forge-proxy behind another load balancer (which
would need it to append instead) is not supported; there the operator lists
both networks and runs a proxy that appends, or terminates at forge-proxy.

### `forge deploy` configures the application

The proxied role's containers get
`FORGE_HTTP_TRUSTED_PROXIES` set to the subnets of the `forge` Docker network,
read on each host when the container is created:

```sh
forge_proxies=$(docker network inspect --format '{{range $i, $c := .IPAM.Config}}{{if $i}},{{end}}{{$c.Subnet}}{{end}}' forge) || exit 1
[ -n "$forge_proxies" ] || { echo "...has no subnet..." >&2; exit 1; }
docker run ... --env-file .forge/<service>/env/web.env --env "FORGE_HTTP_TRUSTED_PROXIES=$forge_proxies" ...
```

A network without a subnet fails the deploy instead of silently reverting to
one shared source. Setting `FORGE_HTTP_TRUSTED_PROXIES` in `config/deploy.yml`
(`env.clear`, the role's `env.clear`, or a secret) disables the automatic
value; an empty value trusts nobody. Roles without the proxy are not
configured.

The whole network is trusted rather than the proxy container's address,
because that address changes when the proxy is recreated, and a stale `/32`
could later belong to another container. Application containers publish no
ports (ADR 0030), so only containers on the `forge` network can reach them.

## Consequences

- Behind forge-proxy each client has its own sign-in, recovery, rate-limit
  and stream counters, and the source digests in security audit events
  identify the client instead of the proxy.
- Without configuration nothing changes: the peer is the client, as before.
- **Residual:** any container on the `forge` network (accessories, other
  services deployed to the same hosts, a compromised dependency in one of
  them) can send requests with a chosen `X-Forwarded-For` straight to the
  application and pick its throttle key. That gives it what it already has by
  being inside the network, plus the ability to spend other clients'
  per-source budgets; operators who share hosts between untrusted services
  should set an explicit, narrower value.
- An application that is reachable both through the proxy and directly from
  a trusted network trusts that network's headers; list only proxy networks.
- A misconfigured, too-broad list re-opens spoofing. Configuration is the
  operator's; the minimum prefix lengths (`/8`, `/16`) only stop the obvious
  mistakes, and a `/8` still trusts 16 million addresses.
- **Residual:** the Docker subnet `forge deploy` trusts includes the
  network's gateway (for example `172.18.0.1`), so processes on the host reach
  the application from a trusted address and can choose their `X-Forwarded-For`.
  The host is root-equivalent anyway, so this adds no capability.
- **Residual:** on a Docker network without IPv6, IPv6 clients reach
  forge-proxy through docker-proxy, which connects from the gateway address,
  so all IPv6 clients share one bucket (the same as before this ADR, not a
  regression). Enable IPv6 on the `forge` network or run the Docker daemon
  with `"userland-proxy": false`.
- A successful sign-in still does not clear the source counter (ADR 0011):
  one client's counter now covers that client only, so this no longer lets
  successful sign-ins of others exhaust a shared budget.

## Forge-Rust port

Not implemented yet; tracked in
[forge-rust#12](https://github.com/rocketinsidecorp/forge-rust/issues/12).
Forge-Rust is to implement the same behavior with the same names: environment
key `FORGE_HTTP_TRUSTED_PROXIES`, empty default, the same validation rules
(including the minimum prefix lengths) and error shape, the same right-to-left
`X-Forwarded-For` walk with fall-back to the peer, the same `/64` bucketing, a
request-scoped resolved client address read by throttles, rate limits and
audit, forge-proxy replacing the header, and `forge deploy` setting the
variable from the `forge` network's subnets.
