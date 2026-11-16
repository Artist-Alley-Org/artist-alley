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
| PR opens / pushes            | `pull_request` | CI on GitHub-hosted runners (`ubuntu-24.04` and `ubuntu-latest`) |
| Push to `dev` / `main`       | `push`         | CI on GitHub-hosted runners (`ubuntu-24.04` and `ubuntu-latest`) |
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
     fan-out `:X.Y.Z / :X.Y / :X / :latest`, plus provenance + SBOM
     (metadata-action's semver patterns drop the leading `v`, so Git tag
     `v0.11.0` publishes Docker tag `:0.11.0`)
   - Sigstore/cosign keyless signatures on every image
   - GitHub Release whose body is the reviewed notes file
     `docs/release-notes/vX.Y.Z.md` from the tagged commit, published
     verbatim with `--notes-file`. Notes are never generated, and the job
     fails before the Release exists if that file is missing or invalid
     (see "Release notes" under "Gates before the tag").

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

### Release notes

The GitHub Release body is human-reviewed public prose, never an
auto-generated list of PR titles. The prose may be drafted by anyone,
including an AI assistant; what makes it releasable is that a person
reviews and approves the wording in the release-prep PR. For tag
`vX.Y.Z` it is the file `docs/release-notes/vX.Y.Z.md`, and
`release.yml` publishes that file verbatim with `--notes-file`. The
first example is `docs/release-notes/v0.11.1.md`, which is the v0.11.1
Release body as reviewed and published.

**What goes in it.** Write plain prose for the people who run Artist
Alley, in the voice of the v0.11.0 and v0.11.1 Releases:
- a `## Artist Alley vX.Y.Z: <theme>` heading;
- a short paragraph on what the release is for;
- a few headed sections on what changed and why it matters;
- the "Full details" link to `CHANGELOG.md` at the tag and the "Every
  merged change" compare link.

It is a summary, not a list of PR titles, and it may state only what the
tagged `CHANGELOG.md` section supports. Keep any caveat the CHANGELOG
carries, such as the provisional status of a dataset edition.

**Procedure:**
1. Add the file in the same release-prep PR that bumps the version
   (`make release`).
2. Get the prose reviewed in that PR like any other public text. The
   review is the gate for the wording. No script judges it.
3. The `release-notes` job in `ci.yml` runs
   `scripts/ci/release_notes.py --package-json web/package.json` on every
   PR and fails while the version in `web/package.json` has no file. A
   version bump without its notes cannot merge green.
4. At the tag, `release.yml` runs
   `scripts/ci/release_notes.py --tag "$TAG" --package-json web/package.json`
   before `gh release create`. It fails, and no Release is created, if:
   - the file for that exact tag is missing, blank or not UTF-8;
   - its first line is not a Markdown heading that names that exact tag
     (`v0.11.1`, not `v0.11.10` or a copied `v0.11.0`);
   - the tag does not agree with `web/package.json`. A stable tag
     `vX.Y.Z` must equal the package version. A prerelease tag
     `vX.Y.Z-rc.1` or `vX.Y.Z-beta.2` may carry either that exact version
     or its base `X.Y.Z`, and needs its own file
     `docs/release-notes/vX.Y.Z-rc.1.md`. A prerelease of any other
     version fails.

   The CI job in step 3 checks the file for the `web/package.json`
   version. With the package at a base version, that is the stable file;
   a prerelease tag cut from it also needs its own file, checked at the tag.

   These checks guard the mechanics only. They do not replace the review.

**Fixing notes after publication.** Edit only the body, from a reviewed
file: `gh release edit vX.Y.Z --notes-file docs/release-notes/vX.Y.Z.md`.
Then read the body back and compare it with the file. Editing the body
re-runs no workflow and does not re-announce the release. Correct the
tracked file in the same change, so the next reader finds the same text.

## Releases that republish the seed dataset

This section is about the **demo seed published on Kaggle**
(`mscrnt/dam-population-seed`), and only for a release that republishes
it. A release that does not republish the demo seed skips this section;
it has no Kaggle step and is not blocked on one.

The **CI datasets** are a different thing. They are the public `site_a`,
`site_a_sm` and `site_b` editions in
`Artist-Alley-Org/artist-alley-dataset-site-a` and `-site-b`, published
as GitHub Releases and pinned in `seed/contracts/ci-datasets.v1.json`
(ADR 0100). They are versioned independently of application releases:
an application tag neither publishes nor republishes them, and a new
edition reaches CI only through a reviewed pin change. Rollback for them
is the pin procedure in ADR 0100, "Rollback". The current editions,
`v2026.11.01` and `v2026.11.01-sm`, were published on 2026-11-13.

When a release republishes the seed dataset, its steps run in this
order, and none starts before the previous one is done:

1. The archive and the seed are verified.
2. The dataset issue closes.
3. The milestone reaches zero open issues.
4. The dataset is published to Kaggle, as `mscrnt/dam-population-seed`.
5. The served dataset is verified.
6. The tag (step 4 of "Cutting a release"), after the gates in
   "Gates before the tag".

