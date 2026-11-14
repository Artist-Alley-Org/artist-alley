---
id: "0100"
title: Public CI datasets are metadata-only repositories with versioned, pinned release payloads
status: proposed
date: 2026-11-12
area: process
phases: []
supersedes: []
related:
  - "0058"
  - "0080"
  - "0097"
  - "0098"
tags:
  - ci
  - seed
  - datasets
  - release
excerpt: >-
  CI needs the public seed datasets without a private runner mount, and the binary
  payload cannot live in Git. Each public dataset repository holds metadata and
  tooling only. The payload ships as versioned GitHub Release assets: ordered parts
  of one deterministic tar.gz stream, each under 2 GiB, described by a release.json.
  CI will pin the repository, the tag and the expected release.json SHA-256, and
  verify every part before extracting or seeding. This records the transport only.
  It grants no rights clearance and publishes nothing.
---

## Context

Issue #1514 moves CI away from datasets mounted from private runner paths.
`ui-pr.yml` mounts `site_a` through the `SEED_SITE_A_PATH` secret and skips its
seeded UI suite when the path is absent (`.github/workflows/ui-pr.yml:43`,
`:135`-`:142`). `ui-nightly.yml` mounts both sites from private paths
(`.github/workflows/ui-nightly.yml:228`-`:229`). Two separate restrictions
keep pull requests from running the seeded suite. Dependabot pull requests
skip it because GitHub withholds the repository secret that supplies the
private `site_a` mount path (`.github/workflows/ui-pr.yml:42`-`:48`). Fork
pull requests do not run the workflow at all, because trusted self-hosted
runners are not available to them (`.github/workflows/ui-pr.yml:38`-`:40`).
These line references describe the workflows as of 2026-11-12, before the
hosted-runner cutover recorded under "CI cutover (staged)" below.

ADR 0058 makes `site_a` (Layer A) the public-safe tier and keeps the current
`site_b` private. Its proposed 2026-11-11 amendment replaces `site_b` with a new
public peer. It also says the release contract and the storage choice are
separate decisions, and that it neither chooses GitHub Releases nor changes
the Kaggle steps in `RELEASING.md`. This ADR is that separate storage
decision. It does not edit ADR 0058.

The constraints measured in the v0.11.1 preflight
(`docs/proposals/v0.11.1-ci-datasets-preflight.md`):

- Three `site_a` files exceed GitHub's 100 MiB ordinary-object limit, so the
  payload cannot be ordinary Git.
- The Git LFS storage and monthly transfer allowance is shared across the
  organization. The preflight read the GitHub Free organization allowance as
  10 GiB each. A full `site_a` payload fetched on every CI run would consume
  a material share of that.
