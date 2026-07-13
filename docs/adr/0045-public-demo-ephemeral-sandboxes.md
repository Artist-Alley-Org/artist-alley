---
id: "0045"
title: Public demo — ephemeral per-visitor sandboxes
status: accepted
date: 2026-06-05
area: ops
phases:
  - "1.48"
supersedes: []
related:
  - "0006"
  - "0008"
  - "0017"
  - "0024"
  - "0038"
  - "0043"
tags:
  - ops
  - demo
  - go-to-market
  - infrastructure
excerpt: >-
  artist-alley.org runs a public demo at demo.artist-alley.org that
  spins up a personal, fully-isolated sandbox instance per visitor.
  Each sandbox is pre-seeded with CC0 sample assets, auto-destroys
  after a TTL, and federates with no other instance. Visitors get the
  real product experience without any cross-visitor moderation surface.
---

> **Status note (2026-07-13):** the canonical GitHub org is now
> **Artist-Alley-Org** (v0.1.0 org move, 2026-07-11), and
> artist-alley.org moved to the private `artist-alley-site` repo at
> the 1.55.Z site split. The `mscrnt/aa-demo-provisioner` repo
> referenced below has not moved — satellite-repo homes are still
> undecided.

## Context

A self-hosted product needs a way for prospective operators to try
it before they commit to standing one up. Three approaches were
considered:

1. **Read-only marketing site** — screenshots + a Loom walkthrough.
   Cheapest, lowest fidelity. Skipped because it doesn't let
   visitors actually touch the upload pipeline, the review canvas,
   the universal viewer, or the admin shell.
2. **Single shared multi-tenant demo instance** — visitors sign up,
   get an account on one big public instance, upload art, comment,
   etc. Highest fidelity. Rejected because the moderation surface
   is permanent and expensive: any public-write surface means spam,
   harassment, NSFW, illegal content, takedown requests, and a
   permanent operations burden we are not prepared to take on for a
   pre-MVP demo.
3. **Ephemeral per-visitor sandboxes** — each visitor gets a
   personal isolated Artist Alley instance, pre-seeded with sample
   art, that auto-destroys after a TTL. This ADR adopts this option.

The walled-garden federation design from
[ADR 0043](/adr/0043-federation-walled-garden-protocol/) makes
option 3 cleanly possible: sandboxes can be configured to federate
with no peers at all, so a runaway visitor cannot accidentally (or
deliberately) push content into any other Artist Alley instance.

## Decision

Run a public demo at **demo.artist-alley.org** that **provisions a
personal, isolated Artist Alley instance per visitor on demand,
pre-seeds it with curated CC0-licensed sample art, and auto-destroys
it after a TTL**.

### High-level shape

```
visitor → demo.artist-alley.org
            ↓
       [Cloudflare Turnstile]
            ↓
       [enter email + click "Send me the demo link"]
            ↓
   provisioner sends magic-link email
            ↓
       visitor clicks link in inbox
            ↓
       [email verified, sandbox provisioning]
            ↓
visitor.demo.artist-alley.org/<sandbox-id>
  (60-min wall-clock TTL,
   30-min idle timeout,
   pre-seeded with sample assets)
            ↓
  TTL expires → sandbox destroyed
  (or visitor clicks "extend to 7 days"
   — no second signup needed; we have their email)
```

**Email-signup-first** (not Turnstile-only-and-claim-later). The
magic-link verification step:

- Filters out drive-by spam and hit-and-run abuse — committing a
  working email address up front is real friction for bots.
- Captures the marketing-funnel contact at the moment of
  highest intent, not as an awkward opt-in at the end.
