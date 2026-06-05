---
id: "0043"
title: Federation — artist-alley walled-garden protocol (ActivityPub-shaped, no public fediverse interop at v1)
status: accepted
date: 2026-06-04
area: architecture
phases:
  - "1.22"
supersedes:
  - "0007"
related:
  - "0006"
  - "0008"
  - "0011"
  - "0020"
  - "0024"
  - "0034"
tags:
  - architecture
  - federation
  - protocol
  - activitypub
  - walled-garden
  - p2p
excerpt: >-
  Artist Alley instances federate with each other through an
  ActivityPub-shaped protocol — but federation is artist-alley to
  artist-alley only at v1, not to the public fediverse. The protocol
  is built ground-up in Go, uses plain JSON with a versioned schema
  (no JSON-LD), Ed25519 signed envelopes, custom activity vocabulary
  for our domain (Asset / Approve / Annotation / WorkflowTransition /
  AssetVersion), and CAS-native URIs that dedupe bytes across the
  network. Trust is per-instance allowlist + optional curated
  directory.
---
## Context

[ADR 0007](/adr/0007-federation-thinking-ahead/) committed Artist Alley
to "thinking ahead" about federation — schema fields like
`origin_server_id` on every federation-relevant table, content-addressed
storage that works across instances by construction, capability-coded
permissions that translate across instances. That preparation is in
place. What ADR 0007 deliberately left open was the actual protocol:
*how* Artist Alley instances would communicate with each other.

This ADR closes that question.

### Three federation shapes considered

The shape of a federation deeply changes the engineering, the
operational story, and the moderation surface. Three concrete shapes
were on the table:

1. **Internal multi-instance (clustering).** A single org runs
   multiple instances (HQ + studio offices). All instances are owned
   by the same entity. Postgres logical replication + storage sync
   would solve it. This is *clustering*, not federation in the
   social-network sense.
2. **Inter-org bilateral.** Studio A pairs explicitly with Studio B.
   Trust established at pairing. Closed network of known peers.
3. **Public fediverse (Mastodon-shaped).** Any compatible instance
   talks to any other. Trust governed by per-instance moderation.
   Open network.

The third — public fediverse — is the largest scope and the most
ambitious story, and our domain (posts + comments + likes + follows
+ subscribes + reposts) maps cleanly onto **ActivityPub**, the W3C
standard the Mastodon / PeerTube / Pixelfed / Pleroma / Bookwyrm
ecosystem uses. PeerTube and Pixelfed are the closest precedents —
both are asset-platforms participating in the public fediverse.

### Why we are not joining the public fediverse at v1

Three reasons pulled us back from full public-fediverse
participation:

1. **Operational + moderation cost.** Joining the open fediverse
   means accepting any well-behaved Mastodon instance as a peer by
   default. That brings spam, harassment, content-policy enforcement,
   defederation politics, takedown protocols, abuse-vector
   engineering. All real, all permanent. Per-instance moderation
   becomes a permanent engineering concern, not a feature.
2. **Compatibility tax.** Mastodon, Pleroma, Misskey, GoToSocial, and
   newer servers each ship subtly different ActivityPub
   interpretations and de facto extensions. Public fediverse
   conformance is a moving target maintained by tracking real-world
   interop bugs. We pay that tax in engineering time forever.
3. **Lost design freedom.** Public-fediverse interop forces our
   protocol to look like Mastodon-shaped ActivityPub. Custom activity
   types (`Approve`, `RequestChanges`, `Annotation`,
   `WorkflowTransition`, `AssetVersion`) have to either degrade
   gracefully on Mastodon clients or not exist. Content-addressed
   URIs (`cas://<sha256>`) can't be assumed. JSON-LD context
   resolution becomes a hard requirement.

### Why a walled garden is the right v1 — without painting ourselves into a corner

The artist-alley federation needs to do real federation work — assets
flowing between studios, comments threading across instances,
cross-org collaboration on brand workspaces, federated review
sessions. That work doesn't require talking to Mastodon. It requires
talking to *other artist-alley instances*.

The walled garden unlocks substantial design freedom:

- **Drop JSON-LD entirely.** Plain JSON with a versioned schema. No
  context resolver, no LD-signature pitfalls, no Mastodon-vs-Pleroma
  context fights. `@context: "artist-alley/v1"` is just a version
  string our parser knows.
- **First-class custom activity types from day one.** Every peer is
  Artist Alley; every peer understands `Approve` natively. No fallback
  to `Note` / `Image` for compatibility.
