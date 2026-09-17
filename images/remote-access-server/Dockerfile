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
# pure Go implementations whatever CGO_ENABLED is, which is what allows a workspace
# image built on any C library to run this binary.
RUN --mount=type=bind,target=. \
    --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -tags osusergo,netgo -trimpath -ldflags="-s -w" \
    -o /out/remote-access-server ./cmd/remote-access-server

# The remote access server runs inside the workspace container, not as a
# container of its own: it spawns the user's shell, so it must share that
# container's filesystem. This image exists so a workspace image can extract the
# binary with COPY --from, which is why there is no ENTRYPOINT. Running this
# image directly would start a server that cannot find a shell to spawn.
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /out/remote-access-server /remote-access-server
USER 65532:65532
