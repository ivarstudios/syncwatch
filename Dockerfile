# syntax=docker/dockerfile:1@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e
# Multi-arch image (linux/amd64, linux/arm64). Go cross-compiles, so no
# emulation is needed at build time.
#
# The base images (and the Dockerfile frontend above) are pinned by digest,
# so a tag that moves can't change what a release is built with. To update
# one, look up the digest of the tag's multi-arch index and replace it here:
#   docker buildx imagetools inspect golang:1.27-alpine   # "Digest: sha256:…"
FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:f92b6ef800e499660581efdabdf25d9d817a9d124eaf900924f0504e7e27e12d AS build
ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/syncwatch ./cmd/syncwatch \
 && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
LABEL org.opencontainers.image.title="syncwatch" \
      org.opencontainers.image.description="Read-only dashboard, weekly report and urgent pings for Syncthing deployments" \
      org.opencontainers.image.source="https://github.com/ivarstudios/syncwatch" \
      org.opencontainers.image.licenses="MIT"
COPY --from=build /out/syncwatch /usr/local/bin/syncwatch
# /data holds the database, secret.key and the TLS key: private to the
# syncwatch user, like the app itself creates it.
COPY --from=build --chown=65532:65532 --chmod=0700 /out/data /data
# HTTPS with a self-signed certificate kept in /data by default; set
# SYNCWATCH_TLS_HOSTS to the host's LAN address so the certificate covers it.
# The gate (command: gate) has its own listen address, 8385.
ENV SYNCWATCH_DATA=/data \
    SYNCWATCH_LISTEN=:8080 \
    SYNCWATCH_GATE_LISTEN=:8385 \
    SYNCWATCH_TLS_SELF_SIGNED=true
VOLUME ["/data"]
EXPOSE 8080
USER 65532:65532
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD ["/usr/local/bin/syncwatch", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/syncwatch"]
CMD ["serve"]