- **CAS-native URIs.** `cas://<sha256>` resolves locally when the
  content already exists, falls back to origin fetch when it
  doesn't. Studio B never re-downloads an asset Studio C already has.
  Massive bandwidth + storage win across the federation.
- **Ed25519 signed envelopes.** Embed the signature in the activity
  rather than relying on HTTP-Signatures-with-evolving-FEP-extensions.
  Faster, simpler, fewer subtle security gotchas.
- **End-to-end encryption for restricted / embargo content.** NaCl-box
  per recipient actor key. Mastodon can't mandate this because they
  can't mandate client support. We can because every peer is us.
- **Strict-typed Go interfaces.** Every activity is a Go struct with
  no `interface{}` escape hatch. `go vet` catches schema drift.
- **Bilateral, not network-wide, trust.** Each operator's allowlist
  is their concern. Defederation is a row delete. No abuse-against-
  the-whole-fediverse story to engineer.

The future-compatibility cost is small if we are disciplined: the
internal protocol is ActivityPub-shaped enough that a future "open
to the public fediverse" decision becomes a thin translation layer
(our custom activities degrade to `Note` / `Image`, our CAS URIs
resolve to HTTPS attachment URLs, our envelope signatures translate
to HTTP-Signatures), not a rewrite. The escape hatch stays open by
construction.

### What we already have that this builds on

- **`origin_server_id`** on every federation-relevant table (per ADR
  0007 + ADR 0011). Every entity has a stable cross-instance
  identity.
- **Content-addressed storage** (ADR 0008). Bytes keyed by SHA-256
  are location-independent. The same storage layer serves bytes
  fetched from a peer the same way it serves local bytes.
- **Capability codes** as dotted-namespace strings. Translate across
  instances cleanly.
- **Audit log infrastructure** (Phase 1.40 / ADR 0033). Federation
  events emit `federation.*` audit category.
- **Job queue** (Phase 1.15.A). Delivery workers ride this.

## Decision

Build an **artist-alley federation protocol**, ActivityPub-shaped,
walled-garden at v1, ground-up in Go using `go-fed/activity` only as
a reference for the spec. The protocol is closed to non-artist-alley
peers at v1; the design preserves the option to open later without
rewrite.

### Protocol shape — ActivityPub-aligned

The data model follows ActivityPub's Actor / Activity / Object /
Collection vocabulary so the mental model is familiar and the future
public-fediverse compatibility option stays open. Departures from
the spec are deliberate and documented.

**Actors.** Every Artist Alley user is an Actor identified by a
URL: `https://studio-a.example/users/alice`. Webfinger-style
discovery is supported (`@alice@studio-a.example` → actor URL) but
not required; the peer registry (below) handles most discovery in
practice.

**Activities.** Activities are emitted from Actors and delivered to
Actor inboxes. Standard activity types we use: `Create`, `Update`,
`Delete`, `Follow`, `Accept`, `Reject`, `Undo`, `Like`, `Announce`
(repost), `Block`.

**Custom activity types** for our domain (artist-alley-only):

- `aa:Share` — explicit grant of access to an object for a paired
  peer (or specific user on a paired peer). Carries object,
  recipient, scope, optional `expires_at`. **Required precondition
  for any object-related activity to flow.**
- `aa:Unshare` — revoke a previous `aa:Share`.
- `aa:Asset` — extends `Document` with technical metadata (color
  profile, EXIF, content hash, file extension, source DCC tool,
  embedded thumbnails).
- `aa:Approve` / `aa:RequestChanges` / `aa:MarkReviewed` —
  federated review actions. Carries the asset reference + reviewer
  identity + comment text.
- `aa:Annotation` — a brush stroke / highlight / text-comment-on-
  region attached to an asset, video timecode, or PDF page coord.
- `aa:WorkflowTransition` — federated workflow state changes.
- `aa:AssetVersion` — version-bumps without resending bytes.
- `aa:Subscribe` — subscribe to a collection / workspace / user's
  output (sender requests inclusion; recipient must grant via
  `aa:Share`).
- `aa:Mention` — explicit mention with cross-instance addressing.

**Objects.** Standard: `Note`, `Image`, `Video`, `Document`,
`Collection`, `OrderedCollection`. Custom: `aa:Asset`,
`aa:Post`, `aa:Workspace`, `aa:BrandKit`.

