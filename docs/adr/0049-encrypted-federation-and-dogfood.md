---
id: "0049"
title: Encrypted federation + two-server local dogfood infrastructure
status: accepted
date: 2026-06-07
area: architecture
phases:
  - "1.22"
supersedes: []
related:
  - "0017"
  - "0020"
  - "0024"
  - "0042"
  - "0043"
  - "0044"
  - "0045"
tags:
  - federation
  - architecture
  - cryptography
  - testing
  - infrastructure
excerpt: >-
  Phase 1.22.I lights up end-to-end encrypted federation (X25519
  keypair-per-user, NaCl-box envelope encryption, peer capability
  negotiation, 7-day key-rotation grace period) — but only after
  a two-server local dogfood setup runs the 1.22.D wire-protocol
  surface against itself. Dogfood infrastructure becomes a
  permanent dev surface in `infra/docker/dogfood/`, not throwaway.
---
## Context

Phase 1.22.A through 1.22.D ship the federation wire-protocol
stack — envelope library, peer registry + handshake + directory,
share access control, inbox + outbox + delivery worker with sub-1s
latency contract, admin observability. Single-instance integration
tests exercise the code paths against fixture peers. They do not
exercise the code paths against **another running Artist Alley
process talking over real TLS to a different Postgres**.

That gap matters. Single-instance testing has known blind spots
that two-instance dogfooding catches:

- **Clock skew.** The ±5min Date check in §10 only fails when two
  separately-running clocks disagree. Single-instance tests share
  one clock; they pass when production would fail.
- **TLS + HTTP-Signature interaction.** Test peers in single-
  instance tests share a Go process; the HTTP-Sig signing +
  verification happens in-memory. Real federation runs through a
  TLS termination point with a different certificate per peer.
- **Defederation cascade ordering.** The cascade-by-peer flow
  emits `aa:Unshare` activities while peer state is being torn
  down. Single-process tests can't reproduce the race where the
  peer's inbox accepts the Unshare while the defederation cascade
  is mid-cleanup.
