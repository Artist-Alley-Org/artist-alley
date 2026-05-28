# Installing artist-alley

Self-hosted art review and archival, distributed as:

| Channel       | Best for                            | Pull / install |
|---------------|-------------------------------------|----------------|
| Docker image  | Most users — fastest path           | `docker pull ghcr.io/mscrnt/artist-alley:latest` |
| Docker Hub    | Same image, alternate registry      | `docker pull mscrnt/artist-alley:latest` |
| `.deb`        | Debian / Ubuntu (22.04, 24.04, ...) | `sudo apt install ./artist-alley_*.deb` |
| `.rpm`        | Fedora / RHEL / openSUSE            | `sudo dnf install ./artist-alley_*.rpm` |
| Static binary | Other Linux + macOS + Windows       | tarball / zip on the Releases page |
| Homebrew      | macOS dev installs                  | `brew install mscrnt/tap/artist-alley` |
| Source        | Building from scratch               | clone repo, see [BUILDING.md](../BUILDING.md) |

All artefacts come from the same commit on every release tag. Docker
images are signed via Sigstore (`cosign verify`). Binaries and packages
ship with a `SHA256SUMS` file.

---

## What you need before installing

- **PostgreSQL 14+** (the database — bring your own; artist-alley
  creates its own schema on first run via embedded migrations)
- A directory for **storage** (binary blobs + generated thumbnails)
  or an S3-compatible bucket
- A **session signing key** (32 hex bytes — `openssl rand -hex 32`)

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
  -v artist-alley-storage:/var/lib/aa-storage \
  ghcr.io/mscrnt/artist-alley:latest
```

Then open <http://localhost:8080>.

A Compose example with Postgres bundled lives under
[`docs/install/compose/`](compose/).

### Tag fan-out

| Tag                  | Updated when                                 |
|----------------------|----------------------------------------------|
| `:vX.Y.Z`            | exact version (immutable, recommended for prod) |
| `:vX.Y`, `:vX`       | latest patch on that line                    |
| `:latest`            | most recent stable release                   |
| `:edge`              | latest commit on `dev` (continuous; not stable) |
| `:edge-{sha}`        | exact dev commit (immutable, pinnable)       |

---

## Debian / Ubuntu

```bash
# Replace X.Y.Z with the version from the Releases page.
curl -LO https://github.com/mscrnt/artist-alley/releases/download/vX.Y.Z/artist-alley_X.Y.Z_linux_amd64.deb
sudo apt install ./artist-alley_X.Y.Z_linux_amd64.deb

# Edit settings:
sudo $EDITOR /etc/artist-alley/aa.env

# Start it:
sudo systemctl enable --now artist-alley
sudo systemctl status artist-alley
```

The package ships with a hardened systemd unit
(`/lib/systemd/system/artist-alley.service`) and a config template at
`/etc/artist-alley/aa.env`.

## Fedora / RHEL / openSUSE

```bash
curl -LO https://github.com/mscrnt/artist-alley/releases/download/vX.Y.Z/artist-alley_X.Y.Z_linux_amd64.rpm
sudo dnf install ./artist-alley_X.Y.Z_linux_amd64.rpm
sudo $EDITOR /etc/artist-alley/aa.env
sudo systemctl enable --now artist-alley
```

---

## Static binary (any Linux/macOS/Windows)

```bash
curl -LO https://github.com/mscrnt/artist-alley/releases/download/vX.Y.Z/artist-alley_vX.Y.Z_linux_amd64.tar.gz
tar xzf artist-alley_vX.Y.Z_linux_amd64.tar.gz
sudo install -m 0755 artist-alley_vX.Y.Z_linux_amd64/aa /usr/local/bin/aa

# Copy the bundled systemd unit + config template:
sudo install -m 0644 artist-alley_vX.Y.Z_linux_amd64/contrib/systemd/artist-alley.service /etc/systemd/system/
sudo install -m 0640 -D artist-alley_vX.Y.Z_linux_amd64/contrib/aa.env.example /etc/artist-alley/aa.env

# Create the system user + storage dir:
sudo useradd --system --no-create-home --shell /usr/sbin/nologin artist-alley
sudo install -d -o artist-alley -g artist-alley -m 0750 /var/lib/artist-alley

sudo $EDITOR /etc/artist-alley/aa.env
sudo systemctl daemon-reload
sudo systemctl enable --now artist-alley
```

## Homebrew (macOS)

```bash
brew install mscrnt/tap/artist-alley
aa --version
```

---

## Verifying signatures (Docker)

```bash
cosign verify ghcr.io/mscrnt/artist-alley:vX.Y.Z \
  --certificate-identity-regexp "https://github.com/mscrnt/artist-alley/.*" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com"
```

Binary SHA256 checksums live alongside each release as
`artist-alley_X.Y.Z_SHA256SUMS`.

---

## Configuration reference

Everything is environment-variable driven. The full list lives in
[`aa.env.example`](config/aa.env.example).

| Variable                       | Default              | Notes |
|--------------------------------|----------------------|-------|
| `AA_HTTP_ADDR`                 | `:8080`              | listen address |
| `AA_DB_HOST`                   | (required)           | Postgres host |
| `AA_DB_PORT`                   | `5432`               |       |
| `AA_DB_NAME`                   | `artist_alley`       |       |
| `AA_DB_USER`                   | `artist_alley`       |       |
| `AA_DB_PASSWORD`               | (required)           |       |
| `AA_DB_SSLMODE`                | `disable`            | `require` / `verify-full` for prod |
| `AA_STORAGE_BACKEND`           | `fs`                 | or `s3` |
| `AA_STORAGE_FS_ROOT`           | `/var/lib/artist-alley` | `fs` backend only |
| `AA_STORAGE_S3_BUCKET`         |                      | `s3` backend |
| `AA_STORAGE_S3_REGION`         |                      |       |
| `AA_STORAGE_S3_ENDPOINT`       |                      | MinIO / R2 / B2 |
| `AA_STORAGE_S3_ACCESS_KEY`     |                      |       |
| `AA_STORAGE_S3_SECRET_KEY`     |                      |       |
| `AA_STORAGE_S3_FORCE_PATH_STYLE` | `false`            | MinIO needs `true` |
| `AA_SCRAMBLE_KEY`              | (required)           | session signing — `openssl rand -hex 32` |
| `AA_LOG_LEVEL`                 | `info`               | `debug` / `info` / `warn` / `error` |
| `AA_LOG_FORMAT`                | `json`               | or `text` |