**Collections.** Actors have `outbox`, `inbox`, `followers`,
`following`, plus artist-alley-specific: `aa:subscriptions`,
`aa:reviews`.

### Serialization — plain JSON, versioned schema

The on-the-wire payload is a plain JSON object with a `@context`
field carrying just the protocol version string:

```json
{
  "@context": "https://artist-alley.org/protocol/v1",
  "type": "aa:Approve",
  "id": "https://studio-a.example/activities/123",
  "actor": "https://studio-a.example/users/alice",
  "published": "2026-06-04T10:00:00Z",
  "object": "https://studio-a.example/assets/abc123",
  "to": ["https://studio-b.example/users/bob"],
  "cc": ["https://studio-a.example/users/alice/followers"],
  "aa:comment": "Approved with minor lighting note.",
  "aa:reviewedAt": "2026-06-04T10:00:00Z",
  "signature": {
    "type": "Ed25519",
    "publicKey": "https://studio-a.example/users/alice#main-key",
    "value": "base64-encoded-signature"
  }
}
```

No JSON-LD context resolution. No `@id` / `@type` aliasing. The
`@context` field is a version marker, not a Linked Data context.
Parser is a strict-typed Go decoder; unknown fields are rejected at
the protocol layer (not silently ignored).

A future public-fediverse compatibility mode would add a
translator that accepts the standard
`"https://www.w3.org/ns/activitystreams"` context and degrades our
custom types to nearest standard equivalents — but that translator
does not ship at v1.

### Authentication — Ed25519 signed envelopes

Every activity carries an embedded `signature` block signed by the
actor's Ed25519 key. The signed payload is the canonicalized JSON
of the activity *excluding* the `signature` field itself, using
RFC-8785 JSON canonicalization.

**Why embedded signatures over HTTP-Signatures.** HTTP-Sig signs
the transport (the HTTP request), not the message. When an activity
is relayed (e.g., a `Boost` propagation), the original signature
doesn't survive the transport hop. Embedded signatures travel with
the message — survives relays, survives storage, survives audit
replay.

**HTTP-Sig still applies at the transport layer** for inbox POST
authentication (every peer signs the HTTP request itself, by
draft-cavage HTTP-Signatures). Two layers of authentication:
the transport layer authenticates "this request really came from
that peer instance"; the embedded signature authenticates "this
activity was emitted by that user."

### Trust model — peer pairing is the channel, not the access grant

This is the load-bearing security property of the protocol.

**Pairing a peer establishes a trusted communication channel —
nothing more.** It does not share posts, collections, workspaces,
brand kits, comments, likes, follows, or any other object. After a
successful pairing, both instances simply *know about* each other
and can authenticate each other's signed requests.

**Every shared object is an explicit, separate opt-in.** To share
a post / collection / workspace / brand kit with a paired peer (or
with a specific user on a paired peer), an authorised local user
takes an explicit "Share" action in the UI. That action emits an
`aa:Share` activity granting access; the recipient peer stores the
grant; subsequent `Create` / `Update` / `Delete` / `Like` /
`Annotation` activities related to that object flow to the
recipient peer.

The mental model is **Slack Connect**, not the open fediverse.
Connecting two workspaces doesn't share channels; each channel is
explicitly shared. Same here: pairing an instance doesn't share
content; each shared object is explicitly granted.

**Trust tiers** describe what the *channel* can carry, not what
content auto-flows:

- `connected` — handshake complete. Activities about explicitly-
  shared objects flow in both directions. No content is shared by
  default. **The standard tier.**
- `directory-listed` — operator additionally opts in to appearing in
  the curated directory (Phase 1.22.G) so other instances can
  discover and request pairing.
- `auto-sync` — opt-in per-peer for instances within a single trust
  domain (e.g., HQ ↔ remote studio under the same corporate roof)
  that want certain object classes to share automatically per a
  saved policy. Never the default. Per-policy granular: "auto-share
  posts I publish to the `brand-bible` collection with HQ" rather
  than blanket auto-share.

**Encryption tier** is orthogonal to trust tier:

- `plaintext` — plain payloads for public / team-tier content.
- `e2e-encrypted` — NaCl-box envelopes for restricted / embargo-
  tier content (per ADR 0020). Mandatory for restricted content
  going over any peer link the operator has classified as
  untrusted-network.

**Optional curated directory.** An optional artist-alley.org-
hosted directory lists known artist-alley instances that have opted
in (one HTTP endpoint, JSON, signed by the directory server). An
operator can subscribe to the directory to receive *suggestions*
for peers to pair with; they still have to complete the pairing
handshake manually. The directory is not a trust mechanism — it is
a discovery aid.

