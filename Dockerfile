# syntax=docker/dockerfile:1
# Single image ghcr.io/a1ekseev/scd for all three services (D10):
#   scd daemon …        (default CMD)
#   scd sfc serve …     (compose `command:`)
#   scd gtl serve …     (compose `command:`)
# Cross-compiles for $TARGETARCH on the build platform (no QEMU).
FROM --platform=$BUILDPLATFORM golang:1.27.2-alpine AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/scd ./cmd/scd

# gcr.io/distroless/static-debian13:latest (CA certificates, tzdata, /etc/passwd; no shell).
# Runs as root: the sfc cache lives in the mounted /app/config/.sfc-cache volume.
FROM gcr.io/distroless/static-debian13@sha256:58133991db06659feaabe0f4e97a35cebf15ef4ea08f8a4c6d2ee5f75e4aa6a0
COPY --from=build /out/scd /usr/local/bin/scd
WORKDIR /app
CMD ["scd", "daemon", "--config", "/app/config/config.yml"]
