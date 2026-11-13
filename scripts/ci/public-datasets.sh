#!/usr/bin/env bash
# scripts/ci/public-datasets.sh: public seed datasets for the hosted UI workflows (#1514, ADR 0100).
#
#   public-datasets.sh preflight <profile>   BEFORE the image build: predicted whole-job disk peak vs measured free
#   public-datasets.sh prepare <profile>     AFTER the build + build-cache prune: measured free vs the dataset
#                                            phases; picks + creates the dataset root
#   public-datasets.sh fetch <site>          pinned checkout + verified download
#   public-datasets.sh release               delete the dataset root (idempotent)
#
# <profile> is a workflow profile in the pins file's `capacity_model` (ui-pr, ui-nightly). The phase
# model, its inputs and their measurements are documented in seed/scripts/ci_capacity.py, which does the
# arithmetic. Both checks use free space MEASURED at that moment and fail with the numbers; nothing is
# ever deleted to make room.
#
# Every pin comes from seed/contracts/ci-datasets.v1.json: repository, immutable tag, the metadata
# commit the tag must resolve to, and the release.json SHA-256. The bytes are downloaded and verified
# by the reviewed seed/scripts/fetch_release_dataset.py; this script only wires it up. It never uses a
# token, never asks for `latest`, and prints no URL (the release asset redirects carry signed query
# strings, so nothing here echoes a request or response URL).
#
# State between steps lives in $AA_CI_DATASET_ROOT, exported through $GITHUB_ENV by `prepare`:
#   $AA_CI_DATASET_ROOT/meta/<site>    metadata-only clone at the pinned tag (HEAD == pinned commit)
#   $AA_CI_DATASET_ROOT/dl/            parent of the fetch tool's private download dir (removed by it)
#   $AA_CI_DATASET_ROOT/sites/<key>    the verified, reconstructed site root that `aa seed` mounts
#
# Fails closed: any clone, pin, download, checksum, extraction or capacity problem exits non-zero and
# names what failed.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
PINS="${ROOT}/seed/contracts/ci-datasets.v1.json"
FETCH="${ROOT}/seed/scripts/fetch_release_dataset.py"
CAPACITY="${ROOT}/seed/scripts/ci_capacity.py"
PINS_TOOL="${ROOT}/seed/scripts/ci_pins.py"
ROOT_NAME="aa-ci-datasets"
GIB=$((1024 * 1024 * 1024))

die() { echo "::error title=public dataset::$*" >&2; exit 1; }
note() { echo "public-datasets: $*"; }

# pin <key> <field>: one format-checked value from the pins file (seed/scripts/ci_pins.py), so a malformed
# pin fails here, before any network request. <key> is a dataset key: site_a, site_b or site_a_sm.
pin() {
    python3 -I "$PINS_TOOL" "$PINS" "$1" "$2"
}

check_site() {
    case "$1" in site_a|site_b|site_a_sm) ;; *) die "unknown dataset '$1' (want site_a, site_b or site_a_sm)" ;; esac
}

avail_bytes() { df -B1 --output=avail "$1" | tail -1 | tr -d ' '; }
fs_of() { df --output=source "$1" | tail -1; }

diagnostics() {
    note "capacity: $(nproc) CPU, $(awk '/MemTotal/ {printf "%.1f GiB", $2/1048576}' /proc/meminfo) RAM"
    df -B1M / "${RUNNER_TEMP:-/tmp}" /mnt 2>/dev/null | sed 's/^/  /' || true
}

docker_root() {
    local d
    d="$(docker info --format '{{.DockerRootDir}}' 2>/dev/null)" || die "docker info failed"
    [ -d "$d" ] || d=/
    echo "$d"
}

check_profile() {
    case "$1" in ui-pr|ui-nightly) ;; *) die "unknown profile '$1' (want ui-pr or ui-nightly)" ;; esac
}

profile_sites() {
    local out
    out="$(python3 -I "$CAPACITY" sites "$1" --pins "$PINS")" || die "capacity model has no valid profile '$1'"
    echo "$out"
}

