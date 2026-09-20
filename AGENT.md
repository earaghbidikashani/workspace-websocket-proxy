# workspace-websocket-proxy Developer Guide

> **Note:** `CLAUDE.md` is a symlink to this file (`AGENT.md`).
> Always edit `AGENT.md` directly — never update `CLAUDE.md`.

## Project Overview

Remote IDE access for [jupyter-k8s](https://github.com/jupyter-infra/jupyter-k8s)
workspaces. Carries SSH inside a WebSocket so a desktop IDE reaches a workspace pod
through the cluster's existing HTTPS ingress, with no cloud-specific dependencies.

Module path: `github.com/jupyter-infra/workspace-websocket-proxy`

No dependency on `jupyter-k8s` (the operator) or `jupyter-k8s-aws`.

## Architecture

**Two servers, two containers, one pod.** They are the two ends of one tunnel, and
this is the single most important thing to understand before changing anything here.

```
    ingress / Traefik  ---- validates a short-lived JWT during the handshake
             |
+--- workspace pod ------------------------------------------+
|  +-- sidecar container ----+  +-- workspace container ---+ |
|  |  ws-proxy               |  |  jupyter lab       :8888 | |
|  |    :8080  ---- raw bytes ----> remote-access-server    | |
|  |                         |  |            127.0.0.1:2222| |
|  |                         |  |  the user's files,       | |
|  |                         |  |  interpreter and kernels | |
|  +-------------------------+  +--------------------------+ |
|   one network namespace, two filesystems                    |
+------------------------------------------------------------+
```

- **`internal/proxy`** — the sidecar. Accepts WebSocket connections on `:8080` and
  copies raw bytes to `127.0.0.1:2222`, enforcing max duration, ping/pong, a
  concurrency cap and a read limit. Protocol-dumb by design: it does not know the
  stream is SSH and performs no authentication, because the ingress validates the
  JWT before any byte reaches it. `revalidator.go` is scaffolding for future
  mid-session re-validation and is currently a no-op.
- **`internal/sshserver`** — a process inside the workspace container. Terminates
  SSH and services each channel by spawning a shell, running an exec command, or
  serving SFTP.

Nothing in this repository starts the SSH server. It is built and published here,
but a workspace image embeds the binary and starts it under its own supervisor,
because the server has to be listening before a client connects. That wiring lives
in the workspace image build, outside this repo.

### Why the SSH server cannot run in the sidecar

Containers in a pod share a network namespace, which is why the sidecar's dial to
`127.0.0.1:2222` arrives. They do not share a filesystem. An SSH server spawns the
user's shell as its own child process, so the shell inherits the server's mount
namespace — it only lands in the real `$HOME` with the real interpreter and kernels
if the server itself runs in that container.

### SSH server invariants

Easy to break, and all four have cost a bug already:

- **Loopback only, no SSH authentication.** The ingress is the gate; the loopback
  bind is the only transport-level protection. `Config.validate` refuses a
  non-loopback bind unless `SSH_ALLOW_NON_LOOPBACK=true`.
- **Faithful exit statuses**, including `128+signal`. IDEs bootstrap their server
  component through exec commands and branch on the exit code.
- **A disconnected client's processes must die.** The output copy blocks reading
  from the child and a silent child never unblocks it, so nothing else notices.
  `terminateOnDisconnect` watches `session.Context()` and signals the process
  *group*; the exec path also needs `Setpgid`, without which a negative-PID signal
  fails with `ESRCH` and the cleanup silently does nothing.
- **Signalling and reaping must not interleave.** A reaped PID can be reused, so
  `sessionCmd` serialises the two under a mutex.

### Port forwarding

Permitted only to loopback destinations, in both directions, so a session cannot
become a proxy into the cluster network.

Reverse forwards (`forward.go`) deliberately avoid the library's
`ForwardedTCPHandler`, because two addresses are involved and they must differ:

- the **listener** must bind loopback, or the forward is reachable from outside the
  pod. The library passes the requested address straight to `net.Listen`, so the
  empty address IDEs send becomes `":port"`.
- the **`forwarded-tcpip` channel** must report the address the *client asked for*.
  RFC 4254 §7.2 specifies the address from the request, and clients match incoming
  channels against exactly what they registered.

The library derives both from one field, so rewriting the request payload satisfies
only the first: the listener binds correctly and then the client rejects every
forwarded connection, which presents as a forward that accepts and instantly resets.
`loopbackForwardHandler` keeps the two apart and keys its table on the requested
address, because that is what a cancel names.

### Health endpoints

`/health` reports on the proxy process alone and never dials the target. Keep it
that way: pod readiness gates every port on the pod, so a readiness probe failing
because remote access broke would also withdraw port 8888 and take the web UI down.
`/health/target` is for alerting, not readiness.

A background prober (`targethealth.go`) checks the target on a timer and
`/health/target` reports the last result. Do not move the probe back into the
handler: a gauge written only by a handler keeps its zero value on a healthy pod,
because nothing calls the endpoint, so an alert on `ws_proxy_target_reachable` would
fire everywhere. Reporting rather than probing also decouples load on the target
from the request rate, which matters because the endpoint is unauthenticated and
shares a listener with the data path.

The probe reads the target's greeting rather than only dialing, because the kernel
completes a TCP handshake from the listen backlog even when the target process is
wedged. The expected prefix is configuration
(`TARGET_HEALTH_BANNER_PREFIX`, default `SSH-2.0-`), so this package holds no SSH
protocol code. The e2e fixture sets it empty because its target is a socat echo.

### Artifacts

| Image | Contains | Runnable |
|-------|----------|----------|
| `workspace-websocket-proxy` | `ws-proxy` | Yes, has an `ENTRYPOINT`. Runs as the sidecar. |
| `remote-access-server` | `remote-access-server` | No `ENTRYPOINT`, on purpose. A carrier so a workspace image can `COPY --from` the binary. |

Keep the dependency surfaces separate. `ws-proxy` must not import
`internal/sshserver`, so the sidecar image carries none of the SSH dependencies
(`gliderlabs/ssh`, `creack/pty`, `pkg/sftp`) and a CVE in them does not flag the
sidecar. `internal/proxy/deps_test.go` enforces this with `go list -deps`, which
catches an import added through `internal/proxy` too.

Both binaries must stay statically linked, because the carrier image ships no C
library and a workspace image may be built on any distribution. Two things keep that
true and both are needed:

- `CGO_ENABLED=0` on every build path, asserted by `make verify-static`.
- `-tags osusergo,netgo` (`GO_BUILD_TAGS`), because `CGO_ENABLED` defaults to `1`
  and `os/user` links `getpwuid_r` when cgo is on. `user.Current()` is reached from
  `homeDir` and `withPasswdFallback`. The tags apply to `go vet` and `go test` too,
  so tests exercise the implementation that ships.

`make verify-carrier-image` is the only place the carrier's contract is exercised:
it builds `test/carrier/Dockerfile` against a glibc and a musl base, extracting the
binary with `COPY --from` and running it. Only the musl base catches a linking
regression.

## Repository Layout

| Directory / file | Purpose |
|------------------|---------|
| `cmd/ws-proxy/` | Sidecar entry point, `--healthcheck` mode, graceful shutdown |
| `cmd/remote-access-server/` | SSH server entry point, flag overrides |
| `internal/proxy/server.go` | HTTP server, `/health`, `/health/target`, WebSocket upgrade |
| `internal/proxy/bridge.go` | Bidirectional WebSocket ↔ TCP copy, binary frames only |
| `internal/proxy/session.go` | Session lifecycle |
| `internal/proxy/targethealth.go` | Background target prober and its cached result |
| `internal/proxy/deps_test.go` | Pins the sidecar's dependency surface |
| `internal/sshserver/server.go` | SSH handlers: session, SFTP, forward callbacks, disconnect cleanup |
| `internal/sshserver/config.go` | Configuration and the loopback-bind guard |
| `internal/sshserver/forward.go` | Reverse port forwarding, loopback-pinned |
| `internal/sshserver/shell.go` | Shell selection, command assembly, environment merging |
| `internal/sshserver/hostkey.go` | Host key load, generate and persist atomically |
| `Dockerfile` | The sidecar image |
| `images/remote-access-server/Dockerfile` | The binary-carrier image |
| `test/carrier/Dockerfile` | Consumer that proves the `COPY --from` contract |
| `test/e2e/` | Kind-based end-to-end tests (Ginkgo, `e2e` build tag) |

## Development

### Prerequisites
- Go 1.26+
- golangci-lint v2.12+
- Container tool: Finch or Docker
- kind (for end-to-end tests)

### Common Tasks
- Build both binaries: `make build`
- Unit and integration tests: `make test`
- Lint: `make lint`
- Lint with auto-fix: `make lint-fix`
- Assert static linking: `make verify-static`
- Build sidecar image: `make docker-build`
- Build carrier image: `make docker-build-ssh`
- Verify the carrier contract: `make verify-carrier-image`
- Download/tidy deps: `make deps`

Run `make test` rather than `go test`: the race detector is what caught a
use-after-close in the PTY resize path, and plain `go test` passes without it.

`internal/sshserver/integration_test.go` drives a real SSH session through a real
proxy over a WebSocket, in process, with no cluster and no `ssh` or `websocat`
binaries. It adapts the WebSocket to a `net.Conn` and hands it to an SSH client, the
same way a `ProxyCommand` does.

### End-to-End Testing

Runs against a local Kind cluster; no cloud account needed.

- Full cycle (setup, run, cleanup): `make test-e2e-full`
- Create the cluster and load the image: `make setup-test-e2e`
- Run the suite: `make test-e2e`
- Run one spec: `make test-e2e-focus FOCUS="should proxy"`
- Delete the cluster: `make cleanup-test-e2e`

The fixture (`test/e2e/testdata/test-pod.yaml`) simulates the remote access server
with a socat echo on 2222, so the suite proves the proxy copies bytes but never
exercises the SSH server. Running the real binary there is tracked in
[#7](https://github.com/jupyter-infra/workspace-websocket-proxy/issues/7).

### Configuration

All via environment variables, with flags overriding for `remote-access-server`.
See `internal/proxy/config.go` and `internal/sshserver/config.go` for defaults, and
README.md for the tables.

### Before Submitting a PR
- `make build`
- `make lint`
- `make test`
- `make verify-static`
- `make verify-carrier-image`
- `make test-e2e-full`

Anything touching concurrency or platform-specific syscalls should also be checked
on Linux, not only on a developer Mac. Two defects reached CI that way: an
`unconvert` finding on `syscall.Stat_t.Dev`, which is `uint64` on Linux and `int32`
on Darwin, and a `WaitGroup.Add` versus `Wait` race that only the Linux race
detector reported.

## CI & Release

See [`.github/AGENT.md`](.github/AGENT.md) for workflow details, release flow, and
how to test workflow changes from feature branches.

## Notes

- Copyright header: `Amazon Web Services`
- Default container runtime is Finch (configurable via `CONTAINER_TOOL`). CI uses `docker`.
- Uses golangci-lint v2 (see `.golangci.yml`). `revive`'s `exported` rule requires a
  doc comment on exported declarations.
- Base image `gcr.io/distroless/static:nonroot`, running as user 65532
- Image versions follow `v0.1.0-rc.N` before the first GA release, matching
  `jupyter-k8s-ui` and `jupyter-k8s-authmiddleware`
