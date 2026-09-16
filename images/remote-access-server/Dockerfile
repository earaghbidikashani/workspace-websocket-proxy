# Build stage
FROM golang:1.26 AS builder
ARG TARGETOS
ARG TARGETARCH

# Bypass Go proxy due to corporate network issues
ENV GOPROXY=direct

# Set working directory
WORKDIR /workspace

# Copy go.mod and go.sum first to leverage Docker cache
COPY go.mod go.sum ./
RUN go mod download

# Copy the source code
COPY internal/ internal/
COPY cmd/ cmd/

# Build
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -tags osusergo,netgo -a -o remote-access-server ./cmd/remote-access-server

# The remote access server runs inside the workspace container, not as a
# container of its own: it spawns the user's shell, so it must share that
# container's filesystem. This image exists so a workspace image can extract the
# binary with COPY --from, which is why there is no ENTRYPOINT. Running this
# image directly would start a server that cannot find a shell to spawn.
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/remote-access-server /remote-access-server
USER 65532:65532
