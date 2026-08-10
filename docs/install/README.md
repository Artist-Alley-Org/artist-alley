# Installing artist-alley

Self-hosted art review and archival, distributed as:

| Channel       | Best for                            | Pull / install |
|---------------|-------------------------------------|----------------|
| Docker image  | Most users — fastest path           | `docker pull ghcr.io/artist-alley-org/artist-alley:latest` |
| Docker Hub    | Same image, alternate registry      | `docker pull mscrnt/artist-alley:latest` |
| Source        | Building from scratch               | clone repo, see [CONTRIBUTING.md](../../CONTRIBUTING.md) and [RELEASING.md](../../RELEASING.md) at the repo root |

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

- **PostgreSQL 14+ with the `pgvector` extension available** (the
  database — bring your own; artist-alley creates its own schema on
  first run via embedded migrations). The migrations also enable
  `pg_trgm`, `pgcrypto`, and `uuid-ossp`, which ship with a standard
  Postgres install, but `pgvector` must be installed separately — the
  easiest path is running one of the
  [`pgvector/pgvector`](https://hub.docker.com/r/pgvector/pgvector)
  images instead of vanilla `postgres`.
- A directory for **storage** (binary blobs + generated thumbnails)
  or an S3-compatible bucket
- A **session signing key** (32 hex bytes — `openssl rand -hex 32`)
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

In the production shape (without that flag), first boot generates a
random admin credential instead, prints it to the container log, and
persists it under `AA_BOOTSTRAP_ADMIN_PATH` (default
`/var/lib/artist-alley`) so you can retrieve it after the fact.

### Tag fan-out

| Tag                  | Updated when                                 |
|----------------------|----------------------------------------------|
| `:vX.Y.Z`            | exact version (immutable, recommended for prod) |
| `:vX.Y`, `:vX`       | latest patch on that line                    |
| `:latest`            | most recent stable release                   |
| `:edge`              | latest commit on `dev` (continuous; not stable) |
| `:edge-{sha}`        | exact dev commit that changed the application (immutable, pinnable) |

`:edge-{sha}` is published only for `dev` commits that touch the
application. Docs-only commits are skipped — they would produce a
byte-identical image — so the tip of `dev` is often a commit with no
image of its own. Pin a sha you can see under
[Packages](https://github.com/Artist-Alley-Org/artist-alley/pkgs/container/artist-alley)
rather than whatever `git rev-parse dev` returns. `:edge` always
points at the newest built application state.

---

## Verifying signatures (Docker)

```bash
cosign verify ghcr.io/artist-alley-org/artist-alley:vX.Y.Z \
  --certificate-identity-regexp "https://github.com/Artist-Alley-Org/artist-alley/.*" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com"
```

Every image ships with an SBOM and provenance attestation, both
attached to the image manifest by `docker/build-push-action`.

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
| `AA_SCRAMBLE_KEY`              | (required)           | session signing — `openssl rand -hex 32` |
| `AA_MASTER_KEY`                | (required)           | at-rest encryption master key — base64-encoded 32 bytes, `openssl rand -base64 32` |
| `AA_BOOTSTRAP_DEFAULT_ADMIN`   | `0`                  | `1` = dev-only default admin (`admin` / `ArtistAlleyMogul`) on first boot |
| `AA_BOOTSTRAP_ADMIN_PATH`      | `/var/lib/artist-alley` | where the generated first-boot admin credential is persisted |
| `AA_EMAIL_MODE`                | `smtp`               | `smtp` / `capture` (record in-memory, never deliver) / `disabled` |
| `AA_LICENSE_PATH`              | `/etc/artist-alley/license.lic` | commercial license file (optional) |
| `AA_ORG_KEY_PATH`              | `/etc/artist-alley/org.key` | organization key file (optional) |
| `AA_LOG_LEVEL`                 | `info`               | `debug` / `info` / `warn` / `error` |
| `AA_LOG_FORMAT`                | `json`               | or `text` |