cmd_preflight() {
    [ $# -eq 1 ] || die "usage: preflight <profile>"
    local profile="$1"; check_profile "$profile"
    local sites s
    sites="$(profile_sites "$profile")"
    for s in $sites; do check_site "$s"; pin "$s" need_bytes >/dev/null; done
    diagnostics
    local docker_dir
    docker_dir="$(docker_root)"
    note "docker root ${docker_dir} on $(fs_of "$docker_dir")"
    python3 -I "$CAPACITY" preflight "$profile" --pins "$PINS" --free "$(avail_bytes "$docker_dir")" \
        || die "capacity preflight failed for ${profile} before the build (numbers above)"
}

cmd_prepare() {
    [ $# -eq 1 ] || die "usage: prepare <profile>"
    local profile="$1"; check_profile "$profile"
    local sites s
    sites="$(profile_sites "$profile")"
    for s in $sites; do check_site "$s"; pin "$s" need_bytes >/dev/null; done
    diagnostics
    timeout 60 docker system df 2>/dev/null | sed 's/^/  /' || true
    local docker_dir
    docker_dir="$(docker_root)"

    # Candidate parents for the dataset root: the runner's temp dir, and /mnt when it exists (not assumed
    # to be a separate disk). Pick the one with the most free space.
    # AA_CI_DATASET_PARENTS (space-separated) replaces the list, for a local run of this script.
    local best="" best_avail=0 c a candidates
    if [ -n "${AA_CI_DATASET_PARENTS:-}" ]; then
        read -r -a candidates <<< "$AA_CI_DATASET_PARENTS"
    else
        candidates=("${RUNNER_TEMP:-}" /mnt)
    fi
    for c in "${candidates[@]}"; do
        [ -n "$c" ] && [ -d "$c" ] || continue
        a="$(avail_bytes "$c")"
        if [ "$a" -gt "$best_avail" ]; then best="$c"; best_avail="$a"; fi
    done
    [ -n "$best" ] || die "no candidate directory for the dataset root"

    local same=()
    if [ "$(fs_of "$best")" = "$(fs_of "$docker_dir")" ]; then same=(--same-fs); fi
    note "dataset root parent ${best} on $(fs_of "$best"); docker root ${docker_dir} on $(fs_of "$docker_dir")"
    python3 -I "$CAPACITY" prepare "$profile" --pins "$PINS" --dataset-free "$best_avail" \
        --docker-free "$(avail_bytes "$docker_dir")" "${same[@]}" \
        || die "capacity check failed for ${profile} after the build (numbers above)"

    local root="${best%/}/${ROOT_NAME}"
    if [ "$best" = /mnt ] && [ ! -w /mnt ]; then
        # The hosted VM's scratch disk is root-owned; create only our own directory on it.
        sudo install -d -m 0755 -o "$(id -u)" -g "$(id -g)" "$root"
    fi
    [ ! -e "${root}/sites" ] || die "dataset root ${root} already holds sites; refusing to reuse it"
    install -d -m 0755 "$root" "${root}/meta" "${root}/dl" "${root}/sites"
    echo "AA_CI_DATASET_ROOT=${root}" >> "${GITHUB_ENV:-/dev/null}"
    export AA_CI_DATASET_ROOT="$root"
    note "dataset root: ${root}"
}

require_root() {
    [ -n "${AA_CI_DATASET_ROOT:-}" ] || die "AA_CI_DATASET_ROOT is not set; run 'prepare' first"
    case "$AA_CI_DATASET_ROOT" in */"${ROOT_NAME}") ;; *) die "AA_CI_DATASET_ROOT does not end in /${ROOT_NAME}" ;; esac
    [ -d "$AA_CI_DATASET_ROOT" ] || die "dataset root ${AA_CI_DATASET_ROOT} is missing"
}