The dataset publish is part of this release procedure, not of the
dataset issue. Its acceptance criteria are:

- the dataset description and `ATTRIBUTIONS.md` name the generated source
- the served signature is verified after upload, as at v0.10.2

If a publish or release gate fails after the dataset issue has closed,
the release stops and the failure is reported. The dataset issue is not
silently reopened.

### Scheduled dataset runs and dataset changes

**No current workflow reads a live, unpinned archive.**

- **Pinned editions only.** Since PRs #1533 and #1535 (2026-11-14),
  `ui-pr`, `ui-nightly` and `ui-scale` seed only from the pinned public
  editions above.
- **A corpus change is a pin change.** It is a reviewed PR to
  `seed/contracts/ci-datasets.v1.json`. That PR's own CI fetches and
  verifies the new edition.
- **Code and corpus move together.** Scheduled runs read `main`, so a pin
  change reaches them only together with `main`'s code, and there is never
  a window where old `main` code meets a new corpus.
- **Rollback** is the pin procedure in ADR 0100, "Rollback".

**Schedules are nominal.** `ui-nightly` is scheduled at 07:00 UTC and
`ui-scale` at 02:17 UTC. GitHub may start a scheduled run late or skip it.
Scheduled nightlies have started between about 13:40 and 17:10 UTC, hours
after their 07:00 slot. When a release depends on a scheduled run, check
the actual run and its head SHA, not the clock.

**Historical:** the retired live-archive procedure. Before #1533 and
#1535, the nightly read a live archive mounted on the self-hosted runner,
so promoting a new archive put it in front of the old `main` code until
the release merge. That required a control-first rehearsal, with
"Compatible (Branch A)" and "Incompatible (Branch B)" outcomes and a
06:00 UTC rollback cutoff. No workflow needs it now, so it is no longer
part of this procedure; its text is in this file's git history before
2026-11-15. If a workflow or deployment ever reads an unpinned archive
again, re-establish an equivalent rehearsal in a reviewed change before
the first promotion.

## Versioning

Semver:
- `v0.x.y` — pre-1.0. Schemas + APIs may still break between minors.
- `v1.0.0` — first stable. After that, breaking changes only in majors.

Pre-releases: `v0.1.0-rc.1`, `v0.1.0-beta.2`. GoReleaser auto-detects
`-` suffixes and marks them as GitHub pre-releases; the `:latest` tag
only moves on a non-pre version.

### Release dates

A release's date is the **UTC date of its GitHub release's publish
time** (`published_at`). Use that date in the `CHANGELOG.md` heading
and in the release index in `docs/roadmap.md`. The release-prep PR is
written before the tag exists, so it can only guess; once the release
is published, check the date with
`gh release view vX.Y.Z --json publishedAt` and correct both files if
the guess was a local-time date that differs.

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

## GitHub-hosted runners

Every workflow job on `dev` and `main` runs on a GitHub-hosted runner
(`ubuntu-24.04`, or `ubuntu-latest` for the small gates), so there is no
self-hosted runner to set up. `main` has carried the hosted revisions
since PR #1547 (merge `a379e5cc`, 2026-11-15). The tools come from the runner image
(Docker with `buildx`, `gh`, `jq`, `curl`) or from setup actions in the
workflows (`setup-go`, `setup-node`, `setup-qemu-action`,
`setup-buildx-action`, `cosign-installer`).

- `release.yml` moved to hosted runners on `dev` in PR #1543
  (2026-11-15). A no-push rehearsal of its linux/amd64,linux/arm64 image
  build passed on `ubuntu-24.04` (run 43654921519). The publishing path
  itself (registry push, cosign signing, `gh release create` and the
  Discord announce) has not yet run on a hosted runner; the first real
  tag after this change reaches `main` is its first live test.
- A tag runs `release.yml` as committed at the tagged commit, so the
  hosted version applies only to tags cut after a `dev` to `main`
  promotion that carries it. Before PR #1547, `main` (`2fff3580`) still
  had the earlier self-hosted revisions of `ci.yml`, `edge.yml`,
  `release.yml` and `site-rebuild-trigger.yml`. Every tag cut from
  `main` at or after `a379e5cc` uses the hosted `release.yml`. The first
  such tag is the first live test of its publishing path, so watch both
  release jobs to completion.
- **Dependabot minor and patch PRs can merge themselves.**
  `dependabot-automerge.yml` labels them `automerge` and squash-merges
  them once their CI passes. That bot merge triggers no `dev` push
  workflows, so the resulting `dev` head has no push CI of its own (no
  Edge, no Verify production image build) until the next human merge.
  Check the latest `dev` push runs before promoting.
- With every CI job hosted, a fork PR's code no longer runs on
  self-hosted hardware through these workflows. GitHub's fork-PR approval
  policy still applies.

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