**Defederation** is a row delete in `federation_peers`. All
`federation_shares` rows targeting that peer cascade-delete; the
peer is sent a final `aa:Unshare` activity for every previously-
shared object so they can purge their local copies. Per-peer mute
/ block tooling at the admin level lives in the Phase 1.20 / 1.21
moderation surface. Per-user block of a remote user is the same
`Block` activity Mastodon ships.

### Opt-in sharing model — `aa:Share` / `aa:Unshare`

Every shareable object class has a "shared with" relationship
maintained in `federation_shares`. The protocol activities that
manage it:

- **`aa:Share`** — grant access to a paired peer (or specific user
  on a paired peer) for a single object. Specifies the object, the
  target peer, the target user (or `null` for "any user on that
  peer"), the scope (`view` / `comment` / `annotate` / `edit`),
  and optional `expires_at`. The recipient peer stores the grant
  and starts accepting related activities.

- **`aa:Unshare`** — revoke a previous `aa:Share`. The recipient
  peer removes the grant and purges any locally cached copies of
  the object.

The originating instance maintains the canonical truth of who has
access. The recipient peer's local cache is best-effort. When the
origin emits an `aa:Update` or `aa:Delete` for a shared object, the
activity is delivered only to peers / users currently on the
`federation_shares` list for that object.

**Inbound activities are filtered against the share list.** A
peer can only send activities about an object if they hold a current
share grant for it. An `aa:Annotation` from Studio B targeting an
asset they were never granted is rejected at the inbox layer.

**Sharing actions are audited.** `federation.share.granted` /
`federation.share.revoked` audit events name the actor, the object,
the recipient peer + user, the scope, and the correlation ID.

**Sharing actions are reversible by both ends.** The originator
revokes via `aa:Unshare`. The recipient can decline (or stop
caching) at their own discretion. Either side can defederate the
whole peer link, which cascades.

**The follow graph is a special case of sharing.** Following a
remote user is an `aa:Share`-style grant *they* give *us* to receive
their `Create` / `Update` / `Like` / `Announce` activities about
their own content. A `Follow` request from a remote user is approved
or denied by the local user (per Phase 1.21 follow controls). No
auto-follow.

### Content-addressed federation — `cas://` URIs

Every reference to asset bytes in a federated activity uses our
content-addressed URI scheme:

```
cas://<sha256>?content-type=image/png&size=12345
```

Resolution rule on the receiving peer:

1. **Check local storage first.** If the SHA-256 is present locally,
   we already have the bytes — use them directly. No transfer.
2. **Check the originating peer's `attributedTo` URL.** Issue a
   signed GET to `{origin}/assets/{hash}/bytes` to fetch the bytes.
3. **Cache the bytes locally** (content-addressed storage means the
   bytes are deduplicated automatically across local + federated
   content).

Across a federation of `N` instances all collaborating on the same
brand kit, the bytes for any single asset transfer at most `N-1`
times — and in practice transfer far fewer times because anyone
who already has the asset for any other reason (private upload,
prior federation, dual-tenancy) skips the fetch.

**This is the single biggest scaling win in the design.** PeerTube
has to move every byte across federation. We don't.

### End-to-end encryption for restricted content

Activities targeting restricted-tier or embargo-tier assets (per
ADR 0020) optionally use a NaCl-box-style encryption envelope:

```json
{
  "@context": "https://artist-alley.org/protocol/v1",
  "type": "aa:Annotation",
  "id": "https://studio-a.example/activities/456",
  "actor": "https://studio-a.example/users/alice",
  "encrypted": {
    "alg": "nacl-box",
    "ephemeralKey": "base64-ephemeral-pubkey",
    "recipients": [
      {
        "actor": "https://studio-b.example/users/bob",
        "ciphertext": "base64-encrypted-payload-for-bob"
      },
      {
        "actor": "https://studio-c.example/users/carol",
        "ciphertext": "base64-encrypted-payload-for-carol"
      }
    ]
  },
  "signature": { ... }
}
```

Only the listed recipient actors can decrypt the payload. The
sender encrypts once per recipient using their published Ed25519
keys (X25519 conversion).

**Sensitivity-tier policy:** restricted and embargo assets use
encrypted envelopes by default when federated; public and team-tier
assets use plain payloads.

### Custom Go package — `internal/federation/`

Built ground-up. `go-fed/activity` consulted only as a reference for
"what does the spec say"; no go-fed source is imported into our
codebase.

```
app/internal/federation/
├── doc.go                  Package overview + protocol spec link
├── envelope.go             Activity envelope + signature verification
├── canonical.go            RFC-8785 JSON canonicalization
├── ed25519.go              Sign / verify primitives
├── vocab.go                Activity + Object Go type registry
├── nacl/                   NaCl-box E2E encryption envelope
├── cas/                    `cas://` URI resolver + remote fetch
├── inbox/                  Inbox endpoint + activity dispatch
├── outbox/                 Outbox + delivery worker (job-queue-backed)
├── peer/                   Peer registry + handshake + trust tiers
├── webfinger/              Webfinger discovery (actor URL resolution)
├── mod/                    Moderation hooks (block lists, rate limits,
│                           content filters, abuse detection)
├── activities/             One file per custom activity type
│   ├── approve.go
│   ├── annotation.go
│   ├── workflow_transition.go
│   ├── asset_version.go
│   └── subscribe.go
└── http_sig.go             draft-cavage HTTP-Signatures for transport
```

No JSON-LD library dependency. JSON parsing via stdlib +
`go-json-experiment/json` for the canonicalization. Ed25519 via
stdlib. NaCl-box via `golang.org/x/crypto/nacl/box`. Net effect:
under 5 dependencies total, all license-clean for our commercial
path.

### Database schema additions (Phase 1.22)

Four new tables. `federation_shares` is the load-bearing access-
control table; nothing flows to a peer without a row here.

```sql
CREATE TABLE federation_peers (
    id                       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    instance_url             TEXT NOT NULL UNIQUE,
    instance_public_key      TEXT NOT NULL,         -- Ed25519 public key, PEM
    display_name             TEXT NOT NULL,
    trust_tier               TEXT NOT NULL DEFAULT 'connected'
                             CHECK (trust_tier IN ('connected', 'directory-listed', 'auto-sync')),
    encryption_policy        TEXT NOT NULL DEFAULT 'plaintext'
                             CHECK (encryption_policy IN ('plaintext', 'e2e-encrypted')),
    enabled                  BOOLEAN NOT NULL DEFAULT TRUE,
    handshake_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    handshake_by_user_ref    BIGINT NOT NULL,
    last_seen_at             TIMESTAMPTZ NULL,
    notes                    TEXT NULL,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE federation_shares (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    object_kind     TEXT NOT NULL                   -- 'post', 'collection', 'workspace', 'brand_kit', 'asset', 'user'
                    CHECK (object_kind IN ('post', 'collection', 'workspace', 'brand_kit', 'asset', 'user')),
    object_id       UUID NOT NULL,                  -- FK enforced at app layer (varies by object_kind)
    peer_id         UUID NOT NULL REFERENCES federation_peers(id) ON DELETE CASCADE,
    target_user_url TEXT NULL,                      -- specific user on the peer; NULL = any user on that peer
    scope           TEXT NOT NULL DEFAULT 'view'
                    CHECK (scope IN ('view', 'comment', 'annotate', 'edit')),
    granted_by_user_ref BIGINT NOT NULL,
    granted_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at      TIMESTAMPTZ NULL,
    revoked_at      TIMESTAMPTZ NULL,
    revoked_by_user_ref BIGINT NULL,
    UNIQUE (object_kind, object_id, peer_id, target_user_url)
);

CREATE INDEX federation_shares_active_idx
    ON federation_shares (object_kind, object_id)
    WHERE revoked_at IS NULL AND (expires_at IS NULL OR expires_at > NOW());
CREATE INDEX federation_shares_peer_idx
    ON federation_shares (peer_id)
    WHERE revoked_at IS NULL;

CREATE TABLE federation_inbox (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    peer_id         UUID NOT NULL REFERENCES federation_peers(id) ON DELETE CASCADE,
    activity_id     TEXT NOT NULL,                  -- the activity's id field
    activity_type   TEXT NOT NULL,                  -- 'Create', 'aa:Approve', etc.
    actor_url       TEXT NOT NULL,
    received_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    processed_at    TIMESTAMPTZ NULL,
    payload         JSONB NOT NULL,                 -- the full activity
    signature_valid BOOLEAN NOT NULL,
    status          TEXT NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending', 'processed', 'rejected', 'error')),
    error_message   TEXT NULL,
    UNIQUE (peer_id, activity_id)
);

CREATE TABLE federation_outbox (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    activity_id     TEXT NOT NULL,
    activity_type   TEXT NOT NULL,
    actor_user_ref  BIGINT NOT NULL,
    target_peer_id  UUID NOT NULL REFERENCES federation_peers(id) ON DELETE CASCADE,
    payload         JSONB NOT NULL,
    enqueued_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    sent_at         TIMESTAMPTZ NULL,
    status          TEXT NOT NULL DEFAULT 'queued'
                    CHECK (status IN ('queued', 'sent', 'failed', 'cancelled')),
    attempt_count   INTEGER NOT NULL DEFAULT 0,
    last_error      TEXT NULL,
    next_attempt_at TIMESTAMPTZ NULL
);

CREATE INDEX federation_inbox_pending_idx ON federation_inbox (peer_id, status) WHERE status = 'pending';
CREATE INDEX federation_outbox_queued_idx ON federation_outbox (status, next_attempt_at) WHERE status = 'queued';
```

Per [ADR 0042](/adr/0042-distributed-catalogs-typed-per-package/), the
catalogue of activity types, trust tiers, and status values lives as
typed Go constants in `internal/federation/` and is mirrored by the
CHECK constraints above.

### HTTP endpoints (Phase 1.22)

- `GET /.well-known/webfinger?resource=acct:alice@studio-a.example`
  — actor URL resolution.
- `GET /.well-known/nodeinfo` — instance metadata (software name,
  version, supported protocols).
- `GET /users/{username}` — Actor JSON.
- `GET /users/{username}/outbox` — Actor outbox (paginated).
- `GET /users/{username}/followers` — Followers collection.
- `GET /users/{username}/following` — Following collection.
- `POST /users/{username}/inbox` — Inbox delivery endpoint.
- `POST /federation/peers/handshake` — Peer handshake endpoint.
- `GET /federation/instance` — Instance Actor (for instance-level
  activities like server announcements).
- `GET /assets/{hash}/bytes` — CAS resolution endpoint (signed
  request from a known peer required).
- `GET /federation/directory` (optional) — instance opt-in directory
  listing.
- Admin: `GET/POST/DELETE /admin/federation/peers` — peer management
  surface.

### Audit + observability

- Audit category `federation.*` per ADR 0033 —
  `federation.peer.created`, `federation.peer.defederated`,
  `federation.activity.received`, `federation.activity.rejected`,
  `federation.delivery.success`, `federation.delivery.failed`.
- Metrics under `federation_` prefix per Phase 1.41 —
  `federation_peers_total{tier}`, `federation_activities_received_total{type}`,
  `federation_delivery_duration_seconds{peer}`,
  `federation_signature_verification_failures_total`,
  `federation_cas_dedup_hits_total`,
  `federation_cas_fetch_bytes_total{peer}`.

### Phase 1.22 sub-phase breakdown

- **1.22.A — Protocol library + envelope.** `internal/federation/`
  package with envelope serialization, RFC-8785 canonicalization,
  Ed25519 sign/verify, NaCl-box encryption, vocabulary types,
  HTTP-Sig transport authentication. Pure Go, no HTTP yet. Tests +
  conformance vectors.
- **1.22.B — Peer registry + handshake.** `federation_peers` table,
  handshake protocol, `/admin/federation` admin surface, peer
  enable/disable/defederation. Per-instance allowlist working end
  to end. **No content shared yet — pairing alone is just the
  channel.**
- **1.22.C — `aa:Share` / `aa:Unshare` + the federation access
  control layer.** `federation_shares` table, `aa:Share` and
  `aa:Unshare` activity types, share-action UI surface ("Share with
  peer…" on posts / collections / workspaces / brand kits), inbound
  activity filtering against the share list (any activity targeting
  an object the sending peer does not hold a current share for is
  rejected at the inbox layer with an audit event). This is the
  load-bearing security work — every subsequent sub-phase depends
  on it.
- **1.22.D — Inbox + outbox + delivery worker.** HTTP endpoints,
  inbox dispatch, outbox + job-queue-backed delivery with retry +
  backoff, CAS fetch on inbound activities. Outbox dispatch
  consults `federation_shares` to determine which peers receive
  each activity.
- **1.22.E — Custom activity types.** Wire `Create` (for posts and
  asset publications), `aa:Approve` / `aa:RequestChanges`,
  `aa:Annotation`, `aa:AssetVersion`, `aa:Subscribe`, plus the
  standard `Follow` / `Like` / `Announce` / `Block`. Per-activity
  handler tests against fixture peers.
- **1.22.F — CAS-native federation.** `cas://` URI scheme, dedup
  on inbound asset references, signed CAS fetch endpoint guarded
  by `federation_shares` (a peer can only fetch bytes for objects
  they hold a current share for), cross-instance dedup metrics.
- **1.22.G — Moderation hooks.** Per-peer block lists, content
  filters, rate limits, abuse detection. Integrated with Phase 1.20
  / 1.21 moderation surface.
- **1.22.H — Curated directory (optional).** Hosted directory on
  artist-alley.org, opt-in subscription, signed directory updates.
  Discovery only; pairing still requires explicit handshake.
- **1.22.I — End-to-end encryption.** NaCl-box envelopes for
  restricted / embargo tier activities (per `encryption_policy`
  on `federation_peers`), recipient key distribution via peer
  handshake.
- **1.22.J — Auto-sync policies (deferred).** Per-peer per-policy
  auto-share rules ("auto-share posts I publish to `brand-bible`
  with HQ"). Only for `auto-sync`-tier peers. Deferred until basic
  federation is proven; the manual `aa:Share` flow is the v1
  default.

## Consequences

### Positive

- **Federation done right for our domain.** Posts + comments + likes
  + follows + reposts + annotations + reviews all federate
  natively. The product is genuinely multi-studio without operator
  glue.
- **CAS-native federation is the single biggest scaling win in
  the design.** Studio B never re-fetches an asset Studio C already
  has. PeerTube can't do this; we can.
- **Custom activity types ship from day one.** `Approve`,
  `Annotation`, `WorkflowTransition`, `AssetVersion` are all
  native — no `Note`-style fallback gymnastics.
- **Walled garden moderation is operationally tractable.** Per-
  instance allowlist means abuse vectors are limited to peers an
  operator chose to trust. No "the whole fediverse can DM you"
  problem.
- **Opt-in sharing is the load-bearing security property.** Peer
  pairing alone shares nothing. Every object exposed across the
  federation requires an explicit `aa:Share` action. Accidental
  leak is structurally impossible: there is no code path that
  delivers an activity about an object to a peer not on its share
  list. Audit answers "who shared what with whom when" directly
  from `federation_shares`. DSAR propagation has a complete list of
  recipients to notify.
- **End-to-end encryption for restricted content** is possible
  because every peer is us. Public fediverse can't do this.
- **The Go library has under 5 dependencies.** Lean, audit-able,
  no JSON-LD context resolver, no AGPL contamination.
- **Future-compatible escape hatch preserved.** ActivityPub-shaped
  protocol with custom types degrades to standard fediverse with
  a thin translator if we ever open. Cheap to keep that door open
  by being disciplined.
- **Phase 1.22 is now scoped and shippable.** Eight sub-phases,
  each well-bounded, no open research questions.

### Negative

- **Real engineering investment.** Eight sub-phases is months of
  work. Building our own protocol library is meaningful effort —
  even simplified, federation is non-trivial.
- **Walled garden gives up public-fediverse network effects.**
  Artist Alley users can't follow their friends on Mastodon
  natively. We accept this trade-off; the public-fediverse story
  is a future ADR if we want it.
- **Standards-body governance is ours.** ActivityPub conformance
  questions get answered by us, not by the W3C / SocialCG. Our
  `@context` version is our problem.
- **Per-peer key distribution is a real operational concern.** Peer
  key rotation, compromised keys, recovery flows — all need
  thinking through in 1.22.B.
- **CAS fetch authorization needs care.** A peer asking
  `GET /assets/{hash}/bytes` for restricted content requires
  per-activity policy enforcement on each fetch, not just at
  publish time. The handler has to know "is this requesting peer
  on the recipient list of an activity that grants access to
  this hash?"
- **DSAR propagation is a real engineering deliverable, not a
  checkbox.** A user requests deletion → we have to emit
  `Delete` activities to every peer that received the content.
  Walled garden makes this tractable (we know the peers); does
  not make it free.
- **No JSON-LD means harder interop with general-purpose
  ActivityPub tools.** Standard libraries assume JSON-LD context
  resolution. Our serialization works for artist-alley peers but
  rejects standard-fediverse-shaped payloads at the parser.

## Alternatives considered

- **Public-fediverse interop via ActivityPub (Mastodon-shaped).**
  Considered as Option C in the federation-shape discussion. Rejected
  for v1 due to moderation operational cost, compatibility tax, and
  lost design freedom. Documented as a future ADR if the project
  ever pivots.

- **`go-fed/activity` as an embedded dependency.** Considered.
  Rejected because (a) we want custom activity types as first-class
  members, not bolted-on; (b) we want to drop JSON-LD; (c) the
  release pace lags spec evolution; (d) the type-interface model
  forces impedance mismatch against our Postgres schema; (e)
  embedding adds dependency surface for capabilities we don't use.
  `go-fed` remains a reference for "what does the spec say".

- **Matrix (via Dendrite).** Considered as the federation backbone.
  Rejected for the asset / social / moderation layer because (a)
  Matrix's room model is the wrong shape for posts + comments + likes
  + follows; (b) we don't need real-time chat across federation as
  the primary use case; (c) running Matrix infrastructure adds
  operational complexity; (d) Synapse is AGPL (incompatible with our
  commercial model) and Dendrite is less feature-complete. The
  Matrix evaluation may be revisited for the *real-time
  presentation rooms + cross-instance chat* concern in a future
  ADR.

- **Custom protocol from scratch (no ActivityPub shape).** Considered.
  Rejected because the ActivityPub data model is genuinely good for
  our domain — Actor / Activity / Object maps cleanly to user / verb
  / target — and adopting it keeps the public-fediverse compatibility
  escape hatch open at trivial cost.

- **Multi-region Postgres logical replication.** Considered for the
  internal-multi-instance story (clustering, ADR 0007 Option 1).
  This ADR does not preclude it — clustering inside a single
  trust domain can use Postgres replication for the storage layer,
  with this federation protocol layered on top for the cross-tenant
  surface. Specifics in a follow-on ADR if needed.

- **AT Protocol (Bluesky).** Considered. Rejected for v1 because
  AT is heavily oriented toward public-network indexing and
  account-portability scenarios that don't match our walled-garden
  goal. Could be revisited as a bridge if we ever want cross-platform
  syndication.

## Implementation

[Sub-phase breakdown documented in the Decision section, 1.22.A
through 1.22.H.]

Phase 1.22 work lands as one milestone with eight epic issues
(one per sub-phase). The protocol-library work (1.22.A) is the
gating sub-phase; everything else builds on it.

Library implementation tracks the spec in
`docs/spec/federation/v1.md` (to be authored alongside 1.22.A).
The spec doc + this ADR are the references contributors work from.

Per [ADR 0042](/adr/0042-distributed-catalogs-typed-per-package/),
the catalogue meta-index at [`docs/catalogs.md`](../catalogs.md) gets
new entries for federation activity types, trust tiers, inbox/outbox
status codes when 1.22.A lands.

## References

- [ActivityPub W3C Recommendation](https://www.w3.org/TR/activitypub/)
  — the data-model shape we follow.
- [ActivityStreams 2.0 Vocabulary](https://www.w3.org/TR/activitystreams-vocabulary/)
  — the type vocabulary we extend.
- [RFC 8785 — JSON Canonicalization Scheme](https://datatracker.ietf.org/doc/html/rfc8785)
  — canonicalization for the signed-envelope payload.
- [draft-cavage HTTP Signatures](https://datatracker.ietf.org/doc/html/draft-cavage-http-signatures-12)
  — transport-layer authentication.
- [Webfinger RFC 7033](https://datatracker.ietf.org/doc/html/rfc7033)
  — actor URL discovery.
- [NaCl box construction](https://nacl.cr.yp.to/box.html) — E2E
  envelope encryption.
- [PeerTube](https://joinpeertube.org/) — asset-platform precedent
  in the public fediverse.
- [Pixelfed](https://pixelfed.org/) — image-platform precedent in
  the public fediverse.
- [`go-fed/activity`](https://github.com/go-fed/activity) — Go
  ActivityPub library, used as spec reference only.
- ADR 0006 — Go as target backend.
- ADR 0007 — Federation thinking ahead (superseded by this ADR).
- ADR 0008 — Storage architecture (CAS).
- ADR 0011 — Asset entity (`origin_server_id`).
- ADR 0020 — Asset gating & NDA workflow (sensitivity tiers).
- ADR 0024 — Privacy & consent (DSAR propagation).
- ADR 0034 — Capability add-ons (federation is *not* an add-on; in-
  tree core capability).
- ADR 0042 — Distributed catalogs (federation type catalogues live
  in `internal/federation/`).