- Lets us rate-limit per email address (cleaner than per-IP, which
  hits NAT'd visitors).
- Removes the awkward "your demo expired, want to extend it?" sales
  prompt because the extension flow becomes "click this same email
  link again."
- Adds ~30s of inbox-bounce friction to the funnel — acceptable for
  the audience (studio operators evaluating a self-hostable tool;
  not impulse browsers).

Magic-link emails use [Resend](https://resend.com/) or a similar
transactional-email service. Volume is bounded by the sandbox
creation cap; cost is rounding error (~$1/mo at launch traffic).

### Sandbox content + permissions

Each sandbox is a full Artist Alley binary running with
`DEMO_MODE=true`, which enables:

- **Pre-seed loader.** On first boot, the binary loads a curated
  CC0 / public-domain seed pack — image, video, 3D, audio, PDF,
  docs, sprite sheets — covering every viewer kind so the visitor
  can exercise the full universal-viewer surface without bringing
  their own assets.
- **Admin user provisioned.** The visitor is logged in as an admin
  user with full capabilities so they can poke the admin shell,
  field editor, capability admin, and theme system.
- **Federation disabled.** The sandbox cannot pair with any other
  instance. `federation_peers` is read-only-empty; the `/admin/
  federation` handshake endpoint returns 503. **This is the
  load-bearing guarantee that demo content never escapes a
  sandbox.**
- **Email disabled.** SMTP send is stubbed; password-reset and
  notification emails write to a local log instead of going out.
- **Storage capped.** Per-sandbox quotas: 1 GB upload total, 100
  MB max single asset, 200 assets total. Hard refusals at the
  handler layer, not just UI warnings.
- **Share links disabled by default.** Share-link creation either
  refused outright or sandbox-scoped (the share URL points at
  `visitor.demo.artist-alley.org` so it dies with the sandbox).
- **Commerce / ads / premium add-ons disabled.** Demo mode never
  validates a `.lic` license; premium-add-on slots return
  "available in self-host."

### Lifecycle

| Event | What happens |
|---|---|
| Visitor clicks Start | Provisioner allocates a sandbox subdomain + Fly.io machine + R2 prefix |
| Sandbox boots | Binary runs migrations, loads seed pack, provisions admin user |
| Visitor uses | Sandbox handles all traffic; activity bumps the idle clock |
| 30 min idle | Machine auto-stops (Fly.io idle policy); state preserved on disk |
| Wall-clock TTL hits 60 min | Provisioner sends termination signal; final state allowed to drain |
| Sandbox destroyed | Fly machine destroyed, Postgres volume destroyed, R2 prefix purged |

**Email-signup-first model** (revised from earlier opt-in-at-end
draft). Every demo session starts with an email + Turnstile +
magic-link verification before the sandbox provisions. The email is
the rate-limiting key (1 active sandbox per email at a time, 3
sandboxes per email per 24h, 100 sandboxes per IP per 24h as a
secondary defence). The visitor opts in to the low-volume marketing
list at signup with a clear, separate checkbox — defaulted ON
because they're already in the funnel, but uncheckable (signup
itself doesn't subscribe; the checkbox does). Privacy posture per
ADR 0024 applies: GDPR-compliant retention, one-click unsubscribe
from every email, DSAR delete-on-request.

The 7-day extension flow is now trivially "click the same magic
link again before the sandbox dies" — no second signup, no separate
claim flow. The link in the original signup email doubles as the
extend-my-sandbox link for its lifetime.

### Provisioner — separate small service

The demo provisioner is its own small Go service, **not** bundled
into the Artist Alley binary. Lives in a separate repo
(`mscrnt/aa-demo-provisioner`, public). Responsibilities:

- Serve the demo landing page (`demo.artist-alley.org`).
- Cloudflare Turnstile verification on the "start" button.
- Allocate a sandbox: pick an unused subdomain, call the Fly.io
  Machines API to create a machine running the Artist Alley
  binary with `DEMO_MODE=true` + the seed pack, point a
  Cloudflare DNS record at it.
- Track sandbox state: subdomain, machine ID, started-at, idle-at,
  claimed-by-email (if any), destroyed-at.
- Enforce per-IP and global rate limits (configurable: default 3
  sandboxes per IP per 24 hours; 500 active sandboxes
  global cap; ~$250/month budget cap with overage refusal).
- Run the destroy cron: any sandbox past TTL is torn down and its
  resources purged.
- Send the claim email if the visitor opts in.

Provisioner is the only thing on `demo.artist-alley.org`; sandboxes
live on `*.demo.artist-alley.org` subdomains.

### Seed pack — CC0 sources only

Avoid hosting third-party art without explicit license. The seed
pack is assembled from public-domain / CC0 sources:

- **[Blender Demo Files](https://www.blender.org/download/demo-files/)** —
  CC0 .blend scenes for the 3D viewer.
- **[Kenney's Asset Packs](https://kenney.nl/assets)** — CC0 sprite
  sheets + game art (target audience overlap).
- **[Open Game Art](https://opengameart.org/)** — CC0-filtered
  selections of game-ready art.
- **[Met Museum Open Access](https://www.metmuseum.org/art/collection)** —
  CC0 high-res images (broader art-house framing).
- **[NASA media library](https://www.nasa.gov/multimedia/)** —
  public-domain video + image content.
- **Sintel / Big Buck Bunny / Tears of Steel** — CC-BY video files
  for the video pipeline showcase (proper attribution kept in the
  seed pack manifest).
- Sample EPUBs from Project Gutenberg (public domain) for the
  doc-viewer showcase.

~50-100 assets total, spanning every viewer-kind, packaged as a
single tarball the sandbox loads on boot.

The seed-pack assembly + per-asset attribution lives in the
provisioner repo at `seed/`. License sources are documented per
asset in `seed/MANIFEST.json`.

### Hosting — Fly.io machines, Cloudflare R2 for the seed pack

- **Compute:** Fly.io Machines with auto-stop-on-idle. Each
  sandbox is a `shared-cpu-1x` with 512 MB RAM + a 1 GB volume.
  Bills per active second; the auto-stop is what keeps the cost
  bounded.
- **Storage:** A single R2 bucket holds the master seed pack.
  Sandboxes pull it on boot. Per-sandbox uploads live in the
  sandbox's local volume (destroyed with the machine).
- **DNS + Turnstile + edge caching:** Cloudflare.
- **Postgres:** runs inside the sandbox machine (single-process
  Postgres on the local volume). Acceptable because each sandbox
  is single-user, single-instance, and ephemeral.
- **No managed Postgres.** Putting each sandbox on its own
  Neon/Supabase project would multiply per-sandbox cost an order
  of magnitude without buying real value.

### Cost projection (per ADR 0043 §"Why a walled garden" the federation cost is zero)

| Traffic | Compute + R2 + Cloudflare | Notes |
|---|---|---|
| 200 sandbox creations/mo | **~$10** | Realistic launch traffic |
| 1,000 sandbox creations/mo | **~$30** | Decent viral lift |
| 10,000 sandbox creations/mo | **~$100-150** | A successful HN front page |

Hard ceiling enforced by the provisioner: at the configured
budget cap (default ~$250/mo, operator-tunable) the "Start demo"
button surfaces a polite "we're at capacity for today" message
and offers the install-locally instructions instead.

### Moderation posture — what we won't do

This ADR's central tradeoff: **the demo provides zero cross-visitor
moderation surface because there is no cross-visitor anything.**
Concretely:

- We do not moderate visitor uploads. They live in a sandbox the
  visitor cannot share to any other visitor or instance.
- We do not store visitor content past the TTL (max 60 min, or 7
  days if explicitly claimed via opt-in email).
- We do not respond to takedown requests for content uploaded to
  a demo sandbox — we destroy the sandbox instead. The destroy
  cron is the takedown mechanism.
- We do not scan visitor uploads for NSFW / CSAM / IP-violation.
  Sandboxes are isolated; nothing they hold is ever served to a
  third party. The legal posture is: "we host nothing
  user-generated past 60 minutes, and never serve it to anyone
  but the original visitor."
- Standard abuse mitigations (rate limits, Turnstile, IP
  blacklist) live in the provisioner — these prevent resource
  abuse, not content abuse.

A future "public-fediverse-enabled demo" would have a real
moderation surface and is explicitly out of scope here. See ADR
0043 §"Why we are not joining the public fediverse at v1".

## Consequences

### Positive

- **Real product experience.** Visitors upload, review, annotate,
  exercise the universal viewer, poke the admin shell — not a
  video, not a tour. Genuine first-contact UX.
- **Zero moderation surface.** Sandbox isolation means we host no
  shared user-generated content. The legal + operational story is
  honest and simple.
- **Bounded cost.** Per-sandbox cost is ~$0.05; global cost is
  rate-limited via the provisioner's budget cap. Realistic launch
  spend is under $20/month.
- **Federation isolation as a structural property.** Sandboxes run
  with federation disabled by configuration. Even a malicious
  visitor cannot push content out of the demo.
- **Reuses the binary.** Demo mode is a single flag on the
  existing Artist Alley binary. No second product to maintain.
- **Marketing-funnel hook is opt-in, not coercive.** The 7-day
  extension is a real value exchange (extended sandbox in
  exchange for an email), and it works without coercive
  dark-patterns.
- **Composes with the install funnel.** Visitor finishes the
  demo, sees "this is your data" prompt, gets a "ready to install
  for real?" CTA pointing at `/guides/install/`.

### Negative

- **Operational dependency on Fly.io.** The provisioner is
  Fly-specific. Migrating to another machine API (Hetzner Cloud,
  DigitalOcean Apps, AWS Fargate) is a real engineering effort.
  Mitigation: keep the provisioner's Fly integration in a single
  package; abstract the "spin / stop / destroy" interface.
- **Provisioner is a separate service to maintain.** It's small
  (~500-1000 LOC of Go) but it's still a moving part with
  uptime + auth + budget-tracking concerns.
- **Pre-seed pack drifts from the live product.** Demo mode loads
  a fixed pack; new viewer features may surface "well, the seed
  pack doesn't have one of those yet." Mitigation: a periodic
  refresh of the seed pack as new viewer-kinds ship.
- **Email signup is a new data-protection responsibility.** Even
  a low-volume marketing list is personal data per GDPR; we need
  a privacy policy (link to ADR 0024) and an unsubscribe
  mechanism.
- **Federated demos are off the table.** A visitor cannot
  experience the federation handshake or the `aa:Share` flow in
  the demo, because federation is disabled. The "try a federated
  Artist Alley" story remains "install two instances yourself."
- **Sandbox auth is a permissive surface by design.** The
  provisioner gives every visitor a logged-in admin user with
  full capabilities. If the binary has a remote-code-execution
  bug, demo sandboxes are the path. Mitigation: the same security
  rigour applies as production; sandboxes don't open new attack
  surface, they just amplify whatever's already exploitable. The
  isolation (per-sandbox VM, no federation) caps the blast
  radius.

## Alternatives considered

- **Read-only walkthrough video.** Lowest fidelity, zero
  interactivity, doesn't communicate the upload + review loop.
  Rejected as the *primary* demo; useful as a 60-second teaser on
  the landing page above the "Start a sandbox" button.
- **Single shared multi-tenant public instance.** Permanent
  moderation operational burden. Rejected per the no-moderation
  constraint.
- **Self-hosted demo image** (downloadable docker-compose + sample
  data). Useful as an alternative for visitors who'd rather try
  locally than start a sandbox; documented at `/guides/install/`
  separately. Not a substitute for click-to-demo.
- **One Always-on shared "browse-only" instance.** A real
  always-on Artist Alley instance with curated content visitors
  can browse but not modify. Considered. Rejected because (a) it
  doesn't exercise the upload + review loop; (b) "browse-only"
  still requires deciding who can see what restricted content;
  (c) the operational story (we host real bytes long-term) is
  harder than ephemeral sandboxes despite looking simpler.
- **Cloudflare Workers + R2 + Hyperdrive instead of Fly.io.**
  Considered. Rejected because Cloudflare Containers (the only
  Workers-adjacent compute that runs a Go binary) is materially
  more expensive than Fly's per-second-when-active pricing for
  this workload, and Workers Free / Paid Plans don't host long-
  running connections well. Cloudflare stays in the picture for
  DNS / Turnstile / R2 (seed pack).

## Implementation

Phase 1.48 ships in sub-phases. The sandbox-side work touches the
main Artist Alley binary; the provisioner-side work lives in a
separate repository.

- **1.48.A — Demo-mode binary flag.** `DEMO_MODE=true` env flag
  in `app/internal/config/`. Disables federation handshake,
  outbound SMTP, license validation, and commerce / ads / premium
  add-ons. Adds a "demo mode banner" to the SPA shell so visitors
  know they're in a sandbox. Tested via integration test that
  every disabled subsystem refuses gracefully.

- **1.48.B — Seed-pack loader.** On boot under `DEMO_MODE`, the
  binary fetches a `SEED_PACK_URL` tarball from R2, extracts
  assets into the local filestore, runs the existing upload
  pipeline against them (so previews + thumbs generate), and
  creates a curated set of posts / collections / brand workspace
  showcasing the universal viewer.

- **1.48.C — Sandbox quotas.** Hard caps at the handler layer:
  total bytes uploaded, per-asset size, asset count. Refused
  uploads return a clear "demo quota reached" error.

- **1.48.D — Provisioner (separate repo).** `mscrnt/aa-demo-provisioner`.
  Landing page, Turnstile-gated "Start demo" button, Fly.io
  Machines integration, sandbox state tracking, rate limits +
  budget cap enforcement, destroy cron, claim-via-email flow.
  ~500-1000 LOC of Go.

- **1.48.E — Seed pack curation + assembly.** Assemble the actual
  CC0 + public-domain seed pack: 50-100 assets across image /
  video / 3D / audio / PDF / docs / sprite. License attribution
  manifest. Build + upload to R2.

- **1.48.F — Landing page copy + funnel.** `demo.artist-alley.org`
  copy: hero ("Try Artist Alley in your browser"), the Start
  Sandbox button, a "what is this?" expandable section, a
  60-second video teaser, the install-locally fallback CTA.
  Cloudflare Turnstile widget. Privacy policy link (per ADR 0024).

- **1.48.G — Operational runbook.** Document the destroy cron
  failure modes, how to manually destroy a runaway sandbox, how
  to lift the budget cap if needed, how to investigate a sandbox
  that won't tear down. Lives in the provisioner repo.

The seed-pack assembly (1.48.E) is the longest-pole item content-
wise; everything else is small Go code.

## References

- ADR 0006 — Go as target backend. Demo-mode is a flag on the
  same binary, not a fork.
- ADR 0008 — Storage architecture. Sandbox storage uses the
  filesystem backend; seed pack downloads from R2.
- ADR 0017 — Monetization & licensing. Demo mode never validates
  a `.lic`; premium-add-on surfaces fall back to "available in
  self-host."
- ADR 0024 — Privacy & consent. Email-signup claim flow is the
  only PII the demo touches; standard DSAR + retention policy
  applies to that list.
- ADR 0038 — Premium add-on layer. Demo mode never enables
  premium add-ons; the admin surfaces show "available in
  self-host" empty states.
- ADR 0043 — Federation walled-garden. The federation-disabled
  posture of sandboxes is the load-bearing isolation guarantee.
- [Fly.io Machines API](https://fly.io/docs/machines/) — the
  provisioning backend.
- [Cloudflare Turnstile](https://developers.cloudflare.com/turnstile/) —
  bot mitigation on the Start button.
