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
| `MAX_CONNECTIONS` | `10` | Concurrency cap, rejected with 429 and a `Retry-After` |
| `READ_LIMIT` | `65536` | Maximum inbound message size |
| `TARGET_HEALTH_BANNER_PREFIX` | `SSH-2.0-` | Greeting the target health probe expects. Empty disables the check. |
| `TARGET_HEALTH_INTERVAL` | `30s` | How often the background prober checks the target |

Endpoints: `/health` reports on the proxy process only and is safe for a pod readiness probe. `/health/target` reports target reachability and is intended for alerting, not readiness, because pod readiness gates every port on the pod. `/metrics` serves Prometheus metrics.

A background prober checks the target every `TARGET_HEALTH_INTERVAL` and publishes the result as `ws_proxy_target_reachable`. `/health/target` reports that last result and does not dial, so the load on the target is fixed by the interval rather than by how often the endpoint is called, and the metric is meaningful whether or not anything scrapes the endpoint. Before the first probe completes it reports `unknown`, which is distinct from `unreachable`.

The probe connects and then reads the target's greeting rather than only dialing, because the kernel completes a TCP handshake from the listen backlog even when the target process is wedged and never accepts, so a dial alone reports a deadlocked server as healthy. Point `TARGET_HEALTH_BANNER_PREFIX` at whatever the target announces, or set it empty for a target that announces nothing.

`remote-access-server`, from the environment, with `--port`, `--host-key` and `--login-shell` overriding:

| Variable | Default | Purpose |
|---|---|---|
| `SSH_LISTEN_ADDR` | `127.0.0.1:2222` | Bind address, must be loopback |
| `SSH_HOST_KEY_PATH` | `$HOME/.ssh/ssh_host_ed25519_key` | Persisted host key, generated on first use |
| `SSH_IDLE_TIMEOUT` | `12h` | Close a connection with no traffic in either direction. `0` disables. |
| `SSH_MAX_SESSIONS` | `10` | Concurrent shell and exec cap, `0` disables |
| `SSH_LOGIN_SHELL` | `false` | Run the session shell as a login shell |
| `SSH_ALLOW_NON_LOOPBACK` | `false` | Permit a routable bind. Publishes an unauthenticated shell. |

Persist the host key on storage that survives a container restart, otherwise every reconnect reports a changed host key. The server warns at startup when the key would not survive, which covers both temporary filesystems and the more common case of a key sitting on the container's own writable layer because no volume is mounted where it lives. The key is written to a temporary file, flushed and renamed into place, so a crash cannot leave a partial file that a later start would refuse. Enable `SSH_LOGIN_SHELL` for images that put their interpreter on `PATH` through a shell profile, such as conda-based images, rather than through the image environment.

`SSH_MAX_SESSIONS` bounds shell and exec channels only. SFTP subsystems and port forwards do not claim a slot, so concurrent transfers and forwarded connections are unbounded, which is acceptable for a single-tenant workspace pod but worth knowing before relying on the number.

Reverse forwards always bind loopback, including when the client requests every interface, which is what IDEs do. A request naming a specific routable address is refused rather than moved.

Session variables come from the server's own environment with the client's overlaid on top. When that leaves `HOME`, `USER` or `LOGNAME` unset, they are filled from the passwd entry, because a launcher need not provide them: supervisord does not set them for a program started under `user=`.

When a client disconnects, the session's process group is sent `SIGHUP` and then `SIGKILL` after a short grace period. Signalling the group rather than the shell alone means processes the session backgrounded go with it. On `SIGTERM` the server drains active sessions for up to twenty seconds before severing what remains, so a rollout does not cut a command or an SFTP write mid-write.

## Development

```
make build         # both binaries into bin/
make test          # unit and integration tests, race detector on
make lint
make docker-build      # the sidecar image
make docker-build-ssh  # the remote access server image
make verify-static     # assert both binaries are statically linked
make verify-carrier-image  # assert a consumer can COPY --from and run the binary
make test-e2e          # Kind-based end-to-end tests
```

Both binaries are built with `CGO_ENABLED=0` and `-tags osusergo,netgo` so they
stay statically linked. That matters because the remote access server is copied
into a workspace image that may be built on any distribution, and `os/user`, which
this code calls to resolve the home directory, links against libc when cgo is
enabled. `make verify-carrier-image` proves the point by extracting the binary into
both a glibc and a musl base and running it.

`internal/sshserver` carries an integration test that drives a real SSH session through a real proxy over a WebSocket, in process, with no cluster and no `ssh` or `websocat` binaries.

## License

[MIT](LICENSE)
