# workspace-websocket-proxy

[![Lint](https://github.com/jupyter-infra/workspace-websocket-proxy/actions/workflows/lint.yml/badge.svg)](https://github.com/jupyter-infra/workspace-websocket-proxy/actions/workflows/lint.yml)
[![Tests](https://github.com/jupyter-infra/workspace-websocket-proxy/actions/workflows/test.yml/badge.svg)](https://github.com/jupyter-infra/workspace-websocket-proxy/actions/workflows/test.yml)
[![E2E Tests](https://github.com/jupyter-infra/workspace-websocket-proxy/actions/workflows/e2e.yml/badge.svg)](https://github.com/jupyter-infra/workspace-websocket-proxy/actions/workflows/e2e.yml)

Remote IDE access for [jupyter-k8s](https://github.com/jupyter-infra/jupyter-k8s)
workspaces. Carries SSH inside a WebSocket so VS Code, Cursor and Kiro connect to a
workspace pod through the cluster's existing HTTPS ingress.

- **No new ingress** — one WebSocket over the port the cluster already exposes, so
  there is nothing extra to open or route.
- **Authenticated at the edge** — the ingress validates a short-lived JWT during the
  WebSocket handshake, before any byte reaches the tunnel.
- **Vendor neutral** — no cloud-specific dependencies and no dependency on the
  operator.
- **IDE agnostic** — any client that can run a `ProxyCommand` works.

## Components

| Component | Description |
|-----------|-------------|
| **ws-proxy** | Sidecar container. Accepts WebSocket connections and copies raw bytes to the SSH server. |
| **remote-access-server** | SSH server that runs inside the workspace container, where the user's files and interpreter live. |

## Architecture

Two servers, two containers, one pod. They are the two ends of the same tunnel.

```
        your laptop
             |  wss:// (SSH inside a WebSocket)
             v
    ingress / Traefik  ---- validates a short-lived JWT
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

The SSH server runs in the workspace container, not the sidecar. Containers in a pod
share a network namespace, which is why the sidecar's dial to `127.0.0.1:2222`
arrives; they do not share a filesystem. An SSH server spawns the user's shell as its
own child process, so the shell only lands in the real `$HOME` with the real
interpreter and kernels if the server itself runs in that container.

## Packages

| Artifact | Description |
|----------|-------------|
| `ghcr.io/jupyter-infra/workspace-websocket-proxy` image | The sidecar. Has an `ENTRYPOINT` and runs as a container. |
| `ghcr.io/jupyter-infra/remote-access-server` image | A distribution artifact, not a service. A workspace image extracts the binary with `COPY --from`. Deliberately has no `ENTRYPOINT`. |

## Installation

The sidecar is deployed by the workspace charts in
[jupyter-k8s-aws](https://github.com/jupyter-infra/jupyter-k8s-aws); enabling remote
access there is enough to run it.

The SSH server has to be embedded in the workspace image, because it must already be
listening before a client connects and a client cannot start it. Copy the binary and
run it under a supervisor alongside the workspace application:

```dockerfile
COPY --from=ghcr.io/jupyter-infra/remote-access-server:v0.1.0 \
     /remote-access-server /usr/local/bin/remote-access-server
```

```ini
[program:remote-access]
command=/usr/local/bin/remote-access-server --port 2222
autostart=true
autorestart=true
```

The binary is statically linked, so it runs on any Linux base image.

## Configuration

`ws-proxy`, from the environment:

| Variable | Default | Purpose |
|----------|---------|---------|
| `LISTEN_ADDR` | `:8080` | Where the proxy accepts WebSocket connections |
| `TARGET_HOST` / `TARGET_PORT` | `127.0.0.1` / `2222` | Where it forwards bytes |
| `MAX_SESSION_DURATION` | `12h` | Hard cap on one connection |
| `PING_INTERVAL` / `PING_TIMEOUT` | `30s` / `60s` | Keepalive and dead-peer detection |
| `MAX_CONNECTIONS` | `10` | Concurrency cap, rejected with 429 and a `Retry-After` |
| `READ_LIMIT` | `65536` | Maximum inbound message size |
| `TARGET_HEALTH_BANNER_PREFIX` | `SSH-2.0-` | Greeting the target health probe expects. Empty disables the check. |
| `TARGET_HEALTH_INTERVAL` | `30s` | How often the background prober checks the target |

`remote-access-server`, from the environment, with `--port`, `--host-key` and
`--login-shell` overriding:

| Variable | Default | Purpose |
|----------|---------|---------|
| `SSH_LISTEN_ADDR` | `127.0.0.1:2222` | Bind address, must be loopback |
| `SSH_HOST_KEY_PATH` | `$HOME/.ssh/ssh_host_ed25519_key` | Persisted host key, generated on first use |
| `SSH_IDLE_TIMEOUT` | `12h` | Close a connection with no traffic in either direction. `0` disables. |
| `SSH_MAX_SESSIONS` | `10` | Concurrent shell and exec cap, `0` disables |
| `SSH_LOGIN_SHELL` | `false` | Run the session shell as a login shell |
| `SSH_ALLOW_NON_LOOPBACK` | `false` | Permit a routable bind. Publishes an unauthenticated shell. |

Notes that matter when deploying:

- **Persist the host key** on storage that survives a container restart, or every
  reconnect reports a changed host key. The server warns at startup when the key
  would not survive, including the common case of a key on the container's own
  writable layer because no volume is mounted where it lives.
- **Enable `SSH_LOGIN_SHELL`** for images that put their interpreter on `PATH`
  through a shell profile, such as conda-based images, rather than through the image
  environment.
- **`SSH_MAX_SESSIONS` bounds shell and exec channels only.** SFTP transfers and
  port forwards do not claim a slot.
- **Reverse forwards always bind loopback**, including when a client requests every
  interface, which is what IDEs do.

### Endpoints

| Path | Purpose |
|------|---------|
| `/health` | The proxy process only. Safe for a pod readiness probe. |
| `/health/target` | Target reachability, for alerting rather than readiness, because pod readiness gates every port on the pod. |
| `/metrics` | Prometheus metrics, including `ws_proxy_target_reachable`. |

A background prober checks the target every `TARGET_HEALTH_INTERVAL` and publishes
the result; `/health/target` reports that last result and does not dial. Before the
first probe completes it reports `unknown`, which is distinct from `unreachable`.

## Contributing

See [AGENT.md](AGENT.md) for architecture, development workflow, and the checks to
run before submitting a PR.

## License

[MIT](LICENSE)
