# syntax=docker/dockerfile:1
# forge-proxy image. Build from the repository root:
#
#   docker build -f cmd/forge-proxy/Dockerfile -t registry.example.com/forge-proxy:0.1.0 .
#
# Push it to a registry the deploy hosts can pull from and reference it by
# digest in config/deploy.yml (proxy.image). See docs/DEPLOYMENT.md.

# Base images are pinned by the digest of their multi-architecture index,
# resolved from the tags shown on 2026-10-20 (golang 1.27.2 on Alpine 3.24).
# Dependabot (docker, /cmd/forge-proxy) proposes new digests; to refresh by
# hand, read "Digest:" from `docker buildx imagetools inspect <image:tag>`.

FROM golang:1.27-alpine@sha256:f92b6ef800e499660581efdabdf25d9d817a9d124eaf900924f0504e7e27e12d AS build
WORKDIR /src
ENV CGO_ENABLED=0
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags="-s -w" -o /out/forge-proxy ./cmd/forge-proxy
# Directories the nonroot user (65532) owns: routing state and ACME cache
# (mount a volume at /var/lib/forge-proxy) and the control socket.
RUN mkdir -p /out/var/lib/forge-proxy/acme /out/run/forge-proxy && \
    chmod 0700 /out/var/lib/forge-proxy /out/var/lib/forge-proxy/acme /out/run/forge-proxy

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/forge-proxy /usr/local/bin/forge-proxy
COPY --from=build --chown=65532:65532 /out/var/lib/forge-proxy /var/lib/forge-proxy
COPY --from=build --chown=65532:65532 /out/run/forge-proxy /run/forge-proxy
USER nonroot:nonroot
# Unprivileged ports; publish them as 80 and 443 on the host.
EXPOSE 8080 8443
VOLUME ["/var/lib/forge-proxy"]
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 CMD ["/usr/local/bin/forge-proxy", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/forge-proxy"]
CMD ["run"]
