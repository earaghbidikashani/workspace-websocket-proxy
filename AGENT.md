# workspace-websocket-proxy Developer Guide

> **Note:** `CLAUDE.md` is a symlink to this file (`AGENT.md`).
> Always edit `AGENT.md` directly — never update `CLAUDE.md`.

## Project Overview

Remote IDE access for
[jupyter-k8s](https://github.com/jupyter-infra/jupyter-k8s) workspaces. Carries
SSH inside a WebSocket so a desktop IDE can reach a workspace pod through the
cluster's existing HTTPS ingress, with no cloud-specific dependencies.

Module path: `github.com/jupyter-infra/workspace-websocket-proxy`

No dependency on `jupyter-k8s` (the operator) or `jupyter-k8s-aws`.

## Architecture

**Two servers, two containers, one pod.** They are the two ends of one tunnel.
This is the single most important thing to understand before changing anything
here.

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

### `internal/proxy` — the sidecar container

Listens on `:8080` for WebSocket connections and copies raw bytes to
`127.0.0.1:2222`. Enforces session lifecycle: max duration, ping/pong,
concurrency cap, read limit.

Protocol-dumb by design: it does not know the stream is SSH, and it performs no
authentication. The ingress validates the JWT before any byte reaches it
(`revalidator.go` is scaffolding for future mid-session re-validation and is
currently a no-op).

### `internal/sshserver` — a process inside the workspace container

Listens on `127.0.0.1:2222`, terminates SSH, and services each channel by
spawning a shell, running an exec command, or serving SFTP.

**It must run in the workspace container, not the sidecar.** Containers in a pod
share a network namespace, which is why the sidecar's dial to `127.0.0.1:2222`
arrives. They do not share a filesystem. An SSH server spawns the user's shell as
its own child process, so the shell inherits the server's mount namespace — it
only lands in the real `$HOME` with the real interpreter and kernels if the
server itself runs in that container.

Four invariants that are easy to break and must not be:

- **Loopback only, no SSH authentication.** The ingress is the gate; the loopback
  bind is the only transport-level protection. `Config.validate` refuses a
  non-loopback bind unless `SSH_ALLOW_NON_LOOPBACK=true`.
- **Faithful exit statuses**, including `128+signal`. VS Code, Cursor and Kiro
  bootstrap their server component through exec commands and branch on the exit
  code. Swallowing it breaks connections in ways that are very hard to diagnose
  from the client.
- **A disconnected client's processes must die.** The output copy blocks reading
  from the child, and a silent child never unblocks it, so nothing else will
  notice. `terminateOnDisconnect` watches `session.Context()` and signals the
  process *group*; the exec path additionally needs `Setpgid`, without which a
  negative-PID signal fails with `ESRCH` and the cleanup silently does nothing.
- **Signalling and reaping must not interleave.** After `Wait` reaps a child its
  PID can be reused, so `sessionCmd` serialises the two under a mutex rather than
  relying on the window being small.

Port forwarding is permitted only to loopback destinations, in both directions, so
a session cannot become a proxy into the cluster network.

Reverse forwards (`forward.go`) do not use the library's `ForwardedTCPHandler`,
and the reason is subtle enough to be worth stating. Two addresses are involved
and they must differ:

- the **listener** must bind loopback, or a forward is reachable from outside the
  pod. The library hands the requested address straight to `net.Listen`, so the
  empty address IDEs send becomes `":port"`.
- the **`forwarded-tcpip` channel** must report the address the *client asked
  for*. RFC 4254 §7.2 specifies the address from the request, and clients match
  incoming channels against exactly what they registered.

The library derives both from one field, so rewriting the request payload cannot
satisfy both: it binds correctly and then every forwarded connection is rejected
by the client, which presents as the forward accepting connections and instantly
resetting them. `loopbackForwardHandler` keeps the two apart, and keys its
forward table on the requested address because that is what a cancel names.

### Nothing here starts the SSH server

This repository builds and publishes it but never launches it. A workspace image
embeds the binary and starts it under its own supervisor, because the server has
to be listening before a client connects. That wiring lives outside this repo,
in the workspace image build.

## Artifacts

| Image | Contains | Runnable? |
|---|---|---|
| `workspace-websocket-proxy` | `ws-proxy` | Yes, has an `ENTRYPOINT`. Runs as the sidecar. |
| `remote-access-server` | `remote-access-server` | No `ENTRYPOINT` on purpose. A carrier so a workspace image can `COPY --from` the binary. |

Keep the dependency surfaces separate. `ws-proxy` must not import
`internal/sshserver`, so the sidecar image carries none of the SSH dependencies
(`gliderlabs/ssh`, `creack/pty`, `pkg/sftp`) and a CVE in them does not flag the
sidecar. `internal/proxy/deps_test.go` enforces this with `go list -deps`, so an
import added through `internal/proxy` is caught too.

Both binaries must stay statically linked, because the carrier image ships no C
library and a workspace image may be built on any distribution. Two things keep
that true, and both are needed:

- `CGO_ENABLED=0` on every build path, asserted by `make verify-static`.
- `-tags osusergo,netgo` (`GO_BUILD_TAGS`), because `CGO_ENABLED` defaults to `1`
  and `os/user` links `getpwuid_r` when cgo is on. `user.Current()` is reached
  from `homeDir` and `withPasswdFallback`, so this package would otherwise pull in
  libc. The tags are applied to `go vet` and `go test` as well, so tests exercise
  the implementation that ships.

`make verify-carrier-image` builds `test/carrier/Dockerfile` against a glibc and a
musl base, extracting the binary with `COPY --from` and running it. Only the musl
base catches a linking regression. That target is the sole place the carrier
image's contract is exercised.

### Key files

- `cmd/ws-proxy/main.go` — sidecar entry point, `--healthcheck` mode, graceful shutdown
- `cmd/remote-access-server/main.go` — SSH server entry point, flag overrides
- `Dockerfile` — the sidecar image
- `images/remote-access-server/Dockerfile` — the binary-carrier image
- `internal/proxy/server.go` — HTTP server, `/health`, `/health/target`, WebSocket upgrade
- `internal/proxy/bridge.go` — bidirectional WebSocket ↔ TCP copy, binary frames only
- `internal/proxy/session.go` — session lifecycle
- `internal/sshserver/server.go` — SSH handlers: session, SFTP, port-forward callbacks, disconnect cleanup
- `internal/sshserver/config.go` — configuration and the loopback-bind guard
- `internal/sshserver/forward.go` — reverse port forwarding, loopback-pinned
- `internal/sshserver/shell.go` — shell selection, command assembly, environment merging
- `internal/sshserver/hostkey.go` — host key load, generate and persist
- `internal/proxy/deps_test.go` — pins the sidecar's dependency surface

`/health` reports on the proxy process alone and never dials the target. Keep it
that way: pod readiness gates every port on the pod, so a readiness probe that
failed because remote access was broken would also withdraw port 8888 and take
the web UI down. `/health/target` is the endpoint that actually probes, and it is
for alerting, not readiness.

The probe reads the target's greeting, not just a dial: the kernel completes a TCP
handshake from the listen backlog even when the target process is wedged, so a
dial alone reports a deadlocked server as healthy. The expected prefix is
configuration (`TARGET_HEALTH_BANNER_PREFIX`, default `SSH-2.0-`), so the check
compares a string and this package still contains no SSH protocol code. The e2e
fixture sets it empty because its target is a socat echo server.

Probing runs on a timer (`targethealth.go`) and `/health/target` reports the last
result. Do not move the probe back into the handler. A gauge written only by a
handler keeps its zero value on a healthy pod, because nothing calls the endpoint,
so an alert on `ws_proxy_target_reachable` would fire everywhere. Reporting rather
than probing also decouples the load on the target from the request rate, which
matters because the endpoint is unauthenticated and shares a listener with the
data path.

## Build & Test

```bash
make build             # both binaries into bin/
make test              # tests with race detector
make lint              # golangci-lint
make docker-build      # sidecar image
make docker-build-ssh  # remote access server image
make test-e2e          # Kind-based end-to-end tests
```

Run `make test` rather than `go test`: the race detector is what caught a
use-after-close in the PTY resize path, and plain `go test` passes without it.

`internal/sshserver/integration_test.go` drives a real SSH session through a real
proxy over a WebSocket, in process, with no cluster and no `ssh` or `websocat`
binaries. It adapts the WebSocket to a `net.Conn` and hands it to an SSH client,
the same way a `ProxyCommand` does.

## Configuration

All via environment variables, with flags overriding for
`remote-access-server`. See `internal/proxy/config.go` and
`internal/sshserver/config.go` for defaults, and README.md for the tables.

## Conventions

- Copyright header: `Amazon Web Services`
- Linter: golangci-lint v2 (see `.golangci.yml`). `revive`'s `exported` rule
  requires a doc comment on exported declarations.
- Container tool: `finch` (default in Makefile)
- Base image: `gcr.io/distroless/static:nonroot`
- User: 65532 (non-root)
- Image versions follow `v0.1.0-rc.N` before the first GA release, matching
  `jupyter-k8s-ui` and `jupyter-k8s-authmiddleware`.
