# syntax=docker/dockerfile:1
# Multi-arch image (linux/amd64, linux/arm64). Go cross-compiles, so no
# emulation is needed at build time.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
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

FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.title="syncwatch" \
      org.opencontainers.image.description="Read-only dashboard, weekly report and urgent pings for Syncthing deployments" \
      org.opencontainers.image.source="https://github.com/ivarstudios/syncwatch" \
      org.opencontainers.image.licenses="MIT"
COPY --from=build /out/syncwatch /usr/local/bin/syncwatch
COPY --from=build --chown=65532:65532 /out/data /data
# HTTPS with a self-signed certificate kept in /data by default; set
# SYNCWATCH_TLS_HOSTS to the host's LAN address so the certificate covers it.
ENV SYNCWATCH_DATA=/data \
    SYNCWATCH_LISTEN=:8080 \
    SYNCWATCH_TLS_SELF_SIGNED=true
VOLUME ["/data"]
EXPOSE 8080
USER 65532:65532
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD ["/usr/local/bin/syncwatch", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/syncwatch"]
CMD ["serve"]
