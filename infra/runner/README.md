# Self-hosted runner — provisioning

The `AristAlley-Github-Actions-Runner` container on the Unraid
host (`192.168.69.197`) runs `ui-pr.yml` and `ui-nightly.yml`.
It's a `myoung34/github-runner:latest` image with the docker
socket mounted, plus three artist-alley-specific bind-mounts:

| Path on host                                                | Path in runner | Purpose                                                                                                            |
| ----------------------------------------------------------- | -------------- | ------------------------------------------------------------------------------------------------------------------ |
| `/mnt/remotes/192.168.69.145_Archives/datasets`             | `/datasets`    | Read-only seed dataset for `apply.py`. Holds `artist_alley/{site_a,site_b}/`.                                       |
| `/srv/runner-tls`                                           | `/runner-tls`  | Read-only TLS bundle for the dogfood profile. Holds `studio-b.local.pem`, `studio-b.local-key.pem`, `mkcert-rootCA.pem`. |
| `/var/run/docker.sock`                                      | (same)         | Lets the runner drive the host's docker engine to spawn sibling containers + run `docker compose`.                  |

The Unraid container template lives at
`/boot/config/plugins/dockerMan/templates-user/my-AristAlley-Github-Actions-Runner.xml`.

## Re-creating from scratch

If the runner container ever needs to be reprovisioned:

```bash
# 1. The dataset mount is just the existing NFS export. Nothing to
#    re-create — it's already at /mnt/remotes/192.168.69.145_Archives.

# 2. The TLS bundle. Run on the Unraid host once; the certs sit at
#    /srv/runner-tls/ until the cert nears expiry (~2-3 years).
mkdir -p /srv/runner-tls
cd /srv/runner-tls
# mkcert binary (if not already on the host):
wget -q https://github.com/FiloSottile/mkcert/releases/download/v1.4.4/mkcert-v1.4.4-linux-amd64 -O /usr/local/bin/mkcert
chmod +x /usr/local/bin/mkcert
# Generate CA + cert:
mkcert -install
mkcert -cert-file studio-b.local.pem -key-file studio-b.local-key.pem studio-b.local
cp "$(mkcert -CAROOT)/rootCA.pem" /srv/runner-tls/mkcert-rootCA.pem
ls -la /srv/runner-tls/   # should show all 3 files

# 3. Apply the template via Unraid Docker tab → AristAlley-Github-
#    Actions-Runner → Edit → Apply. This recreates the container
#    with all three bind-mounts wired.
```

The runner container exposes the docker socket, so anything we
need is reachable via `docker run` / `docker compose` inside the
runner — no further per-runner tooling installs required.

## Why TLS state is host-side, not in the image

Two reasons:

1. **Rotation is cheap on the host, expensive in the image.** When
   the cert nears expiry we just re-run `mkcert -cert-file …` on
   the Unraid host. Rebuilding a custom runner image to bake new
   certs into it (or finding a registry to push to) is more
   moving parts for the same outcome.
2. **The runner image is upstream-tracked.** `myoung34/github-
   runner:latest` updates regularly. Forking it just to add certs
   would mean re-syncing every update; bind-mounts let us follow
   upstream cleanly.

The mkcert root CA at `/srv/runner-tls/mkcert-rootCA.pem` is
mounted read-only and reaches the dogfood `app` + `app-b`
containers via `infra/docker/dogfood/docker-compose.override.yml`
(`SSL_CERT_DIR=/etc/ssl/certs:/etc/ssl/dogfood`). No
`update-ca-certificates` invocation needed because the
containers' Go binary honours `SSL_CERT_DIR` directly.

## How the workflow consumes this

```yaml
# .github/workflows/ui-nightly.yml — abbreviated
- name: Stage TLS prereqs from the runner bind-mount
  run: |
    mkdir -p infra/docker/dogfood/certs
    cp -f /runner-tls/*.pem        infra/docker/dogfood/certs/
    cp -f /runner-tls/*-key.pem    infra/docker/dogfood/certs/

- name: Bring up the full dogfood stack
  run: ./scripts/dogfood/up.sh
  # up.sh's capability check sees the certs already there and
  # skips the mkcert step entirely — same script, different
  # bring-up path.
```

