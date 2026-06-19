# syntax=docker/dockerfile:1

# Build stage runs natively on the builder's arch and cross-compiles to the
# target arch (pure Go, CGO off) — so multi-arch images build fast without QEMU.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
# Let Go fetch the exact toolchain go.mod pins (the base image may lag a patch).
ENV GOTOOLCHAIN=auto
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -buildvcs=false -ldflags="-s -w" \
    -o /out/tgwebdav ./cmd/tgwebdav
# Pre-create a cache dir we can chown into the (writable) final image.
RUN mkdir -p /out/cache

# Minimal, static, non-root runtime. distroless/static ships CA certificates,
# which the Telegram Bot API (HTTPS) needs.
FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/tgwebdav /usr/local/bin/tgwebdav
COPY --from=build --chown=65532:65532 /out/cache /cache
# Blob cache lives on a writable volume; everything else is config via env/flags.
ENV TGWEBDAV_CACHE_DIR=/cache
VOLUME ["/cache"]
# WebDAV + Management API default ports.
EXPOSE 8080 8081
ENTRYPOINT ["/usr/local/bin/tgwebdav"]
CMD ["server"]
