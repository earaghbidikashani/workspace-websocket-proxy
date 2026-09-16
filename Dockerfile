# Pinned to the builder's own architecture so the toolchain runs natively and
# cross-compiles to the target, rather than the whole build running under QEMU.
# The code is pure Go with cgo disabled, so cross-compiling costs nothing.
FROM --platform=$BUILDPLATFORM golang:1.26 AS builder
ARG TARGETOS
ARG TARGETARCH

# Unset by default so the build uses the module proxy. Forwarded from the
# developer's environment by the Makefile, since a network that cannot reach
# proxy.golang.org needs GOPROXY=direct and a build arg is the only way in.
ARG GOPROXY

WORKDIR /workspace

RUN --mount=type=bind,source=go.mod,target=go.mod \
    --mount=type=bind,source=go.sum,target=go.sum \
    --mount=type=cache,target=/go/pkg/mod \
    go mod download

# The source is bind mounted rather than copied, so the output has to land outside
# the read-only mount. osusergo and netgo keep os/user and the resolver on their
# pure Go implementations whatever CGO_ENABLED is.
RUN --mount=type=bind,target=. \
    --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -tags osusergo,netgo -trimpath -ldflags="-s -w" \
    -o /out/ws-proxy ./cmd/ws-proxy

# Use distroless as minimal base image to package the binary
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /out/ws-proxy /ws-proxy
USER 65532:65532

# Expose proxy port
EXPOSE 8080

# Defaults (overridden by pod spec)
ENV LISTEN_ADDR=:8080 \
    TARGET_HOST=127.0.0.1 \
    TARGET_PORT=2222 \
    MAX_SESSION_DURATION=12h \
    PING_INTERVAL=30s \
    PING_TIMEOUT=60s \
    MAX_CONNECTIONS=10

ENTRYPOINT ["/ws-proxy"]