cmd_fetch() {
    [ $# -eq 1 ] || die "usage: fetch <site>"
    local site="$1"; check_site "$site"; require_root
    local repo tag commit sha dsite meta out t0
    repo="$(pin "$site" repo)"; tag="$(pin "$site" tag)"
    commit="$(pin "$site" commit)"; sha="$(pin "$site" release_json_sha256)"
    dsite="$(pin "$site" site)"
    meta="${AA_CI_DATASET_ROOT}/meta/${site}"; out="${AA_CI_DATASET_ROOT}/sites/${site}"
    [ ! -e "$meta" ] && [ ! -e "$out" ] || die "${site}: a previous checkout or site root exists; refusing to reuse it"

    # 1. Metadata-only checkout at the pinned tag. Anonymous: no credential helper, no auth header,
    #    no prompt. The tag must resolve to the pinned commit both remotely and in the clone.
    t0=$(date +%s)
    local remote_sha
    remote_sha="$(GIT_TERMINAL_PROMPT=0 git -c credential.helper= -c http.extraHeader= \
        ls-remote --tags "https://github.com/${repo}.git" "refs/tags/${tag}" | awk '{print $1}')" \
        || die "${site}: could not list the remote tag"
    [ "$remote_sha" = "$commit" ] || die "${site}: tag ${tag} resolves to '${remote_sha}', not the pinned ${commit}"
    GIT_TERMINAL_PROMPT=0 git -c credential.helper= -c http.extraHeader= -c advice.detachedHead=false \
        clone -q --depth 1 --single-branch --no-tags --branch "$tag" "https://github.com/${repo}.git" "$meta" \
        || die "${site}: metadata clone of ${repo}@${tag} failed"
    local head
    head="$(git -C "$meta" rev-parse HEAD)"
    [ "$head" = "$commit" ] || die "${site}: checkout HEAD ${head} is not the pinned ${commit}"
    note "${site}: ${repo}@${tag} metadata HEAD ${head} ($(git -C "$meta" ls-files | wc -l) tracked files, $(( $(date +%s) - t0 ))s)"

    # 2. Download + verify + extract into a NEW site root (the reviewed tool refuses an existing one).
    t0=$(date +%s)
    python3 -I "$FETCH" --repo "$repo" --tag "$tag" --site "$dsite" --expect-manifest-sha256 "$sha" \
        --metadata-root "$meta" --output "$out" --work-dir "${AA_CI_DATASET_ROOT}/dl" \
        || die "${site}: download or verification failed (see the fetch_release_dataset message above)"
    note "${site}: downloaded + verified in $(( $(date +%s) - t0 ))s"
    [ -z "$(ls -A "${AA_CI_DATASET_ROOT}/dl")" ] || die "${site}: download directory was not cleaned up"

    # 3. Readable by the containers' nonroot `app` user. Mode bits only: no byte changes. The tool
    #    creates files 0644 and dirs 0755 under the caller's umask; this makes that independent of it.
    chmod -R u=rwX,go=rX "$out"

    # 4. Independent re-check on the tree that will actually be mounted.
    t0=$(date +%s)
    ( cd "$out" && sha256sum --quiet --strict -c SHA256SUMS ) || die "${site}: sha256sum -c failed after the mode change"
    local listed present
    listed="$(wc -l < "${out}/SHA256SUMS")"
    present="$(find "$out" -type f | wc -l)"
    [ "$present" -eq $((listed + 1)) ] || die "${site}: ${present} files present, expected ${listed} listed + SHA256SUMS"
    note "${site}: sha256sum -c OK on ${listed} files, $(du -sh "$out" | cut -f1) on disk ($(( $(date +%s) - t0 ))s)"
    df -h "$AA_CI_DATASET_ROOT" | tail -1 | sed 's/^/  /'
}

cmd_release() {
    [ -n "${AA_CI_DATASET_ROOT:-}" ] || { note "no dataset root to release"; return 0; }
    case "$AA_CI_DATASET_ROOT" in */"${ROOT_NAME}") ;; *) die "refusing to delete ${AA_CI_DATASET_ROOT}" ;; esac
    if [ -e "$AA_CI_DATASET_ROOT" ]; then
        rm -rf -- "$AA_CI_DATASET_ROOT"
        note "released ${AA_CI_DATASET_ROOT}"
    else
        note "${AA_CI_DATASET_ROOT} already released"
    fi
}

case "${1:-}" in
    prepare) shift; cmd_prepare "$@" ;;
    fetch)   shift; cmd_fetch "$@" ;;
    preflight) shift; cmd_preflight "$@" ;;
    release) shift; cmd_release "$@" ;;
    *) die "usage: public-datasets.sh {preflight <profile>|prepare <profile>|fetch <site>|release}" ;;
esac
