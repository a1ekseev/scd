# syntax=docker/dockerfile:1
# Single image ghcr.io/a1ekseev/scd for all three services (D10):
#   scd daemon …        (default CMD)
#   scd sfc serve …     (compose `command:`)
#   scd gtl serve …     (compose `command:`)
# Cross-compiles for $TARGETARCH on the build platform (no QEMU).
FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/scd ./cmd/scd
# The sfc source cache dir, owned by the runtime user: a named volume mounted
# there copies this ownership on first use.
RUN mkdir -p /out/app/config/.sfc-cache

# gcr.io/distroless/static-debian13:nonroot (CA certificates, tzdata, /etc/passwd;
# no shell; user nonroot 65532). No service needs root: all listen on ports
# >= 1024 and only sfc writes, to /app/config/.sfc-cache.
FROM gcr.io/distroless/static-debian13@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99
COPY --from=build /out/scd /usr/local/bin/scd
COPY --from=build --chown=65532:65532 /out/app /app
WORKDIR /app
USER 65532:65532
CMD ["scd", "daemon", "--config", "/app/config/config.yml"]
