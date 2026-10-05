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

The edge build skips docs-only pushes to `dev` (`paths-ignore` in
`.github/workflows/edge.yml`), so `:edge-{sha}` exists only for
commits that change the application — a docs commit would produce a
byte-identical image, and building it used to cancel the edge run
still going for the code merge ahead of it. `dev`'s tip is frequently
a close-out docs commit, so do not assume `git rev-parse dev` names a
pullable tag.

## Cutting a release

1. Make sure `dev` is healthy: CI green, edge image deployed to your
   staging if you have one.
2. Open the merge PR `dev → main`. Get CI green.
3. Merge with a merge commit (not squash) so the changelog can group
   commits by conventional-commit type. The `main` ruleset now restricts
   `allowed_merge_methods` to `merge` alone, so this is enforced rather
   than remembered.

   > **If the PR reports `BEHIND`:** `main` carries one merge commit per
   > previous release that `dev` never sees, so `dev` is permanently
   > "behind" as a bookkeeping artifact. This blocked the v0.7.0
   > promotion. The `strict_required_status_checks_policy` that caused it
   > was **removed on 2026-07-28** — it bought nothing here, because
   > `main` only ever receives merges *from* `dev` and the checks already
   > ran against dev's exact content. If it is ever re-enabled, the fix is
   > `gh api repos/.../merges -X POST -f base=dev -f head=main` (merging
   > main into dev via the API, no local tree mutation), **not** weakening
   > the rule to get the merge through.
4. Create the annotated tag **via the API**, not locally:

   ```bash
   MAIN=$(git rev-parse origin/main)
   TAG=$(gh api repos/Artist-Alley-Org/artist-alley/git/tags -X POST \
     -f tag=v0.7.0 -f message="v0.7.0 — <headline>" \
     -f object=$MAIN -f type=commit --jq .sha)
   gh api repos/Artist-Alley-Org/artist-alley/git/refs -X POST \
     -f ref=refs/tags/v0.7.0 -f sha=$TAG
   ```

   The API path is preferred over `git tag && git push` because the
   shared working tree is frequently checked out on a verification
   branch, and a stray local tag push is awkward to undo. Both produce
   an identical annotated tag object; verify with
   `git cat-file -t v0.7.0` → `tag` (not `commit`).

   Image signing is Sigstore/cosign, separate from the tag, so a GPG
   tag signature is a bonus rather than a gate.

5. The Release workflow takes over and publishes:
   - Multi-arch Docker images (`linux/amd64,linux/arm64`) to
     `ghcr.io/artist-alley-org/artist-alley` and Docker Hub, with the tag
     fan-out `:vX.Y.Z / :vX.Y / :vX / :latest`, plus provenance + SBOM
   - Sigstore/cosign keyless signatures on every image
   - GitHub Release with auto-generated notes

   **`assets=0` on the GitHub Release is expected.** Docker is the sole
   distribution channel right now. Static binaries, `.deb`/`.rpm` and
   Homebrew were trimmed in #238: `Artist-Alley-Org/webp` is a CGO
   dependency, goreleaser needed `CGO_ENABLED=0`, and CGO cannot be
   cross-compiled — so each target must build on its own platform.
   Restoring them is #286; the macOS arm64 half is #687 (an M1 runner).

   The Release page publishes as soon as the notes job finishes, while
   the multi-arch image job is still running. **The release is not done
   until that second job completes** — `:latest` only moves at the end,
   and the demo's autoupdate keys off `:latest`.
   - Cosign keyless signatures on every image

## Gates before the tag

Step 4 (the tag) waits for every gate below. Each is a stop condition,
not a reminder: if one is not met, the release stops there.

### Dependabot pull requests

No tag, starting with v0.11.0, while an applicable Dependabot pull
request is still open. A Dependabot PR is resolved only when it is
either:

- merged, with its change on the release line and the required CI
  green; or
- deliberately closed or superseded, with the reason written on the PR.

Dependabot PRs are release gates, not milestone issues. They are never
added to a milestone and never counted in the milestone's open set; the
milestone reaching zero open issues does not clear them.

### Post-merge CI on `main`

After the `dev -> main` merge in step 3, the CI runs on the resulting
`main` commit must be green before the tag in step 4 is created.

## Releases that republish the seed dataset

When a release republishes the seed dataset, its steps run in this
order, and none starts before the previous one is done:

1. The archive and the seed are verified.
2. The dataset issue closes.
3. The milestone reaches zero open issues.
4. The dataset is published to Kaggle, as `mscrnt/dam-population-seed`.
5. The served dataset is verified.
6. The tag (step 4 above), after the gates in "Gates before the tag".

The dataset publish is part of this release procedure, not of the
dataset issue. Its acceptance criteria are:

- the dataset description and `ATTRIBUTIONS.md` name the generated source
- the served signature is verified after upload, as at v0.10.2

If a publish or release gate fails after the dataset issue has closed,
the release stops and the failure is reported. The dataset issue is not
silently reopened.

### The scheduled nightly and a promoted archive

The scheduled federation nightly runs `main`'s code against the live
archive. Promoting a new archive before the release merge therefore puts
the new corpus in front of the old `main` code until that merge, so a
promotion is preceded by a rehearsal of the current `main` nightly
against the staged corpus:

- **Control first.** The same `main` SHA is run against the current
  corpus. A green control is required before any conclusion is drawn. A
  red control makes the rehearsal inconclusive, not an infrastructure
  failure by default: its logs are inspected, one controlled rerun is
  allowed only for a demonstrated transient flake, and a real defect is
  resolved through the normal reviewed workflow first.
- **Compatible (Branch A)** means the control and the test run are both
  green. That evidence is valid only while `main` stays at the exact
  rehearsed SHA and the live archive stays byte-identical to the
  rehearsed corpus. Both are checked before every scheduled nightly
  until the release merge. If either changes, the evidence lapses at
  once: the new `main` SHA is rehearsed before a nightly may consume the
  corpus, or the promotion is reversed.
- **Incompatible (Branch B)** means release-readiness comes before the
  promotion: the Dependabot gate resolved, the version bump landed on
  `dev`, and the release PR open with its checks green. The promotion
  then runs inside one bounded window that starts after that day's
  scheduled nightly has concluded and ends at the release merge, with a
  06:00 UTC rollback cutoff ahead of the nightly's 07:00 UTC schedule.

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
- Node 24 (Active LTS)
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
