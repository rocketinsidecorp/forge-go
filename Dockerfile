# syntax=docker/dockerfile:1
# Image for a Forge example service (examples/basic-api by default). Generated
# applications get their own Dockerfile from `forge new`; this one mirrors it.
#
#   docker build -t forge-example .
#   docker build --build-arg EXAMPLE=basic-api -t forge-example .

# Base images are pinned by the digest of their multi-architecture index, like
# cmd/forge-proxy/Dockerfile (same images, same date: golang 1.27.2 on Alpine
# 3.24); this is the image the release workflow publishes and signs.
# Dependabot (docker, /) proposes new digests. The build stage runs on the
# builder's platform and cross-compiles for the target one, so a
# multi-platform build (--platform linux/amd64,linux/arm64) needs no emulator.

FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine@sha256:b8c9b82affbbb82a020bbcf998dad2ba9516944ac7e883524642a481a9505e9c AS build
WORKDIR /src
ENV CGO_ENABLED=0
COPY . .
ARG EXAMPLE=basic-api
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/server ./examples/${EXAMPLE}

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
WORKDIR /app
COPY --from=build /out/server /app/server
ENV FORGE_ENV=production \
    FORGE_HTTP_ADDR=0.0.0.0:8080 \
    FORGE_HTTP_TRANSPORT=trusted-proxy
EXPOSE 8080
USER nonroot:nonroot
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 CMD ["/app/server", "healthcheck"]
ENTRYPOINT ["/app/server"]
CMD ["serve"]
