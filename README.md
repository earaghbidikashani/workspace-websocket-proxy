# workspace-websocket-proxy

Remote IDE access for [jupyter-k8s](https://github.com/jupyter-infra/jupyter-k8s) workspaces. Lets VS Code, Cursor and Kiro connect to a workspace pod over a WebSocket tunnel, with no cloud-specific dependencies.

## Architecture

Two servers, two containers, one pod. They are the two ends of the same tunnel.

```
        your laptop
             |  wss:// (SSH inside a WebSocket)
             v
    ingress / Traefik  ---- validates a short-lived JWT
             |
+--- workspace pod ------------------------------------------+
|                                                            |
|  +-- sidecar container ----+  +-- workspace container ---+ |
|  |                         |  |                          | |
|  |  ws-proxy               |  |  jupyter lab       :8888 | |
|  |    listens :8080        |  |                          | |
|  |    forwards raw bytes ------->  remote-access-server   | |
|  |                         |  |    listens 127.0.0.1:2222| |
|  |                         |  |                          | |
|  |                         |  |  the user's files,       | |
|  |                         |  |  interpreter and kernels | |
|  +-------------------------+  +--------------------------+ |
|                                                            |
|   one network namespace, two filesystems                    |
+------------------------------------------------------------+
```

### `internal/proxy` — the sidecar

Listens for WebSocket connections on `:8080`, published by the workspace Service and reached through the ingress, and copies the bytes to `127.0.0.1:2222`.

It is protocol-dumb. It does not know the stream is SSH and performs no authentication of its own: the ingress validates a JWT during the WebSocket handshake, before any byte reaches the tunnel.

Built and run as a container. See [Dockerfile](Dockerfile).

### `internal/sshserver` — a process inside the workspace container

Listens for SSH on `127.0.0.1:2222`, terminates the SSH protocol, and serves each channel by spawning a shell, running an exec command, or serving SFTP.

It must run in the workspace container rather than the sidecar. Containers in a pod share a network namespace, which is why the sidecar's dial to `127.0.0.1:2222` arrives; they do not share a filesystem. An SSH server spawns the user's shell as its own child process, so the shell only lands in the real `$HOME` with the real interpreter and kernels if the server itself runs in that container.

It is IDE-agnostic. VS Code, Cursor and Kiro all bootstrap their server component by sending it as exec commands over an already-established connection, which is why exit statuses are propagated faithfully.

It performs no SSH authentication and refuses to bind a non-loopback address unless explicitly overridden. Loopback binding is what keeps the socket unreachable from outside the pod.

## Artifacts

| Image | Contains | How it is used |
|---|---|---|
| `ghcr.io/jupyter-infra/workspace-websocket-proxy` | `ws-proxy` | Run as the sidecar container. Has an `ENTRYPOINT`. |
| `ghcr.io/jupyter-infra/remote-access-server` | `remote-access-server` | Not run as a container. A workspace image extracts the binary with `COPY --from` and starts it under its own supervisor. Deliberately has no `ENTRYPOINT`. |

The second image is a distribution artifact, not a service. Running it directly would start a server with no shell to spawn.

## Starting the remote access server

Nothing in this repository launches it. A workspace image embeds the binary and starts it, for example under `supervisord` alongside the workspace application:

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

A supervisor is required because the server has to be listening before any client connects. A client cannot start it.

## Configuration

`ws-proxy`, from the environment:

| Variable | Default | Purpose |
|---|---|---|
| `LISTEN_ADDR` | `:8080` | Where the proxy accepts WebSocket connections |
| `TARGET_HOST` / `TARGET_PORT` | `127.0.0.1` / `2222` | Where it forwards bytes |
| `MAX_SESSION_DURATION` | `12h` | Hard cap on one connection |
| `PING_INTERVAL` / `PING_TIMEOUT` | `30s` / `60s` | Keepalive and dead-peer detection |
| `MAX_CONNECTIONS` | `10` | Concurrency cap, rejected with 429 |
| `READ_LIMIT` | `65536` | Maximum inbound message size |

Endpoints: `/health` reports on the proxy process only and is safe for a pod readiness probe. `/health/target` dials the target and is intended for alerting, not readiness, because pod readiness gates every port on the pod. `/metrics` serves Prometheus metrics.

`remote-access-server`, from the environment, with `--port`, `--host-key` and `--login-shell` overriding:

| Variable | Default | Purpose |
|---|---|---|
| `SSH_LISTEN_ADDR` | `127.0.0.1:2222` | Bind address, must be loopback |
| `SSH_HOST_KEY_PATH` | `$HOME/.jupyter-k8s/ssh_host_ed25519_key` | Persisted host key, generated on first use |
| `SSH_IDLE_TIMEOUT` | `0` (disabled) | Close a session with no traffic |
| `SSH_MAX_SESSIONS` | `10` | Concurrent session cap, `0` disables |
| `SSH_LOGIN_SHELL` | `false` | Run the session shell as a login shell |
| `SSH_ALLOW_NON_LOOPBACK` | `false` | Permit a routable bind. Publishes an unauthenticated shell. |

Persist the host key on storage that survives a container restart, otherwise every reconnect reports a changed host key. Enable `SSH_LOGIN_SHELL` for images that put their interpreter on `PATH` through a shell profile, such as conda-based images, rather than through the image environment.

## Development

```
make build         # both binaries into bin/
make test          # unit and integration tests, race detector on
make lint
make docker-build      # the sidecar image
make docker-build-ssh  # the remote access server image
make test-e2e          # Kind-based end-to-end tests
```

`internal/sshserver` carries an integration test that drives a real SSH session through a real proxy over a WebSocket, in process, with no cluster and no `ssh` or `websocat` binaries.

## License

[MIT](LICENSE)
