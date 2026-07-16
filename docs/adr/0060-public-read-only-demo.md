---
id: "0060"
title: Public read-only demo instance
status: accepted
date: 2026-07-16
area: ops
phases: []
supersedes: []
related:
  - "0058"
tags:
  - demo
  - ops
  - deployment
  - security
excerpt: >-
  A public read-only demo (demo.artist-alley.org) that runs the release
  image with writes blocked at the edge, no guessable admin, and a host-
  side auto-update — so the full feature surface is browsable without
  exposing a mutable or privileged instance.
---

## Context

We want a public demo so prospective operators can browse the whole
admin + review surface without installing anything. The hard constraints:
it must be **read-only** (no visitor can mutate shared state), expose **no
sensitive config or guessable admin**, and **track releases** with near-zero
manual upkeep. It runs on the existing self-hosted Unraid box alongside CI
and the user's other services, so it must also be cheap and self-healing.

The demo lives in a **separate private repo** (`artist-alley-demo`) as a
deploy bundle; this ADR records the architecture, not the app code.

## Decision

1. **Read-only at the edge, not the app.** An nginx front door allows only
   `GET/HEAD/OPTIONS` on `/api/v1/*` (via `limit_except`, the safe idiom),
   allow-lists `/auth/login` + `/auth/logout`, and returns a friendly JSON
   403 on any write. The app is never published to the host — only nginx is
   reachable — so nothing bypasses the edge.

2. **Defense in depth via the role, not a superuser.** The public account
   gets a `demo-viewer` role with the `.read` caps + coarse `.admin` caps
   needed to *render* every admin surface, but deliberately **not**
   `system.admin`. So even if a write slipped past nginx, the app's own
   per-capability guards still refuse it — two layers, not one.

3. **Secure bootstrap — no default admin.** `AA_BOOTSTRAP_DEFAULT_ADMIN` is
   **off**, so the app creates the first admin with a random password (never
   the documented `ArtistAlleyMogul`). The deploy script recovers that
   password from the boot log internally to seed + provision; it is never
   exposed. Only `demo`/`demo` is handed out.

4. **No real secrets.** Blank SMTP / AI / SSO / federation; throwaway
   `AA_MASTER_KEY` / `AA_SCRAMBLE_KEY` generated on the box. The admin config
   screens render empty forms — the feature story with zero exposure.

5. **Env-gated demo affordances.** `AA_DEMO_MODE` surfaces a `demo`/`demo`
   sign-in hint + a read-only banner; off by default, zero footprint on real
   installs.

6. **Host-side auto-update, not CI.** GitHub runners are containers without
   host access to the Compose-Manager stack, so the update trigger lives on
   the **host**: an Unraid User Script polls the registry and, when the
   `:latest` digest advances (a tested, tagged release), runs a reset that
   rebuilds + reseeds. State is nuked freely — it's read-only, nothing to lose.

7. **Exposure via the existing Cloudflare Tunnel.** The bridge-mode tunnel
   container can't reach `127.0.0.1`, so nginx binds the host LAN IP; a
   cache-bypass rule keeps the dynamic app from being edge-cached.

## Consequences

- The demo showcases the full surface with no mutable or privileged access,
  and no bespoke "demo build" — it runs the exact release image.
- Pre-1.0 schema churn is handled by nuke-and-reseed on each release, so a
  breaking migration can't leave the demo wedged.
- Cost: the demo depends on host-side plumbing (User Script cron + tunnel)
  that isn't captured in a GitHub workflow — intentional, since runners can't
  reach the host. Documented in the `artist-alley-demo` README.
- A per-IP rate limit was deliberately skipped: a read-only, media-heavy demo
  would false-positive on normal browsing; Cloudflare's baseline covers abuse.

## References

- ADR 0058 — two-tier demo seed dataset (Layer A public content the demo loads).
- `artist-alley-demo` repo — the deploy bundle (compose, nginx edge, reset +
  provision scripts, host auto-update).
