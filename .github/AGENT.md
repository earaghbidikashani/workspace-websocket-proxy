# .github — CI Workflows

## Workflows

| Workflow | Trigger | Purpose |
|----------|---------|---------|
| `lint.yml` | push/PR | golangci-lint |
| `test.yml` | push/PR | `go mod tidy` drift check, unit and integration tests with the race detector |
| `build.yml` | push/PR | Both binaries, static-linking assertion, both images, carrier-contract check |
| `e2e.yml` | push/PR | Kind cluster, end-to-end suite |
| `release.yml` | `workflow_dispatch` | Orchestrator: validate → lint/test → stage → promote → release |
| `release-stage-image.yml` | `workflow_call` / `workflow_dispatch` | Build both images multi-arch, push to staging GHCR |
| `release-promote-image.yml` | `workflow_call` | Promote both images: crane copy staging → production |

## Artifacts

Both images are versioned together, from one tag.

| Artifact | GHCR path |
|----------|-----------|
| Sidecar image | `ghcr.io/jupyter-infra/workspace-websocket-proxy` |
| Carrier image | `ghcr.io/jupyter-infra/remote-access-server` |

Git tag format: `vX.Y.Z`, or `vX.Y.Z-prerelease` for a release candidate, which
`release.yml` marks as a GitHub pre-release.

## Release Flow

```
workflow_dispatch (version + dry_run)
  → validate (semver, tag uniqueness)
  → lint + test                                          [parallel]
  → stage-image (multi-arch build of both images → staging GHCR)
  → [manual verification]
  → promote-image (crane copy → production GHCR)         [skipped if dry_run]
  → release (git tag vX.Y.Z + GitHub Release)            [skipped if dry_run]
```

`dry_run` stages without promoting, giving a manual verification checkpoint.

The carrier image is consumed by a workspace image in
[jupyter-deploy](https://github.com/jupyter-infra/jupyter-deploy) via `COPY --from`,
so a promoted version is what downstream builds pin. Promote deliberately, and
prefer an explicit version over `:latest` in any consuming Dockerfile.

## Build Caching

The image builds pin the builder stage to `$BUILDPLATFORM` and cross-compile, rather
than running the Go toolchain under QEMU for the non-native architecture. On an
arm64 host, emulating amd64 took 1m34s for one platform; cross-compiling builds both
in 14s.

`cache-from` / `cache-to: type=gha` is set on both image builds. It carries layers
only, not `RUN --mount=type=cache` contents
([moby/buildkit#1512](https://github.com/moby/buildkit/issues/1512)), so the module
and build caches start empty on a fresh runner regardless. The layer cache is still
worth having; there is nothing further to tune there.

`.dockerignore` matters more than it looks. The builds bind mount the whole context,
so everything in it forms the cache key for the Go compile. `bin/` alone is around
25 MB that `make build` rewrites immediately before both image builds, which
invalidated the compile on every run until it was excluded.

## Testing Workflow Changes

`workflow_dispatch` only fires on the default branch. To iterate from a feature
branch, create a temporary push-triggered workflow:

```yaml
# .github/workflows/test-<name>.yml  — DO NOT merge to main
name: Test workflow (temporary)
on:
  push:
    branches: [your-branch]
permissions:
  contents: read
  packages: write
jobs:
  test:
    uses: ./.github/workflows/release-stage-image.yml
    with:
      version: v0.1.0-rc.1
      short_sha: ""
    permissions:
      contents: read
      packages: write
```

- Use pre-release versions (e.g. `v0.1.0-rc.1`) to avoid colliding with real releases.
- Each sub-workflow supports `workflow_call`, and `release-stage-image.yml` also
  supports `workflow_dispatch`, so steps can be triggered individually from the
  Actions UI after merging.
- Remove test workflows before merging to main.

## Registries

| Namespace | Visibility | Purpose |
|-----------|-----------|---------|
| `ghcr.io/jupyter-infra/staging/` | Private | Pre-release validation |
| `ghcr.io/jupyter-infra/` | Public | Production artifacts |