- GitHub's release documentation
  (<https://docs.github.com/en/repositories/releasing-projects-on-github/about-releases>)
  requires each release asset to be under 2 GiB. It states no limit on total
  release size or bandwidth.

The owner chose (2026-11-11):

- two public dataset repositories under Artist-Alley-Org, one per site, holding
  metadata and tooling only;
- the binary payload published as versioned, checksum-verified GitHub Release
  assets;
- no Git LFS and no binary Git history.

A public repository cannot prevent cloning, so a clone must simply contain no
payload and no LFS pointer.

The owner also chose tiered testing. Full `site_a` (about 2,005 assets and 863
posts, after the owner-directed exclusion of the Sponza scene on 2026-11-12)
and the replacement public `site_b` (about 503 assets and 200 posts) are for
scheduled or manually dispatched scale validation. PR CI stays a small,
coverage-complete `site_a` subset. ADR 0080 explains why: seeding the whole
catalogue cost 14 minutes of a 20-minute job. The measured local `site_a`
candidate stream is 2,664,854,328 bytes after the exclusion (2,712,587,909
before it), which is too large to download, verify and extract on every pull
request. The PR subset selected by `--profile ci` with the overlap includes is
164 assets, 84 posts, 204 files and 728,101,752 unique bytes after the
exclusion (165 assets, 87 posts, 275 files and 666,675,617 bytes before it).

## Decision

**Repositories hold metadata and tooling only.**

- Each public dataset repository tracks its top-level metadata files, its
  `SHA256SUMS`, its README and its `.gitignore`. These are ordinary Git blobs.
- The payload directories are present only in a maintainer's local working
  copy, for building releases. They are ignored by root-anchored
  `.gitignore` rules, so `git add -A` cannot stage them.
  - `site_a` payload directories: `3d/`, `audio/`, `documents/`, `fonts/`,
    `images/`, `videos/`.
  - `site_b` payload directory: `images/`.
  - Both repositories use the same root layout (2026-11-13): the site root is
    the repository root. `site_b`'s generated metadata document is
    `DATASET.md`, listed in `SHA256SUMS`, beside a curated, unlisted
    `README.md`. Until then `site_b` kept its site root under `site_b/`.
- Git LFS is not used, and no LFS pointer is committed.

**The payload ships as versioned GitHub Release assets.**

- A release version is one tag on the dataset repository. Its assets are:
  - the sequential parts `<site>-payload.tar.gz.partNNNN` of ONE
    deterministic tar.gz stream of the payload files only;
  - one `release.json`.
- Every part is strictly smaller than the configured limit: default
  1,900,000,000 bytes, never 2 GiB or more.
- `release.json` records:
  - the schema and the site;
  - the `SHA256SUMS` pin (SHA-256, bytes, entries);
  - the metadata files (path, SHA-256, bytes);
  - the payload count, bytes and sorted-list digest;
  - the archive parameters and toolchain;
  - the ordered part names, sizes and SHA-256 values, and the whole stream's
    size and SHA-256;
  - a notice that packaging is not rights clearance or release approval.
- A published version is never edited in place. A changed payload is a new
  version with a new tag.
- **Version tags are calendar editions, `vYYYY.MM.NN`** (owner direction,
  2026-11-13): the year and month of the edition, then a two-digit edition
  number that increases within that month. `v2026.11.01` is the first
  November 2026 edition. A tag versions the DATASET and is independent of the
  application's milestones (for example v0.11.1). Both dataset repositories
  may carry the same edition tag when they are cut together, but each tag
  pins only its own repository's payload.

**CI pins, downloads, verifies, then extracts.**

- A CI job that consumes a public payload will be configured with three pins:
  - the dataset repository;
  - an immutable version tag, never a mutable `latest`;
  - the expected `release.json` SHA-256.

  The tag name alone is not trusted. Release assets can be replaced under an
  existing tag, so the `release.json` pin is what binds the bytes.

  For example, once `v2026.11.01` is published, a `site_a` job would pin
  `Artist-Alley-Org/artist-alley-dataset-site-a`, `v2026.11.01` and
  `release.json` SHA-256 `84996745...`. No CI job is configured with such a
  pin yet.
- The job will:
  1. check out the metadata repository at that tag;
  2. download `release.json` and check it against the pinned SHA-256;
  3. download every listed part, and reject a missing, extra or reordered part;
  4. check every part's size and SHA-256, and the whole stream's SHA-256,
     BEFORE extraction;
  5. extract only listed regular files, into a fresh root;
  6. require the reconstructed root to pass the metadata repository's
     `SHA256SUMS` before seeding.
- A metadata-only clone alone cannot pass `sha256sum -c SHA256SUMS` until the
  matching payload is extracted.

**What exists today, and what does not.**

- `seed/scripts/release_dataset.py` is a LOCAL builder and offline verifier.
  - `build` writes the parts and `release.json`.
  - `verify` checks a local release against a metadata-only root, extracts it
    into a new root and installs that root only after it passes `SHA256SUMS`.
  - It has no network code and knows no URLs. It is not a downloader, and no
    CI job uses it yet.
- On 2026-11-12 it built LOCAL CANDIDATE archives from the local metadata-only
  repositories, and `verify` reconstructed both sites from metadata-only
  copies:

  | | Status | Payload files | Parts | Part bytes | Stream SHA-256 | `release.json` SHA-256 |
  | --- | --- | --- | --- | --- | --- | --- |
  | `site_a` | current, post-Sponza exclusion | 2,180 | 2 | 1,899,999,999 and 764,854,329 | `d99a864052651f29b42b2dd05d5b719da991a98264b93b62b6f9b8a674c3f71c` | `c756c03880abd5729e90b9cd99262c81056965eb4e8c0b3305aed44fcd5220cd` |
  | `site_a` | OBSOLETE, pre-exclusion, contains Sponza: must not be uploaded | 2,251 | 2 | 1,899,999,999 and 812,587,910 | `25a50b8960089d4d9d066a4fc690eec88d73e601b970e3e51d83fc8102ff6a0e` | `8c28c6dcccb692c0c7b3c8858d8d74eeddf419ed675ae3010ae088bf08922e76` |
  | `site_b` | current | 503 | 1 | 3,156,078 | `bc2a609fa88afc3a5424bb54dfde54242f1ad9a94e30718e966ddb09cffb9a85` | `cdc4b78a959461386a9053b92401c6381e86721dfd44db3b1f3e30c656e80184` |

  **The current `site_a` candidate:**
  - **Source:** `site_a` metadata commit
    `249c15fc0d2b7ea13acdbf26f4e5ff90a7796ed4`: 2,005 assets, 863 posts, and
    2,187 `SHA256SUMS` entries (2,180 payload files of 2,709,016,036 bytes).
  - **Why it was built from a private staging root:** the builder refuses
    unlisted payload files, and the 71 excluded Sponza files remain, ignored
    and unlisted, on local disk for reversibility.
  - **Reproducibility:** two independent builds with Python 3.12.3 and zlib
    1.3 were byte-identical.

  The `site_b` candidate was rebuilt after its metadata README and `SHA256SUMS`
  line were corrected. Its payload part is byte-identical to the earlier build.
  After a 2026-11-12 host crash deleted an earlier temporary copy, the archive
  was rebuilt byte-identically from the unchanged `site_b` metadata repository.

  On 2026-11-13 the `site_b` repository was re-laid out at its root (the
  generated README became `DATASET.md`). Its rebuilt candidate's payload part
  is byte-identical to the previous build (`96e2841b...`, 3,160,894 bytes);
  only its `release.json` changed (`a4b1fa20...`), because its metadata pins
  now name `DATASET.md` and the new `SHA256SUMS`.

  Both metadata repositories are public on GitHub since 2026-11-13
  (`Artist-Alley-Org/artist-alley-dataset-site-a` and `-site-b`), metadata
  only. On 2026-11-13 each received ONE DRAFT prerelease GitHub Release,
  `v2026.11.01`, carrying its candidate `release.json` and parts:
  - `site_a`: release id 443965050, target `62ebfa6a...`, `release.json`
    `84996745...`;
  - `site_b`: release id 443964463, target `9fa8fb76...`, `release.json`
    `a4b1fa20...`.

  The drafts were first prepared as `v0.11.1-rc1` and retagged before any
  publication. A draft is not public, creates no Git tag until it is
  published, and is not a pin. On 2026-11-13 the owner accepted the recorded
  source evidence for edition `v2026.11.01` and decided to publish it; see
  "Owner publication decision" under the gates below.

  **Published 2026-11-13.** Both drafts were then published, unchanged, as
  public PRERELEASES. In each repository the tag `v2026.11.01` now exists
  and resolves to the target commit (`site_a` `62ebfa6a...`, `site_b`
  `9fa8fb76...`); release ids, asset ids, names, sizes and digests are the
  ones listed above, and each `main` is unchanged. A fresh anonymous checkout
  at the tag and a fresh network download of every published asset were then
  verified with `seed/scripts/fetch_release_dataset.py` against the pinned
  `release.json` SHA-256s. Both reconstructed roots passed `sha256sum -c`
  (`site_a` 2,189 entries, 2,005 assets and 863 posts; `site_b` 514 entries,
  503 assets and 200 posts). Both overlap anchors were byte-identical across
  the two roots under distinct asset ids, and they were the only two
  cross-peer byte overlaps. The non-overlap control was present only in
  `site_b`. Sponza, the Pexels videos and 2CylinderEngine were absent as
  files and as asset rows in the public `site_a`; they are named only in its
  replacement-provenance fields.

  The private local candidates remain retained and are not themselves pins.
  The pins are the PUBLISHED editions: repository, tag `v2026.11.01`, the
  commit the tag resolves to and the published `release.json` SHA-256, kept in
  one file, `seed/contracts/ci-datasets.v1.json`.
- The PR subset is its own edition, `v2026.11.01-sm` (see "Small PR edition"
  below), published on 2026-11-13 as a public prerelease. `aa seed-package` was
  not used: it is rights-gated and refuses while the ledger is pending. The
  full edition remains the nightly's dataset.

**Test tiers.**

- PR CI keeps a small, coverage-complete `site_a` subset (ADR 0080).
- The accepted nightly contract (ADR 0080, 2026-10-02 amendment) is unchanged:
  `site_a` seeded with `--profile ci`, and `site_b` as a paired peer for peer
  presence (`.github/workflows/ui-nightly.yml:298`, `:306`).
- Full `site_a` and the replacement public `site_b` belong to a SEPARATE
  scheduled or manually dispatched scale job. That job and its tests are not
  written yet.
- The old private `site_b` is excluded entirely: it is in no public repository,
  release asset, CI artifact or log.

## Consequences

- With the hosted cutover (staged, see below), the two UI workflows seed from
  the pinned public editions on GitHub-hosted runners. Dependabot and fork
  pull requests then run the seeded UI suite: it needs no secret and no
  trusted runner, only public data and the read-only token. This ADR still
  does not authorize running untrusted fork code on trusted runners, and the
  cutover does not do so.
- Not every workflow moves. `ci.yml` (including the required
  `Integration tests (full compose stack)` check), `release.yml`, `edge.yml`
  and one job of `site-rebuild-trigger.yml` still run on the self-hosted
  pool. Only the two dataset-consuming UI workflows are hosted.
- The Git history stays small, and nothing in it is a binary payload. A clone
  is metadata. The bytes come only from a pinned, verified release.
- A dataset change is a deliberate new version. CI moves to it only by changing
  its pins, and rollback means restoring the previous pins. No published
  version is overwritten.
- Download and verification add CI time, and part splitting is part of every
  build. The cold-start cost on a hosted runner is unmeasured until the first
  hosted run. On the owner's development machine the CI helper fetched and
  verified `site_a` in about 40 seconds and `site_b` in about 1 second; that
  is not a hosted-runner measurement, and the workflows print free disk and
  per-phase time on every run so the hosted cost is recorded, not assumed.
- Byte-identical rebuilds hold for one Python and zlib toolchain, which
  `release.json` records. A toolchain change may change the bytes but not the
  payload list.
- GitHub's limits can change. The 2 GiB per-asset rule is rechecked before
  each publication.

## Alternatives considered

- **Ordinary Git.** Rejected: files exceed 100 MiB, and binary history grows
  with every revision.
- **Git LFS.** Rejected by the owner: the storage and transfer allowance is
  shared and would be drawn on every CI run, and LFS pointers would still put
  the binary history in the repository's LFS store.
- **Kaggle.** `RELEASING.md` uses it for the published demo dataset. It is not
  chosen for CI, because version-pinned anonymous downloads were not
  established. This ADR does not change the Kaggle procedure, and the two must
  be reconciled before cutover.
- **Public object storage.** It would work, but adds cost, credentials and
  operational burden that Releases do not.
- **Actions artifacts or cache.** Their retention and eviction make them
  unsuitable as the only source. They remain an option for accelerating a
  release that is already verified.

## Gates before publication and cutover

These are the general gates for a public dataset edition. For edition
`v2026.11.01` the owner's 2026-11-13 decision, recorded after the list,
accepts gates 1 and 2 without completing them.

1. **`site_a` evidence reconciliation.** The owner states that the historical
   `site_a` verification is complete. The new CI-release evidence still has to
   bind it to the frozen bytes:
   - the fresh structural draft ledger for the post-exclusion CI selection
     has 204 rows, all `pending`, none reviewed;
   - the private evidence crosswalk (v2) was made for the pre-exclusion
     275-row draft. It lists 24 of those rows as supported candidates, 163
     as needing corroboration, and 88 as conflict or hold candidates;
   - 194 of the 204 current files are byte-identical files from that
     275-row set. The 10 that entered with the exclusion have manifest labels
     and attributions, but no row in that crosswalk, and they are `pending`
     and unreviewed in the new ledger.
2. **`site_b` rights review.** Every file of the replacement public `site_b`
   needs a formal per-file review. All 503 private evidence rows are
   `pending_review`.
3. **Policy.** The ADR 0058 amendment replacing private `site_b` must be
   accepted.
4. **Release verification.** The release manifests and parts must be built
   from the reviewed bytes, and verified against a metadata-only clone.
5. **Publication targets.** Each site needs a distinct public repository
   remote, an immutable tag and the uploaded release assets, all re-verified
   after upload.
6. **CI configuration.** CI must pin the repository, the tag and the expected
   `release.json` SHA-256, with the download-verify-extract steps implemented
   and reviewed.
7. **First runs.** An initial cold CI smoke, then the scale checks for the
   large tier.
8. **Rollback.** The rollback procedure must be documented.

**Owner publication decision for `v2026.11.01` (2026-11-13).** The owner
reviewed the recorded source evidence and elected to publish dataset edition
`v2026.11.01` of both repositories as a provisional public prerelease. For
this edition, gates 1 and 2 are accepted by that decision rather than
completed:

- the per-file ledger rows are NOT all reviewed (the `site_a` CI-selection
  draft is still `pending`, and all 503 `site_b` rows are `pending_review`);
- the unresolved source caveats stay disclosed in the release notes, the
  dataset `README.md`/`ATTRIBUTIONS.md` and the records, and are not silently
  resolved: the 75 NPS clips are PROVISIONAL, the Factory Kit model's source
  review is pending, and `site_b`'s comparison against Kenney's current
  first-party packs was not run.

This is not legal rights clearance. Status of the other gates for this
edition:
- Gate 4 is met by the verified rev115 (`site_a`) and rev125 (`site_b`)
  candidates.
- Gate 5 is met: both public repositories carry a published `v2026.11.01`
  prerelease whose tag resolves to the pinned commit, and a fresh public
  download of every asset was verified against the pinned `release.json`
  SHA-256s (see "Published 2026-11-13" above).
- Gate 6 is staged, not met: the CI wiring below exists in the application
  PR #1533 (open, not merged) and is under review.
- Gates 3, 7 and 8 remain open.

**CI cutover (staged, unmerged).** The owner directed that the dataset CI run
on public GitHub-hosted runners rather than the owner's self-hosted machines.
The staged change:

- `ui-pr.yml` and `ui-nightly.yml` run on `ubuntu-24.04` (4 CPU, 16 GiB RAM,
  14 GiB SSD). They no longer use `SEED_SITE_A_PATH`, `SEED_SITE_B_PATH`,
  `/datasets`, `/runner-tls` or a `docker network connect` of a runner
  container. The `seed-preflight` job that skipped the suite without the
  secret is removed; the job and check names are kept.
- `scripts/ci/public-datasets.sh` reads `seed/contracts/ci-datasets.v1.json`,
  checks that the tag resolves to the pinned commit remotely and in a fresh
  anonymous metadata checkout, runs `seed/scripts/fetch_release_dataset.py`
  with the pinned `release.json` SHA-256 into a new root, makes the tree
  readable by the containers' nonroot user (mode bits only) and re-runs
  `sha256sum -c` on the tree that is mounted. Any failure stops the job.
- `infra/docker/ci/docker-compose.hosted.yml` publishes `app`, `nginx` and
  `nginx-b` on loopback at their in-network ports, and the VM's `/etc/hosts`
  maps their names to 127.0.0.1, so each URL means the same thing from the
  runner and from the containers. The apps still federate over Docker DNS at
  the same peer URLs.
- The nightly's `studio-b.local` certificate comes from a per-run throwaway CA
  (`scripts/ci/ephemeral-studio-b-tls.sh`) whose key is destroyed after
  signing.
- The seed contracts are unchanged: PR `site_a --profile ci --fixtures`;
  nightly `site_a --profile ci --fixtures` and `site_b --limit-per-extension 3
  --fixtures`, now against the public replacement `site_b`.
- `ui-pr` fetches the small edition (pins key `site_a_sm`, tag
  `v2026.11.01-sm`, passed to the fetch tool as dataset site `site_a`);
  `ui-nightly` keeps the full `site_a` and `site_b` `v2026.11.01` editions.
  Pin validation lives in `seed/scripts/ci_pins.py`: full editions accept only
  `vYYYY.MM.NN`, and `site_a_sm` only `vYYYY.MM.NN-sm`.
- Disk is the binding constraint on the advertised standard runner (14 GB).
  The jobs are ordered by disk phase: build the image(s), prune the job's own
  build cache, then fetch, seed and release the dataset. A phase model
  (`seed/scripts/ci_capacity.py`, inputs in the pins file's `capacity_model`)
  takes the peak per phase rather than one sum, and two checks gate on free
  space MEASURED on the runner: a whole-job preflight before the build and a
  binding check after the build. Neither deletes anything to make room. The
  build inputs were measured on a clean overlay2 daemon (cold builds, `du` of
  the Docker root before and after the cache prune):

  | profile | build peak | kept after prune | whole-job need | need after build |
  | --- | --- | --- | --- | --- |
  | `ui-pr` (small edition) | 5,702,983,524 | 1,758,671,905 | 8,176,725,348 | 5,684,354,560 |
  | `ui-nightly` | 8,679,315,187 | 2,981,508,047 | 12,913,080,735 | 8,431,398,082 |

  Both fit 14,000,000,000 bytes; the nightly has about 1.1 GB of headroom. With
  the full edition, `ui-pr` needed 9,956,057,251 and 6,797,385,346. The
  earlier staged arithmetic (every download and extracted byte plus an 8 or
  10 GiB Docker floor, all at once) needed 14,607,515,312 and 16,761,559,854
  bytes and could not pass. The seed growth, postgres, Playwright and margin
  terms are partly allowances, and the hosted Docker storage layout may
  differ from the measured one; the first hosted runs replace them with
  measurements.

**Small PR edition `v2026.11.01-sm` (published 2026-11-13, public prerelease).** By
owner direction, PR CI uses a smaller `site_a` edition; the full edition stays
the nightly's. It is a byte-identical subset of `v2026.11.01`: the rows and
files `aa seed --profile ci` selects (application `2421d6d8`), the two declared
overlap anchors and the selected rows' licence notice.

- 165 of 2,005 assets, 91 of 863 posts, 9 of 92 videos, 207 payload files
  (665,709,517 bytes); 131 of 131 coverage dimensions; both anchors; every
  included row byte-identical to `v2026.11.01`.
- Its descriptive files describe the subset: `groups.csv` `asset_count` and
  `metadata.csv` `group_size` are the retained members; `ATTRIBUTIONS.md`
  summarises the included rows' licence and attribution values and keeps the
  full edition's text as labelled background; `dataset-metadata.json`
  describes the subset; `EDITION.json` records the source pins, the selection
  and every change. The rights caveats are carried unchanged.
- Release (built with `release_dataset.py`): public prerelease
  <https://github.com/Artist-Alley-Org/artist-alley-dataset-site-a/releases/tag/v2026.11.01-sm>,
  release id 444795243. Its `release.json` has SHA-256
  `c6838e5020b1bc56f048c0602c2021c1c840e3e3ddf236827eb7358b521a503f`, and its
  one part is 658,160,717 bytes, SHA-256
  `139292dcd564bff3b513356087acf9992544b0e4b9a3a5e8f8f412d856f31f88`.
- Metadata: commit `3f078b1500ce066d5df04adeba11d38fc8ad7277` on the public
  branch `edition/v2026.11.01-sm` of the `site_a` repository (parent
  `62ebfa6a`, 11 metadata files, no payload), with the public lightweight tag
  `v2026.11.01-sm` on it. `main` and `v2026.11.01` are unchanged.
- Verified before publication: the tagged metadata plus the release rebuild a
  root that passes all 215 checksums; the CI selection on it equals the
  full-edition coverage selection; `aa seed-overlap` passes against the public
  `site_b`; an isolated `aa seed --profile ci --fixtures` covered 131 of 131.
- Verified after publication: an anonymous fetch of the public edition through
  `scripts/ci/public-datasets.sh fetch site_a_sm` (tag check, metadata
  checkout at the pinned commit, download and verification against the pinned
  `release.json`) passed. The rebuilt root passed all 215 checksums, held 165
  assets, 91 posts, 9 videos and both overlap anchors, and covered 131 of 131
  CI dimensions; the downloaded assets' digests equal the published ones. The
  temporary copy was deleted afterwards.

The CI cutover is committed on the feature branch and open as PR #1533 to
`dev`; it is not merged, so the merged CI still runs the old workflows. Gate
7's cold hosted smoke is NOT passed: the first hosted runs, `ui-pr` 43520041934 and `ui-nightly` 43520575280 (a dispatch of the feature branch), both on commit 232cbb44, fetched and verified the
pinned public editions, passed both capacity checks and seeded, then failed at
the bounded preview-render drain. Each gate started with 112 gated jobs and
still had 81 outstanding when the 480 s cap expired (slowest `preview.video`
271.5 s and 290.1 s), so Playwright never ran. The nightly's federation
bring-up and shell scenarios passed. A correction raising the cap to 1,500 s
and giving the hosted browser cache its own key is under review; no hosted
Playwright run has passed yet. The release notes, which describe the merged
CI, still say that no CI job uses these editions; they stay unchanged until
the cutover is merged.

The next steps are: review the drain and cache correction on PR #1533, get a
hosted `ui-pr` and nightly run to pass, merge, then update the release notes
and close gates 7 and 8.

A local candidate archive by itself authorizes no upload; for `v2026.11.01`
the owner's decision does. The obsolete pre-exclusion `site_a` candidate
must never be uploaded. Nothing in this ADR calls the current sources
rights-cleared.

## Reference

- ADR 0058 (demo seed dataset tiers; proposed `site_b` replacement amendment)
- ADR 0080 (coverage-selected CI fixture; nightly two-contract amendment)
- ADR 0097, ADR 0098 (archive outputs, deterministic assembly)
- `docs/proposals/v0.11.1-ci-datasets-preflight.md`
- `seed/scripts/release_dataset.py`, `seed/scripts/test_release_dataset.py`
- `seed/scripts/fetch_release_dataset.py`, `seed/scripts/test_fetch_release_dataset.py`
- `seed/contracts/ci-datasets.v1.json`, `scripts/ci/public-datasets.sh`,
  `scripts/ci/ephemeral-studio-b-tls.sh`, `infra/docker/ci/docker-compose.hosted.yml`
- `.github/workflows/ui-pr.yml`, `.github/workflows/ui-nightly.yml`
- GitHub, About releases:
  <https://docs.github.com/en/repositories/releasing-projects-on-github/about-releases>