`scripts/dogfood/up.sh` itself doesn't know or care which path
it's on; it just checks whether the certs exist OR whether
mkcert is available.

## Persistent workspace + stale git locks (#350)

Unlike a GitHub-hosted runner, which hands every job a fresh VM,
our self-hosted runners reuse one workspace directory per repo
(`_work/artist-alley/artist-alley`) across every job they ever
run. That reuse is what makes checkouts fast, and it's also what
lets one job's wreckage break the next one.

Most workflows set `concurrency.cancel-in-progress: true`, so a
superseded run is killed wherever it happens to be — including
mid-`git fetch`. Git writes `.git/index.lock`,
`.git/refs/**/*.lock`, and friends before mutating a ref and
removes them after; a SIGKILL in that window leaves them behind.
Because the workspace persists, the *next* job to land on that
runner hits `actions/checkout` and fails with "Unable to create
'.../index.lock': File exists" — a failure with nothing to do
with the commit under test, on a job whose author changed
nothing.

Every self-hosted job therefore sweeps the locks immediately
before `actions/checkout`:

```yaml
- name: Clear stale git locks (persistent self-hosted workspace)
  run: find "${GITHUB_WORKSPACE}" -type f -name '*.lock' -path '*/.git/*' -delete 2>/dev/null || true
```

The `-path '*/.git/*'` guard matters: it confines the sweep to
git's own metadata so a legitimate `*.lock` file tracked in the
working tree is never touched. The step is idempotent and always
exits 0 — a clean workspace, or one that doesn't exist yet on a
brand-new runner, is a no-op rather than a failure.

As of #942 **every** job in `ci.yml` is self-hosted and every one
of them carries the step. This paragraph previously read "GitHub-
hosted jobs (`ubuntu-latest`, e.g. ci.yml's `codegen-drift`) get a
fresh workspace per run and deliberately do **not** carry the
step" — `codegen-drift` was the last hosted job in that file, and
moving it here is exactly what made the sentence false. The four
remaining `ubuntu-latest` jobs in the repo live in other workflows
(`adr-frontmatter-check.yml`, `dependabot-automerge.yml` ×2,
`mdx-hazard-check.yml`); those do still get a fresh VM per run and
still, correctly, omit the step.

A second thing the reused workspace makes load-bearing:
`actions/checkout`'s `clean` input, which defaults to `true` and
runs `git clean -ffdx && git reset --hard HEAD` before fetching.
On a fresh VM that default is a no-op; here it is the only reason
a generated file left dirty by an earlier job on the same runner
cannot be carried into the next job's `git status --porcelain`.
Do not set `clean: false` on a self-hosted job without working out
what that lets survive between runs.

**Also wired host-side:** `ACTIONS_RUNNER_HOOK_JOB_STARTED`
(`/hooks/job-started.sh`, added 2026-07-16) performs the same
sweep before every job on all three runners, so this is belt and
braces. This section used to file that hook under "future
hardening, not implemented"; it is implemented. The in-workflow
step stays anyway, and is the one to keep if you have to choose:
the hook is host-side state provisioned from the Unraid container
template above, so it would silently not exist on any runner added
without it, whereas the step travels with the repo.

## Tested with

- `myoung34/github-runner:latest` (verified 2026-06-10).
- Unraid 7.x dockerMan.
- mkcert v1.4.4.

## Notes on /etc/hosts

The dogfood profile already declares `studio-b.local` as a
network alias on `nginx-b` (`docker-compose.yml`), so anything
attached to `artist-alley_default` resolves it via docker DNS.
The `/etc/hosts` edit `up.sh` does for local-dev hosts is
unnecessary in CI — the runner attaches to the compose network
in the workflow instead.
