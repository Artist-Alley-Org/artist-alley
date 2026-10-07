# Installing artist-alley

Self-hosted art review and archival, distributed as:

| Channel       | Best for                            | Pull / install |
|---------------|-------------------------------------|----------------|
| Docker image  | Most users — fastest path           | `docker pull ghcr.io/artist-alley-org/artist-alley:latest` |
| Docker Hub    | Same image, alternate registry      | `docker pull mscrnt/artist-alley:latest` |
| Source        | Building the image yourself         | clone the repo, then `docker build -t artist-alley:local .` at the repo root (see [Building from source](#building-from-source)) |

Docker is the sole v0.1.0 distribution channel. All images are
multi-arch (`linux/amd64` + `linux/arm64`) and signed via Sigstore
(`cosign verify`).

> **Binary / package distribution (.tar.gz, .deb, .rpm, Homebrew) is
> deferred post-v0.1.0.** The Go binary needs a cgo build against
> `libwebp` for the variant encoder, which is straightforward inside
> the Docker image build but not for cross-arch tarball distribution
> on the release runner. Docker gets us there without the plumbing.

---

## What you need before installing

- **PostgreSQL 15 or newer, with the `pgvector` extension (0.5.0 or
  newer) available.** Bring your own; artist-alley creates its own
  schema on first run via embedded migrations. The schema uses
  `UNIQUE NULLS NOT DISTINCT` constraints, which need PostgreSQL 15,
  and an HNSW index, which needs pgvector 0.5.0. The migrations also
  enable `pg_trgm`, `pgcrypto`, and `uuid-ossp`, which ship with a
  standard Postgres install, but `pgvector` must be installed
  separately; the easiest path is one of the
  [`pgvector/pgvector`](https://hub.docker.com/r/pgvector/pgvector)
  images instead of vanilla `postgres`.
- **On a fresh database, the role artist-alley connects as must be a
  superuser.** The first migration creates the `pgvector` extension
  and sets a comment on it. Postgres lets only a superuser create
  pgvector (it is not a trusted extension), and only the extension's
  owner set that comment, so creating the extension in advance as a
  different superuser is not enough. The official `postgres` and
  `pgvector/pgvector` images make `POSTGRES_USER` a superuser, which
  is why the reference Compose stack works as is.
- A directory for **storage** (binary blobs + generated thumbnails)
  or an S3-compatible bucket
- A **password-hashing pepper** (`AA_SCRAMBLE_KEY`, 32 hex bytes:
  `openssl rand -hex 32`). Keep it stable across restarts.
- An **at-rest master key** (base64-encoded 32 bytes —
  `openssl rand -base64 32`)

The web frontend is **embedded into the binary** — there's nothing
separate to deploy.

---

## Docker (recommended)

```bash
docker run -d \
  --name artist-alley \
  -p 8080:8080 \
  -e AA_DB_HOST=postgres.local \
  -e AA_DB_USER=artist_alley \
  -e AA_DB_PASSWORD=secret \
  -e AA_DB_NAME=artist_alley \
  -e AA_SCRAMBLE_KEY=$(openssl rand -hex 32) \
  -e AA_MASTER_KEY=$(openssl rand -base64 32) \
  -v artist-alley-storage:/var/lib/aa-storage \
  ghcr.io/artist-alley-org/artist-alley:latest
```

> `AA_MASTER_KEY` is **required** — without it the container exits at
> startup and Docker restart policies will crash-loop it. Generate the
> key once, store it somewhere durable, and reuse it on every start:
> it encrypts secrets at rest, and losing it means losing access to
> everything it protects.

Then open <http://localhost:8080>.

The repo-root [`docker-compose.yml`](../../docker-compose.yml) is the
reference Compose stack with Postgres (pgvector image) bundled.

### First login

For a local evaluation, add `-e AA_BOOTSTRAP_DEFAULT_ADMIN=1` to the
`docker run` line: first boot then creates a default admin account
(`admin` / `ArtistAlleyMogul`). This is a dev convenience — don't use
it for a real deployment.

In the production shape (without that flag), first boot creates an
`admin` account with a random password and prints it to the container
log:

```bash
docker logs artist-alley 2>&1 | grep -A3 "FIRST-BOOT"
```

It also tries to write the credential to `bootstrap-admin.txt` under
`AA_BOOTSTRAP_ADMIN_PATH` (default `/var/lib/artist-alley`), but in the
published image the app runs as a non-root user that cannot create that
directory, so the write fails and the log is where to find the
password. Sign in and change it.

### Tag fan-out

Release tags have no leading `v`: the `v0.11.0` release is pulled as
`:0.11.0`, and its line as `:0.11` or `:0`.

| Tag                  | Updated when                                 |
|----------------------|----------------------------------------------|
| `:X.Y.Z`             | exact version (immutable, recommended for prod) |
| `:X.Y`, `:X`         | latest patch on that line                    |
| `:latest`            | most recent stable release                   |
| `:edge`              | newest build from `dev` (continuous; not stable) |
| `:edge-<short-sha>`  | a specific eligible `dev` build (immutable, pinnable) |

`:edge-<short-sha>` identifies a specific eligible `dev` build.
Docs-only pushes are skipped, because they would produce a
byte-identical image; other non-doc changes (including CI-only
commits) may still produce an edge image. So the tip of `dev` is often
a commit with no image of its own. Pin a sha you can see under
[Packages](https://github.com/Artist-Alley-Org/artist-alley/pkgs/container/artist-alley)
rather than whatever `git rev-parse dev` returns. `:edge` always
points at the newest build.

---

## Verifying signatures (Docker)

Replace `X.Y.Z` with a release version, without the `v` (for example
`0.11.0`):

```bash
cosign verify ghcr.io/artist-alley-org/artist-alley:X.Y.Z \
  --certificate-identity-regexp "https://github.com/Artist-Alley-Org/artist-alley/.*" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com"
```

Every image ships with an SBOM and provenance attestation, both
attached to the image manifest by `docker/build-push-action`.

---

## Building from source

```bash
git clone https://github.com/Artist-Alley-Org/artist-alley
cd artist-alley
docker build -t artist-alley:local .
```

This is the same multi-stage build the published images use: it builds
the SvelteKit frontend with npm, embeds it in the Go binary, and
packages the binary with the tools it calls at runtime (ffmpeg,
ImageMagick, Ghostscript, Chromium and others). Only Docker is needed
on your machine. Run the result exactly like the published image, with
`artist-alley:local` as the image name.

There is no supported build of a standalone native binary yet: the
binary depends on those runtime tools, which only the image provides.

---

## Configuration reference

Everything is environment-variable driven. The full list lives in
[`aa.env.example`](config/aa.env.example).

| Variable                       | Default              | Notes |
|--------------------------------|----------------------|-------|
| `AA_HTTP_ADDR`                 | `:8080`              | listen address |
| `AA_DB_HOST`                   | `postgres`           | Postgres host |
| `AA_DB_PORT`                   | `5432`               |       |
| `AA_DB_NAME`                   | `artist_alley`       |       |
| `AA_DB_USER`                   | `artist_alley`       |       |
| `AA_DB_PASSWORD`               | (required)           |       |
| `AA_DB_SSLMODE`                | `disable`            | `require` / `verify-full` for prod |
| `AA_DB_MAX_CONNS`              | `20`                 | pgx pool max connections |
| `AA_DB_MIN_CONNS`              | `2`                  | pgx pool min connections |
| `AA_DB_CONN_MAX_LIFETIME`      | `1h`                 | pgx pool connection lifetime |
| `AA_STORAGE_BACKEND`           | `fs`                 | or `s3` |
| `AA_STORAGE_FS_ROOT`           | `/var/lib/artist-alley/storage` | `fs` backend only; the Docker image sets it to `/var/lib/aa-storage` |
| `AA_STORAGE_S3_BUCKET`         |                      | `s3` backend |
| `AA_STORAGE_S3_REGION`         |                      |       |
| `AA_STORAGE_S3_ENDPOINT`       |                      | MinIO / R2 / B2 |
| `AA_STORAGE_S3_ACCESS_KEY`     |                      |       |
| `AA_STORAGE_S3_SECRET_KEY`     |                      |       |
| `AA_STORAGE_S3_USE_PATH_STYLE` | `true`               | path-style addressing (MinIO needs `true`; set `false` for AWS virtual-hosted style) |
| `AA_SCRAMBLE_KEY`              | (required)           | password-hashing pepper, `openssl rand -hex 32`; keep it stable |
| `AA_MASTER_KEY`                | (required)           | at-rest encryption master key — base64-encoded 32 bytes, `openssl rand -base64 32` |
| `AA_BOOTSTRAP_DEFAULT_ADMIN`   | `0`                  | `1` = dev-only default admin (`admin` / `ArtistAlleyMogul`) on first boot |
| `AA_BOOTSTRAP_ADMIN_PATH`      | `/var/lib/artist-alley` | where first boot tries to write the generated admin credential; the app user must be able to create and write that directory (in the published image the default is not writable, so use the log) |
| `AA_EMAIL_MODE`                | `smtp`               | `smtp` / `capture` (record in-memory, never deliver) / `disabled` |
| `AA_LICENSE_PATH`              | `/etc/artist-alley/license.lic` | license file (optional); no commercial or paid licenses are offered yet, and without one the built-in community defaults apply |
| `AA_ORG_KEY_PATH`              | `/etc/artist-alley/org.key` | organization key file (optional) |
| `AA_LOG_LEVEL`                 | `info`               | `debug` / `info` / `warn` / `error` |
| `AA_LOG_FORMAT`                | `json`               | or `text` |
