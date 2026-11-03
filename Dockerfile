# Jevlar container image.
#
# Build:  docker build -t jevlar:dev .
# Run:    docker run --rm -p 8080:8080 -v "$PWD/configs:/etc/jevlar:ro" \
#           --env-file .env jevlar:dev
#
# The runtime stage is distroless and runs as a non-root user. The config
# and any secret files are mounted read-only; only the usage directory is
# writable.
#
# Run with the shipped container configuration (binds 0.0.0.0 and reads
# TYPESAFE_API_KEY / JEVLAR_CLIENT_KEY_HASH from the environment):
#   docker run --rm -p 8080:8080 \
#     -v "$PWD/configs/container.toml:/etc/jevlar/config.toml:ro" \
#     --env-file .env jevlar:dev

# --- build ---------------------------------------------------------------
# Build on the runner's native platform and cross-compile to the image's
# target, so multi-platform builds do not need QEMU.
FROM --platform=$BUILDPLATFORM golang:1.27.3 AS build

WORKDIR /src
# Cache dependencies separately from sources.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG TARGETOS=linux
ARG TARGETARCH
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
      -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/jevlar ./cmd/jevlar

# --- runtime -------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/jevlar /usr/local/bin/jevlar

# Jevlar writes rollups here when usage.enabled = true.
VOLUME ["/data"]

# The runtime image has no shell or HTTP client; the binary probes its own
# /healthz. It reads the same default config path as CMD below.
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
  CMD ["/usr/local/bin/jevlar", "healthcheck", "--config", "/etc/jevlar/config.toml"]

USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/jevlar"]
CMD ["--config", "/etc/jevlar/config.toml"]
