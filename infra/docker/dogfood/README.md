# Artist Alley — Dogfood Stack

A second Artist Alley instance running alongside the dev stack so
federation scenarios can run end-to-end against two real peers.
Phase 1.22.I-a per ADR 0049 Track A. Issue #98.

The dev stack at the repo root plays **studio-a**; the
`dogfood` profile in the root `docker-compose.yml` spawns
**studio-b** on a separate Postgres / app / nginx with mkcert
TLS at `https://studio-b.local:9443`.

This is a **permanent dev surface**, not throwaway scaffolding.
As new federation phases ship, their integration scenarios run
against this stack and the canonical-scenarios catalogue grows.

## Layout

```
infra/docker/dogfood/
├── nginx/
│   └── studio-b.conf       TLS termination + reverse proxy to app-b
├── certs/                  mkcert-issued certs (gitignored)
└── README.md               this file

scripts/dogfood/
├── up.sh                   first-run mkcert + /etc/hosts + compose up
├── down.sh                 stop studio-b, keep volumes
├── reset.sh                destructive — nuke studio-b's volumes
├── seed.sh                 wrap apply.py against studio-b (--site required)
├── pair.sh                 auto-pair studio-a ↔ studio-b
├── tail.sh                 combined log tail, color-coded per instance
└── scenarios/              canonical regression catalogue
    ├── lib.sh              shared helpers (login, api wrappers, assertions)
    ├── 01-like-cross-instance.sh        (full)
    ├── 02-share-collection-comment.sh    (outline)
    ├── 03-defederation-cascade.sh        (outline)
    ├── 04-restricted-pre-Ih.sh           (outline)
    └── 05-restricted-encrypted.sh        (stub — gated on 1.22.I-i)
```

## Why a profile, not a separate compose

Earlier iterations of this work spun up TWO full studios
(studio-a + studio-b) in a standalone `infra/docker/dogfood/
docker-compose.yml`. That works but wastes resources — three
Postgres instances + three apps if you also keep the dev stack
up. The profile model reuses dev as studio-a and adds only the
studio-b side, gated behind the `dogfood` compose profile so
default `docker compose up -d` is unchanged.

Tradeoff: dev's nginx doesn't terminate TLS, so studio-b →
studio-a runs over plain HTTP on the internal docker bridge.
studio-a → studio-b is HTTPS via mkcert. Both directions sign
HTTP-Sig the same way; the federation protocol doesn't care
about transport TLS. For prod-shape encrypted-federation testing
(Phase 1.22.I-b+), we may add a TLS-terminating reverse proxy
in front of dev too. Skipping for now.

## Datasets

**studio-a** (= dev) keeps whatever data you've loaded into the
dev DB. **studio-b ships empty** and the operator seeds it via
`./scripts/dogfood/seed.sh --site <path>`. There's no embedded
demo dataset for studio-b — you supply your own (typically a
different `site_*` directory than what's loaded in dev so
federation scenarios produce visible cross-stack flow).

## Hostnames + ports

| Studio | Browser URL                       | Container DNS         |
|--------|-----------------------------------|-----------------------|
| A      | http://localhost:5173 (Vite)      | studio-a.local        |
|        | http://localhost:8080 (nginx)     | (via network alias)   |
| B      | https://studio-b.local:9443       | studio-b.local        |

`studio-a.local` resolves to dev's nginx **inside the docker
bridge network only**. `studio-b.local` resolves to nginx-b
both inside the bridge (via alias) AND on the host browser
(via `/etc/hosts` managed by `up.sh`).

Each nginx binds to the same port internally as externally
(studio-b: container :9443 → host :9443) so the federation URL
stored in DB works identically from inside containers and from
the host browser.

## TLS

mkcert is the canonical local CA. `up.sh` installs the root CA
once into the host trust store and issues `studio-b.local`
into `certs/`. Both browsers and Go's HTTP client trust without
test-mode workarounds — production parity for HTTP-Sig signing
and envelope verification.

## Bootstrap admin

Both stacks set `AA_BOOTSTRAP_DEFAULT_ADMIN=1` so the helper
scripts + scenarios can authenticate as `admin / ArtistAlleyMogul`
without fishing a random password out of the boot log.

Dev-only password — fine for dogfood, never for production.

## Common ops

```bash
# Cold start (first run, or after reset)
./scripts/dogfood/up.sh

# Seed studio-b. Pick a site_* directory the operator owns;
# DIFFERENT from what's in dev so federation produces visible
# cross-stack flow.
./scripts/dogfood/seed.sh --site /path/to/your/site_dir

# Auto-pair the two instances
./scripts/dogfood/pair.sh

# Run the canonical scenarios
./scripts/dogfood/scenarios/01-like-cross-instance.sh

# Watch federation traffic with per-instance color
./scripts/dogfood/tail.sh

# Stop studio-b, keep volumes
./scripts/dogfood/down.sh

# Nuke studio-b's volumes + start fresh
./scripts/dogfood/reset.sh
```

`docker compose --profile dogfood up -d` also works for
plain compose, but skips the mkcert + /etc/hosts setup `up.sh`
handles.

## Status of the canonical scenarios

| # | Name | Status |
|---|---|---|
| 01 | Basic pairing + Like              | **full** — runs end-to-end |
| 02 | Share collection + cross-instance comment | outline — plan in comments |
| 03 | Defederation cascade              | outline — plan in comments |
| 04 | Restricted content (pre-I-h)      | outline — plan in comments |
| 05 | Restricted content end-to-end     | stub — gated on 1.22.I-i |

Outlines exist so the scaffolding is in place when the operator
sits down to the dogfood week. Filling 02–04 to full assertions
is part of running the week — the act of writing the assertions
IS the test for whether the wire actually works end-to-end.
