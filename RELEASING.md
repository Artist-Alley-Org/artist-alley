# Releasing artist-alley

The branch model is `feat/* → dev → main`, with releases cut from
`main` via a semver tag. Every release artefact comes from the same
git tag.

## Branch lifecycle

```
feat/foo  ─┐
feat/bar  ─┼─→ dev ─→ main ─→ tag vX.Y.Z ─→ release artefacts
feat/baz  ─┘    (continuous edge images)
                                            (binaries, deb, rpm,
                                             brew, signed images,
                                             GitHub Release)
```

| Action                       | Trigger        | Output |
|------------------------------|----------------|--------|
| PR opens / pushes            | `pull_request` | CI on `ubuntu-latest` |
| Push to `feat/*` / `dev` / `main` | `push`    | CI on self-hosted runner |
| Push to `dev` (post-merge)   | `push: [dev]`  | `:edge` + `:edge-{sha}` images |
| Push tag `vX.Y.Z`            | `push: tags`   | Full release matrix |

CI runs first on every change. Branch-image builds + release
publication are gated on CI green.

## Cutting a release

1. Make sure `dev` is healthy: CI green, edge image deployed to your
   staging if you have one.
2. Open the merge PR `dev → main`. Get CI green.
3. Merge with a merge commit (not squash) so the changelog can group
   commits by conventional-commit type.
4. Locally:

   ```bash
   git checkout main
   git pull
   git tag -s -a v0.1.0 -m "v0.1.0"
   git push origin v0.1.0
   ```

   (`-s` requires a GPG key — drop it if you haven't set one up yet.
   We sign images via Sigstore separately, so the tag signature is a
   bonus rather than a gate.)

5. The Release workflow takes over and publishes:
   - Static binaries for `linux/{amd64,arm64}`, `darwin/{amd64,arm64}`,
     `windows/amd64` (`.tar.gz` / `.zip`)
   - `.deb` + `.rpm` packages (amd64 + arm64)
   - Homebrew formula → `Artist-Alley-Org/homebrew-tap` (if `HOMEBREW_TAP_TOKEN`
     is set)
   - Multi-arch Docker images to `ghcr.io/mscrnt/artist-alley` and
     `docker.io/${DOCKERHUB_USERNAME}/artist-alley`
   - SHA256SUMS + SBOM
   - GitHub Release with auto-generated changelog
   - Cosign keyless signatures on every image

## Versioning

Semver:
- `v0.x.y` — pre-1.0. Schemas + APIs may still break between minors.
- `v1.0.0` — first stable. After that, breaking changes only in majors.

Pre-releases: `v0.1.0-rc.1`, `v0.1.0-beta.2`. GoReleaser auto-detects
`-` suffixes and marks them as GitHub pre-releases; the `:latest` tag
only moves on a non-pre version.

## Hotfix flow

For a fix that can't wait for the next `dev → main` merge:

1. Branch off `main`: `git checkout -b fix/critical-thing main`
2. PR the fix into both `main` and `dev` (or merge to main, then
   merge main back into dev).
3. Tag a patch release: `v0.1.1`.

## Secrets the workflows need

| Secret                 | Required? | What it does |
|------------------------|-----------|--------------|
| `GITHUB_TOKEN`         | (auto)    | GHCR push, Release upload, OIDC for Sigstore |
| `DOCKERHUB_USERNAME`   | optional  | Docker Hub username (mirror images there too) |
| `DOCKERHUB_TOKEN`      | optional  | Docker Hub PAT scoped to push only |
| `HOMEBREW_TAP_TOKEN`   | optional  | PAT (`repo` scope) for `Artist-Alley-Org/homebrew-tap` |

When optional secrets are unset, the matching steps no-op silently.

## Self-hosted runner setup

Label your runner `self-hosted,artist-alley`. The runner needs:

- Docker (with `buildx` and `qemu` plugins for cross-arch builds)
- Go (matches `app/go.mod`'s declared version)
- Node 22
- `goreleaser` (the `goreleaser-action` will install if missing)
- `cosign` (the `cosign-installer` action will install if missing)

PR workflows run on GitHub-hosted `ubuntu-latest` to keep fork PRs
from touching your hardware.

## Local dry-run

You can rehearse the release without pushing anything:

```bash
# Build the SvelteKit bundle into the embed dir
cd web && npm ci && npm run build && cd ..
mkdir -p app/internal/http/static_assets
cp -r web/build/. app/internal/http/static_assets/

# Snapshot — runs the full GoReleaser pipeline minus the publish
goreleaser release --snapshot --clean
ls -la dist/
```
