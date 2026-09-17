# Verifies the contract the remote access server image exists to provide: that a
# workspace image can lift the binary out with COPY --from and run it.
#
# Built against both a glibc and a musl base. The binary must be statically
# linked, since the image it comes from ships no C library at all, and only the
# musl base actually catches a regression there.

ARG CARRIER_IMAGE=jupyter-k8s-remote-access-server:latest
ARG BASE_IMAGE=debian:bookworm-slim

FROM ${CARRIER_IMAGE} AS carrier

FROM ${BASE_IMAGE}
COPY --from=carrier /remote-access-server /usr/local/bin/remote-access-server

# Executing at all proves the binary satisfies this base's dynamic loader.
RUN remote-access-server --help > /dev/null 2>&1

# An empty HOME forces the passwd lookup, which is the call that links against
# libc when cgo is enabled. Reaching the loopback check proves it resolved a home
# directory; had the lookup failed, the binary would have refused the resulting
# empty host key path instead. The bind address is deliberately not loopback so
# the server refuses and exits rather than starting and blocking the build.
RUN HOME= SSH_LISTEN_ADDR=0.0.0.0:2222 remote-access-server 2>&1 \
    | grep -q "refusing to listen" \
    || (echo "the binary did not reach its loopback check: the passwd lookup failed" && exit 1)