- **Encrypted-content lifecycle.** Once 1.22.I ships X25519
  keypair-per-user + capability negotiation, the integration
  questions ("what if the recipient hasn't generated their key
  yet?", "what if the recipient peer is on an older build that
  doesn't advertise the capability?") need two separately-deployed
  builds to test.
- **Operational mental model.** An operator running Artist Alley
  thinks in terms of "my instance" and "their instance." A
  development surface that doesn't reflect that boundary leaves
  decisions about UX (peer name display, audit-log surface,
  diagnostic affordances) untested against the real workflow.

Phase 1.22.I introduces encryption, which doubles the
implementation complexity and triples the failure modes that
single-instance testing won't see. **Standing up a two-server
local dogfood environment before 1.22.I starts is the right
sequencing.** It's also a permanent dev surface — every future
federation-touching phase (1.22.J, future cross-instance
presentation rooms per #97, federation-aware moderation work,
etc.) benefits from the same harness.

### Why now and not earlier

1.22.A–D shipped without dogfood infrastructure because the wire-
protocol layer could be exercised through fixture peers without
materially compromising correctness. The agent's end-to-end test
in 1.22.D-b-6 explicitly used production tick intervals and
asserted the sub-1s latency contract — that test proves the wire
layer end-to-end inside one process. Encrypted federation
introduces *inter-process* concerns the single-process test
cannot prove. The cost-benefit shifted at the 1.22.D → 1.22.I
boundary.

## Decision

Phase 1.22.I delivers two coordinated workstreams. The dogfood
infrastructure must land first; the encryption work depends on it
to validate behavior.

### Track A — Two-server local dogfood infrastructure (1.22.I-a)

Two complete Artist Alley stacks running side-by-side on one host,
each with its own Postgres + app + web + filestore + TLS cert.
Bridge-networked so they resolve each other by hostname; bound to
host ports so a browser can reach each. Sub-phases below use the
canonical names `studio-a.local` and `studio-b.local`.

**Hosting layout:**

```
infra/docker/dogfood/
├── docker-compose.yml          two stacks on one bridge network
├── studio-a.env                hostname + port + db config
├── studio-b.env                same
├── certs/                      mkcert-issued certs (gitignored)
└── README.md                   operational doc
```

**Helper scripts (in `scripts/dogfood/`):**

- `up.sh` — provision mkcert on first run, write `/etc/hosts`
  entries (or document them), bring both stacks up.
- `down.sh` — stop both, leave volumes intact.
- `reset.sh` — destructive: nuke volumes; next `up.sh` is fresh.
- `seed.sh` — seed each instance with a starter admin + a small
  set of demo users + a curated CC0 asset set (the seed dataset
  is its own deliverable — see §"Seed dataset" below).
- `pair.sh` — auto-pair the two instances via the
  `/admin/federation/peers/add` handshake flow.
- `tail.sh` — combined log tail (color-coded per instance) for
  observing federation traffic in real time.
- `scenarios/01-like-cross-instance.sh` through `05-restricted-
  encrypted.sh` — pre-canned scripts that drive end-to-end
  scenarios (see §"Scenarios" below).

**TLS:** mkcert is the canonical local CA. Installed once into the
host trust store; issues `studio-a.local` and `studio-b.local`
certificates. Both browsers and Go's HTTP client trust without
test-mode workarounds. Self-signed alternatives and HTTP-only
modes are explicitly rejected — the whole point of dogfooding is
production parity for federation auth.

**Hostnames:** `studio-a.local` and `studio-b.local` resolve to
`127.0.0.1` for the host (browser access on bound ports); docker
bridge DNS resolves them for container-to-container HTTP within
the dogfood network. The setup script writes/updates the
`/etc/hosts` block under a marker comment so re-running is
idempotent.

**Permanence.** `infra/docker/dogfood/` is a tracked first-class
dev surface, not throwaway scaffolding. Future federation-
touching phases run their integration scenarios against it. The
canonical-scenarios catalogue grows over time as new federation
surfaces ship.

### Track B — Encrypted federation (1.22.I-b through 1.22.I-i)

The encryption work lands as a sequence of sub-phases. Design
decisions confirmed before Track B starts (open questions from
the prior reviewer discussion locked in here):

#### Decision 1: X25519 keypair lifecycle — eager generation

Every user gets an X25519 keypair generated at user-create time.
Reasoning:

- Matches the existing HTTP-Sig actor-key pattern (also eager).
- No "federation is stuck waiting for key gen" path; the user
  is federation-ready from creation.
- Pre-MVP DB footprint cost is trivial (~64 bytes per user for
  key material; private key encrypted at rest with the master
  key per ADR 0017).
- Lazy generation adds a synchronization edge — what does the
  federation code do when it needs to send to a recipient whose
  key generation hasn't fired? Refuse? Block? Eager removes
  the question.

#### Decision 2: Public key distribution via actor profile inline

`GET /users/{username}` returns the actor profile with an
embedded `publicKeys` block listing the user's current X25519
public key + version + algorithm. No separate `/federation/users/
{username}/keys` endpoint.

Reasoning:

- Matches the ActivityPub-shaped story (actor objects carry their
  signing keys inline today).
- Cache layer lives next to the existing actor cache; one fewer
  endpoint to plumb.
- Key rotation is signalled through profile-version bumping; peers
  re-fetch when they see a version they don't have cached.
- Multi-key responses for the rotation grace period (current key
  + retained-for-decrypt older keys) fit naturally as a `keys`
  array, not a single field.

#### Decision 3: Peer-handshake capability advertisement

The handshake response includes a `supported_capabilities` JSONB
array. Capability tokens are versioned strings:

```json
[
  "encryption-nacl-box-v1",
  "object-integrity-proofs-v1",
  "batched-inbox-v1"
]
```

Stored on `federation_peers.capabilities`. Sender checks
capabilities before encrypted emission. If the peer doesn't
advertise `encryption-nacl-box-v1`, restricted content can't
federate to them; the existing sender refusal + audit-event +
admin-banner path fires.

This is **required** to make the upgrade window safe — post-1.22.I
instances talking to pre-1.22.I instances during the upgrade
window must fall back gracefully.

#### Decision 4: Key rotation with 7-day grace period

User rotates their X25519 keypair via `/account/security/rotate-
federation-keys`. The flow:

- Generate new keypair; encrypt private key at rest.
- Insert new key with `is_current=true`; flip prior key's
  `is_current=false` and set `retained_until=NOW() + 7 days`.
- Bump the user's actor profile version (peers re-fetch).
- Emit `federation.user.key_rotated` audit event.
- Outbox dispatcher uses the current key for new outbound.
- Inbox handler tries current key first, falls back to retained
  older keys on decrypt failure.
- Periodic sweeper drops retained keys past their `retained_until`.

7 days is the default; admin-tunable per-instance. Lower
acceptable for high-security operators; higher acceptable for
operators with low-volume federation where in-flight messages
take longer to drain.

Without this, key rotation drops messages in flight. The grace
period is non-negotiable.

#### Decision 5: Sender-side refusal flip

1.22.D's emission refusal for restricted content is currently
unconditional ("encrypted federation not yet supported").

1.22.I-h flips it: refuse only if BOTH conditions hold:

- The recipient peer's `capabilities` does NOT include
  `encryption-nacl-box-v1`; OR
- The recipient user (resolved by actor URL) has no current
  X25519 public key fetchable.

Otherwise, the dispatcher encrypts and emits. The refuse-emit +
audit-banner path stays in place for the cases where it still
fires.

### Sub-phase breakdown (1.22.I-a through I-i)

The dogfood track is sub-phase a (Track A); encryption is sub-
phases b through i (Track B).

- **1.22.I-a — Dogfood infrastructure** (Track A). `infra/docker/
  dogfood/` + `scripts/dogfood/` + operational doc + mkcert
  bootstrap + the Scenario 01-04 scripts (Scenario 05 lands with
  I-i below). Runs the existing 1.22.D wire surface against
  itself; reports any surprises from Scenarios 01-04 against the
  current build before encryption work starts. Gates everything
  else in 1.22.I.

- **1.22.I-b — X25519 keypair-per-user.** Eager generation on
  user create; at-rest encryption of the private key with the
  master key per ADR 0017's encrypted credentials pattern; storage
  table `federation_user_keys` carrying `(user_id, version,
  public_key, private_key_encrypted, is_current, created_at,
  retained_until)`; migration + sqlc + tests.

- **1.22.I-c — Public key distribution + actor profile inline.**
  `GET /users/{username}` response gains a `publicKeys` block
  (array of current + retained-for-decrypt keys with version +
  algorithm). Per-actor cache layer; invalidation on rotation
  bumps the profile version. Webfinger continues to carry only
  the actor URL; the keys come from the actor fetch.

- **1.22.I-d — Capability negotiation at peer handshake.**
  Handshake response carries `supported_capabilities` JSONB
  array; stored on `federation_peers.capabilities`; sender-side
  check at outbox dispatch resolution time; audit event named
  with the capability gap on refusal.

- **1.22.I-e — Outbox encryption.** NaCl-box envelope encryption
  per recipient at delivery time. Sender fetches recipient
  pubkey via the I-c cache, encrypts the payload, signs the
  envelope. Per-recipient encryption means one outbox row →
  one recipient → one encryption operation; no shared envelope
  across recipients.

- **1.22.I-f — Inbox decryption.** Light up the
  `TODO(1.22.I)` markers stamped in 1.22.D-a-3. Receiver tries
  current key first; falls back to retained older keys on
  decrypt failure. New §12.1 reject reason `decrypt_failed`
  for envelopes that don't decrypt against any retained key.

- **1.22.I-g — Sender refusal flip.** Replace the unconditional
  "encryption not supported" refusal with the capability-aware
  check from Decision 5. Refuse only when peer capability is
  absent OR recipient key is unfetchable.

- **1.22.I-h — Key rotation flow.** User-facing
  `/account/security/rotate-federation-keys` action; profile
  version bump; retained-until sweeper as a background job;
  admin UI surfaces ("when did this user last rotate?" and "are
  any retained keys near expiry?"); audit events.

- **1.22.I-i — Scenario 05 + dogfood validation.** Scenario 05
  script in `scripts/dogfood/scenarios/`: studio-a's admin
  marks an asset as `restricted`, shares with studio-b's user,
  observes encrypted envelope delivered + decrypted + the
  restricted content surfaces correctly on b's UI. Acceptance:
  end-to-end works in the dogfood environment with default key
  rotation policies. Validates the entire 1.22.I deliverable.

### Scenarios — the canonical regression catalogue

Five scenarios make up the canonical federation regression catalogue.
Scripts live under `scripts/dogfood/scenarios/` and run against the
two-server dogfood stack. Scenarios 01-04 land in I-a (no encryption
prerequisite); Scenario 05 lands in I-i.

- **Scenario 01 — Basic pairing + Like.** studio-a's `alice` Likes
  studio-b's `bob`'s public post. Within sub-1s, `bob` sees the
  Like with peer attribution. Validates: HTTP-Sig + envelope sig +
  LISTEN/NOTIFY + delivery worker + inbox dispatcher + Like
  handler + remote-actor cache + notification routing.
- **Scenario 02 — Share collection + cross-instance comment.**
  studio-a's admin shares a public-asset collection with studio-b's
  `bob`; `bob` comments on a shared asset; `alice` sees the comment.
  Validates: aa:Share + access gate + Create(Note) handler + reply
  resolution.
- **Scenario 03 — Defederation cascade.** studio-a defederates
  studio-b after Scenarios 01/02 have active state. Observe
  aa:Unshare emissions, outbox cancel-by-peer, peer marked disabled,
  inbox refuses subsequent activity from studio-b, audit log shows
  cascade event. Most likely to surface ordering bugs.
- **Scenario 04 — Restricted content (pre-I-h).** studio-a tries to
  share restricted content with studio-b. Expected before 1.22.I-h
  ships: unconditional sender-side refusal; admin banner explains
  "encrypted federation requires Phase 1.22.I." After I-h ships:
  scenario flips to validate capability check, then succeeds if
  capability is advertised on both ends.
- **Scenario 05 — Restricted content end-to-end (post-1.22.I-i).**
  studio-a shares restricted content with studio-b's user; observe
  encrypted envelope; b decrypts with X25519 private key; restricted
  content renders correctly on b's UI; rotation of b's keys at
  +1 day still allows in-flight decryption via retained key.
  Final 1.22.I acceptance test.

### Seed dataset

The dogfood `seed.sh` script needs a curated dataset that
exercises every federated entity type meaningfully: at least 3
users per instance, sample public + team + restricted assets,
collections with mixed sensitivity, posts of each type, comments
+ likes across users. The dataset is its own deliverable;
likely overlaps with the existing Phase 1.48 public-demo seed
pack (per ADR 0045) — same CC0 source catalogue (Blender Demo
Files, Kenney, Open Game Art, Met Museum, NASA, Sintel /
Big Buck Bunny, Project Gutenberg).

The dataset surface is intentionally NOT specified in this ADR —
it's an operational artifact that evolves with the test
scenarios. Track in `scripts/dogfood/seed/MANIFEST.json` with
per-asset attribution. Dataset can be regenerated freely; no
permanence claim.

## Consequences

### Positive

- **Encryption design is informed by real two-instance behavior
  before implementation lands.** The integration questions
  (key lifecycle, capability negotiation, rotation timing) get
  concrete answers from running Scenarios 01-04 against the
  current build before encryption work starts.
- **Permanent dev surface for all future federation work.**
  Phase 1.22.J, future cross-instance presentation rooms (#97),
  federation moderation, federation observability — every
  federation-touching phase tests against the same dogfood rig.
- **Canonical regression catalogue.** The 5-scenario set becomes
  the federation regression test you can re-run any time the
  surface changes. New federation features add new scenarios;
  the existing scenarios catch any wire-layer regressions.
- **Production-parity local dev.** mkcert + bridge networking +
  separate Postgres per instance means a local dogfood run
  closely mirrors what an operator's two paired instances would
  look like. Bugs found locally are bugs that would have hit
  production.
- **Defederation cascade ordering gets exercised.** Scenario 03
  catches the ordering bugs that single-instance tests
  structurally can't.
- **The encryption sub-phase plan is concrete.** I-b through I-i
  are scoped + sequenced; each has a known integration test in
  the dogfood rig that validates it.

### Negative

- **Operational complexity for first-time setup.** mkcert install,
  `/etc/hosts` entries, two separate compose stacks running side
  by side — more friction than `docker compose up` for new
  contributors. Mitigated by the `up.sh` script handling the
  bootstrap.
- **Resource cost on the dev host.** Two stacks ≈ 2× CPU + 2× RAM
  + 2× disk. Most dev machines handle this but it's a real cost
  to call out. The `down.sh` / `reset.sh` story matters.
- **The seed dataset is real work.** Curating ~50-100 CC0 assets
  with proper attribution is a small but non-zero effort. Lands
  with I-a as part of the dogfood setup; reused for the Phase
  1.48 demo when that ships.
- **Permanent dev-surface maintenance.** When the federation
  protocol changes (new activity types, new endpoints), the
  scenario scripts need updating. Canonical regression catalogue
  is a maintenance commitment.
- **Key rotation grace period creates a key-material retention
  window.** 7 days of retained-for-decrypt keys means more attack
  surface if an old key is exfiltrated. Mitigation: keys retained
  at rest are encrypted with the master key like the current key;
  retention is for decrypt only (never sign).

## Alternatives considered

- **Single-instance integration tests only, defer dogfood.**
  Considered. Rejected because encrypted federation specifically
  has cross-process integration questions (key lifecycle,
  capability negotiation, rotation timing) that single-instance
  tests structurally can't answer. Dogfood IS the prerequisite
  for I, not a parallel nice-to-have.
- **Cloud-hosted two-instance staging.** Considered. Rejected
  for local-dev iteration speed; staging makes sense post-1.22.I
  as a release-gate validation but not as the primary dev
  surface.
- **Lazy X25519 keypair generation.** Considered. Rejected per
  Decision 1 above — simplicity wins over the tiny DB footprint
  savings.
- **Dedicated `/federation/users/{username}/keys` endpoint.**
  Considered. Rejected per Decision 2 — actor profile inline
  matches the ActivityPub story and avoids a parallel cache
  layer.
- **Implicit capability negotiation** (try encrypted; fall back
  to plaintext on 4xx). Considered. Rejected because the failure
  mode is opaque (peer responds with `encryption_not_supported`
  per existing §12.1; we'd parse the reason and decide). Better
  to know up front from the handshake which peers support what.
- **30-day key rotation grace period.** Considered. Rejected as
  the default; 7 days is the right balance for in-flight message
  draining without expanding the key-retention attack surface.
  Admin-tunable per instance for operators with different
  threat models.

## Implementation

Sub-phases land sequentially per the breakdown above.
1.22.I-a (dogfood infrastructure) must land first; everything
else is gated on it. Sub-phases I-b through I-i can land in
order or with light parallelism between non-conflicting pairs
(e.g., I-d capability negotiation and I-h key rotation flow are
mostly independent surfaces).

Spec doc updates land alongside each sub-phase:
`docs/spec/federation/v1.md` §6 (encryption) gets concrete
in I-b (X25519 surface), I-d (capability negotiation),
I-h (rotation policy). The §12.1 catalogue gains
`decrypt_failed` in I-f.

Each scenario script in `scripts/dogfood/scenarios/` runs as a
shell script driving the admin + user APIs of both instances
in sequence. Scenarios are idempotent against the seed dataset
(re-running a scenario against a fresh seed produces the same
observable end state).

The seed dataset assembly + per-asset attribution lands as part
of 1.22.I-a's deliverable (with overlap to the Phase 1.48
public-demo seed pack when that ships).

## References

- ADR 0017 — Monetization & licensing. Master-key-on-host pattern
  reused for X25519 private-key at-rest encryption.
- ADR 0020 — Asset gating & NDA workflow. Sensitivity tiers
  (restricted, embargo) are the federation paths that require
  encryption.
- ADR 0024 — Privacy & consent. Federated user PII handling
  applies to retained federation keys + actor profile responses.
- ADR 0042 — Distributed catalogs. New typed constants for
  capability tokens, key versions, encryption-algorithm names
  go in `app/internal/federation/` per the convention.
- ADR 0043 — Federation walled-garden protocol. Defines the
  §1.22.I sub-phase originally; this ADR fleshes it out with
  the specific decisions + dogfood prerequisite.
- ADR 0044 — Activities ledger CQRS-lite. The dispatcher's
  capability-aware refusal pattern composes with the ledger +
  outbox separation.
- ADR 0045 — Public demo / ephemeral sandboxes. The seed
  dataset overlaps the demo-mode seed pack; both pull from the
  same CC0 source catalogue.
- [mkcert](https://github.com/FiloSottile/mkcert) — local CA
  for development TLS.
- [NaCl box construction](https://nacl.cr.yp.to/box.html) —
  X25519 + XSalsa20 + Poly1305 envelope authentication.
- Future cross-instance presentation rooms over SSE ([#97](https://github.com/mscrnt/artist-alley/issues/97)) —
  future enhancement that benefits from the same dogfood rig.
