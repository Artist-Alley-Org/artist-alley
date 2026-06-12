# ArchivePub

**Federated digital asset management protocol — draft v0.x**

| | |
|---|---|
| **Title** | ArchivePub — Federated Digital Asset Management Protocol |
| **Status** | Draft (v0.x — actively in development) |
| **Latest version** | This document |
| **Canonical home** | [`docs/protocol/archivepub.md`](.) (will move to a dedicated domain when v1.0 is finalised) |
| **Reference implementation** | [Artist Alley](https://github.com/mscrnt/artist-alley) — see [`app/internal/federation/`](../../app/internal/federation/) |
| **Editor** | The Artist Alley contributors |
| **Predecessor / data model** | [ActivityPub](https://www.w3.org/TR/activitypub/) (W3C Recommendation, 2018) |

## Abstract

ArchivePub is a federated protocol for sharing digital assets and their
associated workflow state between independently-operated Digital Asset
Management (DAM) instances. Built on the ActivityPub data model, it
extends the activity + actor vocabulary with asset-shaped object types
(assets, posts, collections, brand workspaces) and asset-shaped
activity types (sharing with access controls, approval, annotation).

Where ActivityPub solves federated social messaging, ArchivePub solves
federated archival + creative collaboration: studios reviewing each
other's work, museums sharing condition reports, brand teams
distributing kits to partner agencies, archives loaning collections
without centralising authority. Federation is walled-garden by default
(peer-registered, not open-fediverse-discoverable), encrypted-by-policy,
and access-controlled per object — but the data model is open and the
protocol is implementable by anyone.

## Status of this document

This is an actively-maintained **draft**. The protocol surface tracks
the reference implementation's federation arc (Phases 1.22.A–1.22.J in
the Artist Alley roadmap). The document version `v0.x` will become
`v1.0` when the federation arc completes its dogfood validation week
(see [ADR 0049](../adr/0049-encrypted-federation-and-dogfood.md)).

Expect changes. Specifically expect:
- The namespace prefix (currently `aa:` in the reference impl, see
  [§ Open questions](#open-questions)) may change before v1.0.
- The encryption envelope (`aa:Envelope` with NaCl-box, planned for
  Phase 1.22.I) is reserved but not yet shipped.
- The capability-negotiation handshake (Phase 1.22.I-d) will surface
  protocol-version negotiation hooks not yet present in v0.x.

Implementers writing against v0.x SHOULD pin to a specific commit of
this document and the reference implementation. Don't build a
production deployment on v0.x and expect a clean upgrade — the
purpose of the v0.x window is to discover the spec, not to ship it.

When v1.0 is published, this document will move to a dedicated home
(provisional: `archivepub.org`) and a versioned change log will track
breaking + non-breaking revisions per the standard
major.minor.patch convention.

## Conventions

The key words "MUST", "MUST NOT", "REQUIRED", "SHALL", "SHALL NOT",
"SHOULD", "SHOULD NOT", "RECOMMENDED", "MAY", and "OPTIONAL" in this
document are to be interpreted as described in
[RFC 2119](https://www.rfc-editor.org/rfc/rfc2119) when, and only when,
they appear in all capitals.

## 1. Introduction

### 1.1 Motivation

Existing federated content protocols (ActivityPub, AT Protocol,
Nostr, Matrix) are designed for social messaging, microblogging,
forums, or chat. Their data models centre on short-form posts,
real-time conversations, and follower graphs.

Digital Asset Management is a different problem. The objects are:
- **Versioned** (an asset has revisions, metadata, derivatives, parents)
- **Multi-format** (a single asset is an image + a 3D model + an audio
  track + reference docs — each with their own viewer + metadata)
- **Workflow-shaped** (assets move through draft → review → approved
  → archived states with audit trails)
- **Access-controlled per object** (not all collaborators see all
  assets; brand kits + restricted content + NDA-bound material need
  per-tier visibility)
- **Long-lived** (an asset's primary value accrues over decades, not
  the hours typical of social posts)

No existing federated protocol fits this shape. Studios, museums,
galleries, libraries, and archives that want to share work across
institutional boundaries today have two options: a proprietary SaaS
DAM that owns the storage (Bynder, Frame.io, AEM Assets) or
filesystem rsync. Neither preserves the metadata + workflow + access
controls that make the work meaningful.

ArchivePub fills the gap.

### 1.2 Non-goals

ArchivePub explicitly does NOT:
- **Replace ActivityPub for social messaging.** A Mastodon instance
  is not an ArchivePub server. An ArchivePub server is not a Mastodon
  instance. The shared data model lets them coexist on the same wire
  if an implementation chooses, but neither implies the other.
- **Define a global public namespace.** Federation is walled-garden:
  peer-paired with explicit trust, not openly discoverable. Operators
  who want public-fediverse interop layer that on separately (see
  Issue [#108](https://github.com/mscrnt/artist-alley/issues/108) on
  the reference implementation roadmap).
- **Specify storage.** ArchivePub describes wire format + activity
  semantics, not how the underlying bytes are stored. Implementations
  MAY use filesystem, S3-compatible object storage, IPFS, or anything
  else.
- **Define a viewer.** Per-format rendering (image, 3D, audio, PDF,
  font, etc.) is an implementation concern.
- **Mandate authentication mechanism.** Federation envelopes are
  signed (per [§ 4.2](#42-signature)); how an actor proves their
  identity at the server level (cookie, OAuth, mTLS) is local
  implementation detail.

### 1.3 Relationship to ActivityPub

ArchivePub uses ActivityPub's data model verbatim:
- Activities are JSON-LD documents with `@context`, `type`, `actor`,
  `object`.
- Standard activity verbs (`Create`, `Update`, `Delete`, `Follow`,
  `Accept`, `Reject`) carry their ActivityPub semantics.
- Standard object types (`Note`, `Image`, `Person`, `Collection`)
  remain valid; ArchivePub extends rather than redefines.

ArchivePub adds:
- A vocabulary extension namespace
  ([§ 3.1](#31-namespace-and-vocabulary)) for DAM-shaped types
- New activity verbs (`Share`, `Unshare`, `Approve`, `Annotation`)
- New object types (`Asset`, `Post`, `Workspace`, `BrandKit`,
  `Collection` with DAM semantics)
- A signed envelope wrapper ([§ 4](#4-wire-format)) carrying
  signature + (planned) encryption metadata
- A walled-garden peer registry + trust model
  ([§ 5](#5-federation-lifecycle))
- A capability-negotiation handshake (planned for v1.0)
- Object-level access control via the `Share` activity, distinct from
  ActivityPub's follower-based addressing

An implementation that speaks ArchivePub MUST be able to ignore
ActivityPub-native activities directed at it (e.g. a `Follow` from a
Mastodon instance) without erroring. Conversely, an ArchivePub
envelope is structurally valid JSON-LD and parseable by any AP
implementation, even if the unknown extension types are ignored.

## 2. Terminology

| Term | Definition |
|---|---|
| **Server** | An ArchivePub-implementing application instance. Holds local actors, objects, and a federation peer registry. |
| **Actor** | A user, team, or workspace identity that can authoritatively emit activities. Reference impl maps Actor → `user` row. |
| **Peer** | A remote ArchivePub server with which this server has established a pairing. Peers have a trust tier (see [§ 3.4](#34-trust-tiers)). |
| **Walled-garden federation** | Federation where peer relationships are explicitly established (not openly discoverable). The default + only mode in v0.x. |
| **Envelope** | The signed JSON-LD wrapper carrying one activity from one server to another. See [§ 4](#4-wire-format). |
| **Share** | The verb (`aa:Share`) by which an actor grants access to a specific object to a specific peer or actor. See [§ 5.3](#53-sharing-model). |
| **Brand workspace** | A grouping of assets representing a brand's identity (logo, colour palette, type, voice). Federation-shareable as `aa:BrandKit`. |
| **CAS** | Content-Addressed Storage — assets identified by content hash so two servers holding identical bytes can deduplicate at the storage layer. |

## 3. Data model

### 3.1 Namespace and vocabulary

The reference implementation uses the prefix `aa:` for ArchivePub
extensions, mapped to a placeholder URI. **The final prefix + URI for
v1.0 is under review** ([§ Open questions](#open-questions)).

Provisional candidates:
- `arc:` mapped to `https://archivepub.org/ns#`
- `apub:` mapped to `https://archivepub.org/ns#`

Implementers SHOULD treat the namespace prefix as opaque + alias-able.
Activities + objects MUST use the full URI in the `@context` block,
not the prefix.

### 3.2 Activity types

| Verb | Source | Semantics in ArchivePub |
|---|---|---|
| `Create` | AP-native | Used for `aa:Asset`, `aa:Post`, `aa:Collection`, `aa:Workspace`, `aa:BrandKit` creation. Carries the object payload inline. |
| `Update` | AP-native | Replaces the object's fields with the updated payload. Partial updates are NOT supported; resend the full object. |
| `Delete` | AP-native | Soft-deletes the object (server MAY retain bytes for CAS dedup). Sharing relationships SHOULD cascade-cancel. |
| `Follow` | AP-native | Subscribe an actor to another actor's published activities. The `Accept` / `Reject` AP-native verbs are used in the standard handshake. |
| `aa:Share` | ArchivePub | Grant a remote peer or actor access to a specific object. Carries an access tier (`view` / `comment` / `annotate` / `edit` / `admin`). See [§ 5.3](#53-sharing-model). |
| `aa:Unshare` | ArchivePub | Revoke a previously-granted share. Causes the recipient's local cache to mark the object as gone + cascade-cancel any downstream shares the recipient may have re-shared (subject to the recipient's policy). |
| `aa:Approve` | ArchivePub | Transition an object through its workflow_state machine. Includes the new state + actor + optional comment. Audit-logged on both sides. |
| `aa:Annotation` | ArchivePub | Add a frame-anchored, time-anchored, or rect-anchored comment to an asset. Carries an anchor descriptor + body + optional drawing payload. |

Future-reserved (not yet shipped):
- `aa:Loan` — for physical archive mode (Phase 1.51 in the reference impl)
- `aa:Provenance` — for chain-of-custody events
- `aa:Lock` — for explicit exclusive-edit reservation

### 3.3 Object types

| Type | Semantics |
|---|---|
| `aa:Asset` | A versioned digital asset with metadata, file hash, sensitivity tier, owner. Extends `Object`. |
| `aa:Post` | A multi-asset bundle with title + description + author + workflow state. Extends `Note`. |
| `aa:Collection` | A named grouping of posts or assets with visibility tier (`private`, `org-only`, `followers`, `explicit-share`). Extends `Collection` (AP-native). |
| `aa:Workspace` | A studio-level grouping above collections — typically maps to a department or project. |
| `aa:BrandKit` | A specialised workspace carrying brand identity (logo, colour, type, voice). First-class federation citizen. |
| `aa:Annotation` | A frame/time/rect-anchored note attached to an asset. May also be the *object* of an `aa:Annotation` activity. |

ArchivePub MAY use AP-native types (`Note`, `Image`, `Person`) where
they carry no DAM-specific semantics; for example, the actor in a
federation handshake is a standard `Person`.

### 3.4 Trust tiers

Peer relationships are scoped by trust tier:

| Tier | Definition | Defaults |
|---|---|---|
| `connected` | Explicitly paired bilateral peer. Both sides accept activities. | Receives shared content per `aa:Share` grants; can re-share to its own peers only if the originating share permits. |
| `directory-listed` | A peer listed in a (future) curated federation directory. Implies vetted but not bilateral. | Read-only access to public-tier content; cannot receive `aa:Share` grants. |
| `auto-sync` | A peer with an automatic mirroring policy (planned, Phase 1.22.J). | Receives a subset of new objects per a configured policy; bypasses per-object `aa:Share`. |

v0.x SHOULD implement `connected` only; the others are reserved.

### 3.5 Encryption policies

Each peer relationship carries an encryption policy:

| Policy | Behaviour |
|---|---|
| `plaintext` | Envelopes are signed but not encrypted. Suitable for trusted within-organisation peers. v0.x default. |
| `e2e-encrypted` | Envelopes are signed + the content payload is NaCl-box encrypted to the recipient's published X25519 public key. **Planned for v1.0 (Phase 1.22.I).** Reserved in v0.x. |

A peer's encryption policy is announced during the pairing handshake.
Senders MUST honour the recipient's declared policy.

### 3.6 Sensitivity tiers (per-asset)

Independent of the federation peer policy, each `aa:Asset` carries a
sensitivity tier governing local access:

| Tier | Visibility |
|---|---|
| `public` | Visible to anyone with org-level access. |
| `team` | Visible to members of the asset's owning team. |
| `restricted` | Visible only to explicitly granted users. Federation refuses to ship unless the recipient peer has `e2e-encrypted` policy. |
| `embargo` | Like `restricted`, plus a temporal release date. |

The intersection of asset sensitivity + peer encryption policy
determines federation eligibility — `restricted` + `plaintext` peer =
refusal, with a specific reject reason.

## 4. Wire format

### 4.1 Envelope structure

Every federation message is a signed JSON-LD envelope:

```json
{
  "@context": ["https://www.w3.org/ns/activitystreams",
               "https://archivepub.org/ns"],
  "type": "aa:Envelope",
  "id": "https://studio-a.example.org/envelopes/<uuid>",
  "version": "0.1",
  "from": "https://studio-a.example.org/users/alice",
  "to": ["https://studio-b.example.org/users/bob"],
  "created_at": "2026-06-09T12:34:56Z",
  "activity": { /* the inner Activity, an AP-shaped Create/Update/Share/etc. */ },
  "signature": {
    "algorithm": "Ed25519",
    "key_id": "https://studio-a.example.org/users/alice#main-key",
    "value": "<base64 of Ed25519 signature over RFC-8785 canonical JSON of the envelope sans .signature>"
  }
}
```

Envelopes are POSTed to the recipient's per-actor inbox:
- Single delivery: `POST /federation/users/{actor}/inbox`
- Batched delivery (recommended): `POST /federation/inbox/batch` with
  an array of envelopes

The Content-Type is `application/activity+json` (per ActivityPub) or
`application/ld+json; profile="https://www.w3.org/ns/activitystreams"`.

### 4.2 Signature

All envelopes MUST be signed.

- Algorithm: **Ed25519** (per [RFC 8032](https://www.rfc-editor.org/rfc/rfc8032))
- Canonicalisation: **RFC 8785 JCS** (JSON Canonicalization Scheme)
- Signing scope: the entire envelope with the `signature.value`
  field omitted
- Key distribution: actor public keys are published inline in the
  actor's profile document (per ActivityPub `publicKey` convention)

The receiving server MUST verify the signature against the actor's
published key BEFORE processing the inner activity. Verification
failures are rejected with reject reason `envelope_sig_invalid`
(see [§ 4.4](#44-reject-reasons)).

### 4.3 Encryption (planned, v1.0)

Reserved. Per ADR 0049:
- Algorithm: **NaCl-box** (Curve25519 + XSalsa20 + Poly1305)
- Keypair: per-user X25519, generated eagerly on user creation,
  wrapped at-rest with `AA_MASTER_KEY` per the reference impl's
  encryption policy
- Distribution: public keys published inline in the actor profile
  (same channel as the Ed25519 signing keys)
- Failure handling: `encryption_required_but_not_supported` reject
  reason when a peer's policy demands encryption but the sender's
  capability set doesn't include it

Full specification deferred to Phase 1.22.I; this section will be
expanded when the reference implementation lands.

### 4.4 Reject reasons

Standardised reject reasons (sent back as `Reject` activities or
inline HTTP-error payloads):

| Reason | Semantics |
|---|---|
| `envelope_sig_missing` | The envelope had no `signature` field. |
| `envelope_sig_invalid` | The signature failed verification. |
| `envelope_sig_unknown_algo` | The algorithm is not supported. |
| `unknown_object` | The inner activity references an object the receiver doesn't recognise. |
| `unshared_object` | The receiver has no `aa:Share` grant for the referenced object. |
| `encryption_required_but_not_supported` | Recipient's policy demands encryption; sender can't provide it. |
| `plaintext_type_mismatch` | A type that MUST be encrypted arrived as plaintext. |
| `peer_defederated` | The sending peer was previously defederated. |
| `actor_unknown` | The `from` actor isn't resolvable. |
| `clock_skew_too_large` | The envelope's `created_at` is too far from receiver's clock. |
| `replay_detected` | This envelope's `id` was already processed (idempotency cache hit with mismatched payload). |

This list is non-exhaustive and MAY be extended.

## 5. Federation lifecycle

### 5.1 Peer pairing

Pairing is bilateral:
1. Operator A initiates: POST a `aa:PeerRequest` to operator B's
   well-known endpoint
2. Operator B reviews + accepts: POST a `aa:PeerAccept` back to A
3. Both sides record the peer in their `federation_peers` registry
   with trust tier `connected` + an encryption policy negotiated
   during the handshake

There is currently no global discovery; both operators must already
know each other's instance URLs out-of-band.

A future curated directory (Phase 1.22.H in the reference impl) MAY
provide discovery for the `directory-listed` tier. v0.x does not
specify directory behaviour.

### 5.2 Capability negotiation (planned, v1.0)

During the pairing handshake, both sides exchange a capability set:

```json
{
  "protocol_versions": ["0.1"],
  "encryption_algorithms": ["nacl-box"],
  "signature_algorithms": ["Ed25519"],
  "supported_object_types": ["aa:Asset", "aa:Post", "aa:Collection",
                             "aa:Workspace", "aa:BrandKit"],
  "supported_activity_types": ["Create", "Update", "Delete", "Follow",
                               "aa:Share", "aa:Unshare", "aa:Approve",
                               "aa:Annotation"]
}
```

The intersection of both sides' capability sets defines what can be
exchanged. A sender attempting to emit an activity outside the
intersection MUST refuse and surface a clear operator-side error.

Specification deferred to Phase 1.22.I-d.

### 5.3 Sharing model

Sharing is the primary access-control primitive.

An `aa:Share` activity grants the recipient access to a specific
object with a specific permission tier:

```json
{
  "type": "aa:Share",
  "actor": "https://studio-a.example.org/users/alice",
  "object": "https://studio-a.example.org/assets/<uuid>",
  "target": "https://studio-b.example.org/users/bob",
  "aa:permission": "comment",
  "aa:expires_at": "2026-12-31T00:00:00Z"
}
```

Permission tiers:

| Tier | Allowed actions |
|---|---|
| `view` | Read the object + its metadata. |
| `comment` | + Add comments. |
| `annotate` | + Add annotations. |
| `edit` | + Modify metadata (not bytes). |
| `admin` | + Re-share to others (subject to optional re-share-allowed flag). |

The receiving server MUST track active shares in a local
`federation_shares` table + use them as the access-control gate for
any inbound activity referencing the object.

`aa:Unshare` revokes the grant. Defederation (operator-side action)
cascade-cancels all outstanding shares to that peer.

### 5.4 Activity emission

When a local activity occurs that has federation implications
(an asset is created with `aa:Share` already in effect, a comment is
added to a shared asset, a workflow state transitions, etc.) the
emitting server:

1. Writes the activity to its local activities ledger (see
   [ADR 0044](../adr/0044-activities-ledger-cqrs-lite.md))
2. Determines recipients by walking `federation_shares` + `Follow`
   relationships
3. Wraps the activity in an envelope + signs it
4. POSTs to each recipient's inbox (or batches per
   `POST /federation/inbox/batch`)
5. Records delivery state + retries with exponential backoff on
   transient failures

Recipients SHOULD be able to receive the same envelope multiple times
(idempotency via the envelope's `id` field + a replay cache).

### 5.5 Inbox processing

Incoming envelopes follow a multi-stage pipeline:

1. Parse + structural validation
2. Signature verification (per [§ 4.2](#42-signature))
3. Replay-cache check
4. Encryption decryption (when applicable, planned for v1.0)
5. Activity-type dispatch
6. Per-verb access-control check (does `from` have an `aa:Share`
   grant for the referenced object?)
7. Domain-side application (create the local mirror row, append the
   comment, transition the workflow state, etc.)
8. Activity ledger write
9. Audit log entry

Each stage produces an outcome (`accepted` / `rejected` / `deferred`)
and a reason code per [§ 4.4](#44-reject-reasons). The reference
implementation's 13-stage pipeline is documented in
[ADR 0043](../adr/0043-federation-walled-garden-protocol.md) and
shipped at commit `28dea2e`.

## 6. Security considerations

This section is a placeholder for v0.x. The current security
analysis lives in [ADR 0043 § Security](../adr/0043-federation-walled-garden-protocol.md)
and [ADR 0049](../adr/0049-encrypted-federation-and-dogfood.md).

Items the v1.0 version of this section MUST cover:
- Threat model (curious peer, malicious peer, defederated peer,
  compromised user, MitM)
- Key rotation procedures (Phase 1.22.I-h in the reference impl)
- Replay-attack defences (envelope `id` uniqueness + replay cache)
- Clock-skew tolerance bounds + the reject reason for excess
- Audit-log immutability requirements
- Defederation cascade semantics (what happens to shares + cached
  content when a peer is revoked)

## 7. Privacy considerations

Placeholder. v1.0 MUST cover:
- The deliberate walled-garden posture (peers are not publicly
  discoverable)
- What activities + metadata are visible to which peers
- The sensitivity-tier + encryption-policy intersection model
- Operator obligations around defederation (does cached content stay
  or get purged?)
- User-side data subject rights when their content is federated

## 8. Conformance

A conformant ArchivePub implementation MUST:
- Implement Ed25519 envelope signing + verification
- Reject unsigned envelopes
- Implement the per-actor inbox + (recommended) batch inbox endpoints
- Honour `aa:Share` access control before applying inbound activities
- Implement the core activity types (`Create`, `Update`, `Delete`,
  `aa:Share`, `aa:Unshare`)
- Implement the core object types (`aa:Asset`, `aa:Post`,
  `aa:Collection`)
- Emit standard reject reasons per [§ 4.4](#44-reject-reasons)

A conformant implementation MAY:
- Implement `aa:Approve`, `aa:Annotation`, `aa:Workspace`,
  `aa:BrandKit`
- Implement encryption (will be REQUIRED at v1.0 for peers with
  `e2e-encrypted` policy)
- Implement directory-listed or auto-sync trust tiers
- Implement re-sharing semantics with `admin` permission tier

Conformance test vectors (a la W3C ActivityPub) are planned for v1.0
and will live alongside this document.

## 9. Open questions

These are tracked across the document with TBD markers. Summarised
here for reference:

| Question | Resolution path |
|---|---|
| Final namespace prefix (`aa:` / `arc:` / `apub:`) | Defer to v1.0; reference impl renames in a single migration before publication |
| Domain home for the spec | Operator's call; provisional `archivepub.org` |
| Encryption envelope final shape | Phase 1.22.I — see ADR 0049 |
| Conformance test vector format | Phase 1.22.I-i — will be JSON files matching the 5 canonical regression scenarios |
| Public-fediverse compatibility (webfinger + nodeinfo) | Out of scope for v1.0; tracked at reference-impl issue [#108](https://github.com/mscrnt/artist-alley/issues/108) as a separate concern |
| Re-sharing semantics with `admin` tier | Phase 1.22.J auto-sync work will surface the real requirements |

## 10. References

### Normative

- [RFC 2119](https://www.rfc-editor.org/rfc/rfc2119) — Key words for
  use in RFCs
- [RFC 8032](https://www.rfc-editor.org/rfc/rfc8032) — Edwards-Curve
  Digital Signature Algorithm (Ed25519)
- [RFC 8785](https://www.rfc-editor.org/rfc/rfc8785) — JSON
  Canonicalization Scheme
- [ActivityPub (W3C)](https://www.w3.org/TR/activitypub/) — federated
  social networking protocol
- [Activity Vocabulary (W3C)](https://www.w3.org/TR/activitystreams-vocabulary/)

### Informative

- [ADR 0042](../adr/0042-distributed-catalogs-typed-per-package.md) —
  Vocabulary organisation in the reference implementation
- [ADR 0043](../adr/0043-federation-walled-garden-protocol.md) —
  Federation walled-garden protocol design rationale
- [ADR 0044](../adr/0044-activities-ledger-cqrs-lite.md) — Activity
  ledger as source of truth
- [ADR 0049](../adr/0049-encrypted-federation-and-dogfood.md) —
  Encrypted federation + dogfood plan

## 11. Reference implementation

The Artist Alley repository contains the reference implementation:

- Federation transport + inbox/outbox: [`app/internal/federation/`](../../app/internal/federation/)
- Activity types + vocabulary: [`app/internal/federation/vocab.go`](../../app/internal/federation/vocab.go)
- Peer registry + handshake: [`app/internal/federation/peer/`](../../app/internal/federation/peer/)
- Share access control: [`app/internal/federation/shares/`](../../app/internal/federation/shares/)
- Inbox 13-stage pipeline: [`app/internal/federation/inbox/`](../../app/internal/federation/inbox/)
- Outbox dispatcher + delivery worker: [`app/internal/federation/outbox/`](../../app/internal/federation/outbox/)

Phase-tagged commits track each protocol surface; the commit log
labelled `1.22.A` through `1.22.D` is the load-bearing federation
arc of v0.x.

## Appendix A — Canonical regression scenarios

The reference implementation's dogfood validation week (per
[ADR 0049 § Track A](../adr/0049-encrypted-federation-and-dogfood.md))
runs 5 canonical scenarios. These will become the conformance test
vectors at v1.0:

1. **Like across instances** — Studio A's actor likes a Studio B post.
   Like activity round-trips; both sides see the like count update.
2. **Share collection** — Studio A shares its brand workspace with
   Studio B. Brand kit + member assets + tags become visible to
   Studio B with `view` permission.
3. **Defederate cascade** — Studio A defederates Studio B. All
   outstanding shares cancel; Studio B's cached content marks
   `peer_defederated`.
4. **Restricted-share refusal (pre-v1.0)** — Studio A attempts to
   share a `restricted`-sensitivity asset to a `plaintext`-policy
   peer. The share is refused with `encryption_required_but_not_supported`.
5. **Restricted-share success (v1.0)** — Same setup, peer has
   `e2e-encrypted` policy. The share succeeds with the content
   NaCl-box-encrypted to the recipient's published X25519 key.

## Appendix B — Examples

*To be filled in as the federation bug-fix arc surfaces canonical
example envelopes from the dogfood week.*

---

## Document change log

| Version | Date | Changes |
|---|---|---|
| v0.1 | 2026-06-09 | Initial draft created. Captures the state of the reference implementation through commit `929e4a0` (1.22.D shipped). Reserves encryption (v1.0) + capability negotiation (v1.0) + conformance test vectors (v1.0) as future work. |
| v0.2 | 2026-06-11 | 1.22.I-a (dogfood infrastructure) shipped via PR #109 (foundational `scripts/dogfood/*` helpers + paired-instance Scenarios 01-04; closes #98) + PR #110 (CI-resident automation: `ui-pr.yml` + `ui-nightly.yml` running on a containerised self-hosted runner + 270-test regression net, caught five production-class bugs including SPA-fallback at `063a232`). 1.22.I-b (X25519 keypair-per-user) shipped via PR #111: every user account on every running reference-implementation instance now has a current Curve25519 keypair, master-key-wrapped at rest, stored in `federation_user_keys`. No wire-format changes in this revision — key distribution remains reserved for 1.22.I-c, envelope encryption remains reserved for 1.22.I-e + I-f. |
| v0.3 | 2026-06-11 | 1.22.I-c (encryption-key distribution) — **first wire-breaking change** in the v0.* series (v0.1 → v0.2 was code-state hygiene; this revision adds a new envelope-level field). Adds the optional `aa:encryptionPublicKey` envelope extension. Senders that have a current X25519 public key SHOULD include the block on every outbound envelope; receivers SHOULD parse + persist it under the sending actor's URI so future encrypted envelopes (1.22.I-e/I-f) have a known recipient key. Shape: `{"type": "aa:X25519PublicKey", "publicKeyBase64": "<32-byte base64>", "version": <int >= 1>}`. The `type` discriminator MAY be omitted in v0.3 and defaults to `aa:X25519PublicKey`; future algorithm tokens will require the discriminator. Pre-v0.3 peers omit the field entirely; receivers MUST handle absence gracefully — sender refusal is the 1.22.I-g concern. The version is sender-controlled monotonic; rotation lifecycle is 1.22.I-h. No envelope encryption in this revision; remains reserved for 1.22.I-e + I-f. Reference implementation: schema in migration `00008_remote_actor_encryption_keys.sql`, parser in `federation/inbox.extractEncryptionKey`, emission in `federation/outbox.buildEnvelope`, cache + change-detection in `federation/remote.Handler`. **Forward notice**: v0.4 (capability negotiation, 1.22.I-d) and v0.5 (envelope encryption against advertised keys, 1.22.I-e) are also expected to be wire-breaking. Implementers building against the spec during the I-* arc SHOULD pin to a specific revision and treat each minor bump as a re-test trigger until v1.0 stabilises. |
| v0.4 | 2026-06-11 | 1.22.I-d (peer capability negotiation) — **second wire-breaking change**. Handshake offer + confirm envelopes gain an optional `supported_capabilities` field carrying the sender's typed advertisement (JSON array of strings). Receivers compute the INTERSECTION with their own `KnownCapabilities` set and persist the result on `federation_peers.capabilities`; both sides end up with the same set because intersection is commutative. Vocabulary at v0.4: `e2e-encrypted`, `nacl-box`, `x25519`, `ed25519-envelope-sig`, `http2-batched-inbox`. Open on the wire (peers MAY advertise unknown strings, receivers MUST preserve them through round-trip — re-saving a stored row doesn't drop peer-side metadata) but closed in code (this reference implementation only dispatches on `KnownCapabilities`). Receiver-side rule for the nil-vs-empty distinction: a missing `supported_capabilities` field (pre-v0.4 peer) leaves `capabilities_negotiated_at` NULL, surfaces the peer via `ListPeersMissingCapabilities` for operator re-pairing; an explicit `[]` (peer with no overlapping caps) sets `capabilities_negotiated_at = NOW()` with an empty array. The outbox resolver gains a per-recipient gate that consults `SupportsE2E()` when `Input.RequiresEncryption=true` and emits `federation.emission.skipped` (with reason `capability_missing_e2e_encrypted`) for any recipient whose peer hasn't negotiated the required capabilities; gate is dormant at v0.4 (no production caller sets the flag) and lights up at v0.5 (envelope encryption, 1.22.I-e). Reference implementation: schema in migration `00009_peer_capabilities.sql`, vocabulary + helpers in `federation/peer/capabilities.go`, handshake wiring in `federation/peer/handshake.go`, resolver gate in `federation/outbox/resolver.go::applyCapabilityGate`, audit recorder in `audit.Recorder.FederationEmissionSkippedForPeer`. **Forward notice unchanged**: v0.5 (envelope encryption) remains the next wire-breaking change. |
