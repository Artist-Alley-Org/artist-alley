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

**CI pins, downloads, verifies, then extracts.**

- A CI job that consumes a public payload will be configured with three pins:
  - the dataset repository;
  - an immutable version tag, never a mutable `latest`;
  - the expected `release.json` SHA-256.

  The tag name alone is not trusted. Release assets can be replaced under an
  existing tag, so the `release.json` pin is what binds the bytes.
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
  only, with no Release, tag or uploaded archive.

  These are measurements of local candidates. The two current candidates are
  retained privately. They are not release pins, they authorize no upload, and
  they must not be embedded as CI constants. A pin exists only after a reviewed publication.
- No PR subset release exists. `aa seed-package` builds the coverage-selected
  subset package, but it is rights-gated, and its rights ledger is still
  pending, so it refuses to build.

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

- Dependabot pull requests can eventually seed from pinned public data,
  without the private mount secret, once a pinned PR subset release exists
  and CI is wired to it. Fork pull requests remain skipped until a separate
  trusted-runner policy or change permits them. This ADR does not authorize
  running untrusted fork code on trusted runners.
- The Git history stays small, and nothing in it is a binary payload. A clone
  is metadata. The bytes come only from a pinned, verified release.
- A dataset change is a deliberate new version. CI moves to it only by changing
  its pins, and rollback means restoring the previous pins. No published
  version is overwritten.
- Download and verification add CI time, and part splitting is part of every
  build. The cold-start cost is unmeasured until the first CI smoke.
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

None of these is complete. Each must pass, and be recorded, before any upload
or CI change:

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

No local candidate archive authorizes an upload, and the obsolete
pre-exclusion `site_a` candidate must never be uploaded. Nothing in this ADR
calls the current sources rights-cleared.

## Reference

- ADR 0058 (demo seed dataset tiers; proposed `site_b` replacement amendment)
- ADR 0080 (coverage-selected CI fixture; nightly two-contract amendment)
- ADR 0097, ADR 0098 (archive outputs, deterministic assembly)
- `docs/proposals/v0.11.1-ci-datasets-preflight.md`
- `seed/scripts/release_dataset.py`, `seed/scripts/test_release_dataset.py`
- `.github/workflows/ui-pr.yml`, `.github/workflows/ui-nightly.yml`
- GitHub, About releases:
  <https://docs.github.com/en/repositories/releasing-projects-on-github/about-releases>
