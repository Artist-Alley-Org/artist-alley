# ArchivePub

**Federated digital asset management protocol — v1.0**

| | |
|---|---|
| **Title** | ArchivePub — Federated Digital Asset Management Protocol |
| **Status** | Final (v1.0 — stamped 2026-07-13; soak window closed 2026-06-22 clean) |
| **Latest version** | This document |
| **Canonical home** | [`docs/protocol/archivepub.md`](.) (will move to a dedicated domain when v1.0 is finalised) |
| **Reference implementation** | [Artist Alley](https://github.com/Artist-Alley-Org/artist-alley) — see [`app/internal/federation/`](../../app/internal/federation/) |
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

The document is at **v1.0** (final — stamped 2026-07-13 as a
spec-only change after the 7-day soak window closed 2026-06-22
clean; Appendix A conformance test vectors locked at rc1). The
following items shipped during the v0.x window and are now
stable:

- The namespace prefix is **`aa:`** (finalised at v1.0-rc1; the
  earlier "may change before v1.0" disclaimer is resolved).
- The encryption envelope (NaCl-box, per-recipient, via the
  `encryption` field — see [§ 4](#4-wire-format)) shipped
  across Phases 1.22.I-e through I-i.
- The capability-negotiation handshake shipped at Phase
  1.22.I-d; the `supported_capabilities` field is part of every
  v0.4+ offer / confirm envelope.

Implementers SHOULD pin to a specific commit of this document
and the reference implementation. v1.0 final shipped exactly as
the rc1 plan specified: a no-code, spec-only stamp after the
soak window closed clean.

When v1.0 is published, this document will move to a dedicated
home (provisional: `archivepub.org`) and a versioned change log
will track breaking + non-breaking revisions per the standard
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
  Issue [#108](https://github.com/Artist-Alley-Org/artist-alley/issues/108) on
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
| `e2e-encrypted` | Envelopes are signed + the content payload is NaCl-box encrypted to the recipient's published X25519 public key. Shipped in v0.5 (sender, 1.22.I-e) + v0.6 (receiver, 1.22.I-f). |

A peer's encryption policy is announced during the pairing handshake.
Senders MUST honour the recipient's declared policy.

**v0.7 (1.22.I-g)** completes the encryption story end-to-end with the
sender-refusal policy: from v0.7 forward, `policy: e2e-encrypted`
becomes **enforceable** at the sender side. Pre-v0.7 senders silently
degraded `restricted` shares to plaintext when the recipient couldn't
decrypt; v0.7+ senders **refuse** to dispatch (see §3.6 + §5.3).

### 3.6 Sensitivity tiers (per-asset)

Independent of the federation peer policy, each `aa:Asset` carries a
sensitivity tier governing local access AND federation transmission:

| Tier | Visibility | Federation transmission (v0.7+) |
|---|---|---|
| `public` | Visible to anyone with org-level access. | Best-effort: encrypted when both sides can; plaintext otherwise. |
| `team` | Visible to members of the asset's owning team. | Best-effort: encrypted when both sides can; plaintext otherwise. |
| `restricted` | Visible only to explicitly granted users. | **MUST encrypt**: refuse if recipient peer can't (capability missing OR pubkey unfetchable). |
| `embargo` | Like `restricted`, plus a temporal release date. | **MUST encrypt**: same refusal rule as `restricted`. |

The combined decision matrix (sender side, per recipient peer):

| Tier | Peer e2e + key | Path |
|---|---|---|
| `public`, `team` | yes | Encrypted (opportunistic) |
| `public`, `team` | no | Plaintext (1.22.D backwards-compat) |
| `restricted`, `embargo` | yes | Encrypted (required) |
| `restricted`, `embargo` | no | **REFUSED** — outbox row terminal-fails with `refused_reason=encryption_required_but_unavailable`; no envelope reaches the receiver. |
| unknown / future tier | yes | Encrypted |
| unknown / future tier | no | **REFUSED** (conservative default — unknown tier is treated as required-encryption). |

Unknown tiers default to require-encryption so the failure mode of an
unrecognised tier is "refused + visible in the operator audit log"
rather than "leaked + invisible". Implementers adding a new tier MUST
update this matrix in the same revision.

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
| `decrypt_failed` | The encrypted envelope's ciphertext did not open against any retained receiver key (1.22.I-f). |
| `encryption_required` | **Receiver-side variant** (ACTIVE at v1.0-rc1 / 1.22.I-i): plaintext envelope arrived for a target whose intrinsic sensitivity tier mandates encryption — the sender violated the v0.7 §3.6 policy. Distinct from `encryption_required_but_not_supported`, which is the recipient declaring inability to receive plaintext. Defense in depth: I-g's sender-side refusal is the primary enforcement; this gate refuses what a misbehaving sender should never have sent. **Activation**: the v0.8 gate code shipped with the `SensitivityLookup` callback dormant; v1.0-rc1 wires the callback at boot to resolve `asset`-kind objects via the new `assets.sensitivity` column. Other object kinds (post, collection, comment) currently pass through — they gain coverage as those domains grow their own sensitivity columns. |
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

**Sender refusal policy (v0.7+, 1.22.I-g).** When the shared object's
sensitivity tier is `restricted` or `embargo`, senders **MUST refuse**
to dispatch the activity to any recipient peer where either:

- the peer has not negotiated the `nacl-box` capability during the
  pairing handshake (§5.2), OR
- the recipient actor's published encryption public key is not
  retrievable at dispatch time.

Refusal is per-recipient: a single `aa:Share` activity fanning out to
multiple peers (one e2e-capable, one legacy) emits encrypted to the
capable peer + refuses for the legacy one. The refusal is **terminal**
— the sender's outbox row records the refusal in the audit log
(`federation.emission.refused`) + no envelope reaches the receiver.
Capability changes on the recipient peer do NOT auto-trigger
re-dispatch; operator action (re-pair to refresh capabilities OR move
the share to a lower sensitivity tier) is required.

Senders running v0.7 against legacy peers SHOULD prepare for operator
inquiries about refused shares; the audit log's
`metadata->>'reason' = 'encryption_required_but_unavailable'`
filter is the canonical diagnostic.

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
| Domain home for the spec | Operator's call; provisional `archivepub.org` |
| Public-fediverse compatibility (webfinger + nodeinfo) | Out of scope for v1.0; tracked at reference-impl issue [#108](https://github.com/Artist-Alley-Org/artist-alley/issues/108) as a separate concern |
| Re-sharing semantics with `admin` tier | Phase 1.22.J auto-sync work will surface the real requirements |

Resolved at v1.0-rc1 (previously open):

- **Namespace prefix** — `aa:` is final. The earlier "may change
  before v1.0" disclaimer is removed; the prefix appears
  throughout the wire format (activity-type catalogue, envelope
  field naming) and across the conformance vectors.
- **Encryption envelope final shape** — landed across Phases
  1.22.I-e through I-i. See [§ 4](#4-wire-format) for the
  `encryption` block, [ADR 0049](../adr/0049-encrypted-federation-and-dogfood.md)
  for the design.
- **Conformance test vector format** — Appendix A's table is
  the authority; each row maps to a script under
  `scripts/dogfood/scenarios/`. The 8 active scenarios (01,
  05, 06, 07, 08, 09, 11, 12) are what implementations claiming
  ArchivePub conformance MUST pass.

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

> **Status at v1.0-rc1**: these scenarios are the formal
> conformance test vectors. A conformant ArchivePub
> implementation MUST pass scenarios 01, 05, 06, 07, 08, 09,
> 11, and 12 against a peer running the reference
> implementation. Reference scripts live at
> `scripts/dogfood/scenarios/`; implementers can port or run
> them against their own pair.

The reference implementation's dogfood validation week (per
[ADR 0049 § Track A](../adr/0049-encrypted-federation-and-dogfood.md))
runs these scenarios on every nightly. Each name maps to a
script under `scripts/dogfood/scenarios/`:

| # | Scenario | What it tests |
|---|---|---|
| 01 | `01-like-cross-instance.sh` | Wire signature + dispatch + per-actor inbox (Phase 1.22.D). |
| 05 | `05-restricted-asset-roundtrip.sh` | Receiver-side encryption-required gate fires on plaintext envelopes targeting restricted-tier assets (Phase 1.22.I-i — the cap-stone). |
| 06 | `06-wire-dispatch.sh` | Outbox dispatcher + the sub-1s p99 latency contract via LISTEN/NOTIFY (Phase 1.22.D-b). |
| 07 | `07-encryption-key-distribution.sh` | aa:encryptionPublicKey envelope extension + remote-actor cache (Phase 1.22.I-c). |
| 08 | `08-capability-negotiation.sh` | Handshake intersection + capability persistence (Phase 1.22.I-d). |
| 09 | `09-outbox-encryption-sender-side.sh` | Sender-side NaCl-box envelope encryption + the encrypted envelope shape (Phase 1.22.I-e). |
| 11 | `11-refusal-flip.sh` | Sensitivity-driven sender refusal policy + audit (Phase 1.22.I-g). |
| 12 | `12-rotation-lifecycle.sh` | Key rotation + retained-key fallback + sweeper reap + admin observability (Phase 1.22.I-h). |

Scenarios 02 (share collection), 03 (defederate cascade), and
04 (restricted-share refusal pre-I-h) stay as outline scripts
at v1.0-rc1 — they exercise behavior that's wire-format-stable
but needs more product wiring (the collection-share UI, the
cascade observability dashboard) before they can be
end-to-end-tested without operator-side babysitting.

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
| v0.5 | 2026-06-11 | 1.22.I-e (envelope encryption, sender side) — **third wire-breaking change**. Envelopes gain an optional `encryption` field containing a per-recipient NaCl-box ciphertext + the sender + recipient key id/version metadata the receiver needs for the I-h rotation grace window. Shape: `{"algorithm": "nacl-box-v1", "sender_key_id": "<actor URL>#encryption-key", "sender_key_version": <int>, "recipient_key_id": "<actor URL>#encryption-key", "recipient_key_version": <int>, "nonce": "<base64 24 bytes>", "ciphertext": "<base64>"}`. When `encryption` is present the envelope's activity-payload extras (`actorDisplayName`, `content`, etc.) MUST be absent — the receiver decrypts the ciphertext into the original extras map. Routing-critical fields (`type`, `id`, `actor`, `published`, `to`, `cc`, `object`, `signature`) stay in clear so the inbox can authenticate the sender + dispatch the activity without decrypting. The Ed25519 envelope signature covers the entire envelope including the encryption block (RFC 8785 JCS canonicalization); tampering with the ciphertext, nonce, or metadata invalidates the signature. Per-recipient: each outbox row → one recipient → one NaCl-box seal; no shared envelopes across recipients. Nonce is fresh `crypto/rand` per emission (reuse with the same keypair is catastrophic for XSalsa20). The sender's private key is master-key-wrapped at rest; the dispatcher unwraps + zeros the bytes around `box.Seal`. **Rollout coordination**: the reference implementation REMOVES `nacl-box` from `KnownCapabilities` in this revision so the I-d intersection produces an empty `SupportsE2E` result against every peer — production traffic does NOT encrypt at v0.5 even though the code path exists. The receiver-side decrypt (v0.6, 1.22.I-f) re-adds `nacl-box` to `KnownCapabilities` as its final step + triggers a re-pair. Implementers MUST NOT advertise `nacl-box` until they ship the matching decrypt path; advertising what they can't honor breaks every encrypted envelope at the receiver. Reference implementation: schema in migration `00010_outbox_encryption_metadata.sql` (observability mirror), primitive in `federation.EncryptActivityPayload` / `federation.DecryptActivityPayload` (both ship in I-e so the round-trip tests cover the full pipeline; I-f wires the decrypt side into the inbox), envelope shape in `federation.EncryptionBlock`, dispatcher integration in `federation/outbox/delivery.Worker.tryEncryptFor`, audit recorder in `audit.Recorder.FederationEmissionEncrypted`. **Forward notice**: v0.6 (envelope decryption + retained-key fallback) is the next wire-breaking change. |
| v0.6 | 2026-06-11 | 1.22.I-f (envelope decryption, receiver side + retained-key fallback) — **fourth wire-breaking change** (in name only; the wire shape from v0.5 is unchanged — what changes is that receivers MUST now decrypt envelopes carrying the `encryption` block from v0.5 instead of rejecting them). The reference implementation's inbox dispatcher gains a stage-4 decrypt branch (`federation/inbox.Dispatcher.dispatchOne` between envelope re-parse and verb-handler dispatch): when `env.Encryption != nil` the dispatcher resolves the recipient's local user_ref from `envelope.To[0]`, looks up the sender's pubkey via the I-c remote-actor cache, walks the recipient's retained keys via `inbox.DecryptForUser` (order: `is_current DESC, version DESC` — current key tries first, then any retained-not-expired keys for the rotation grace window) and unwraps + zeros the private scalar per attempt. On success `env.Extra` is restored from the JSON plaintext so every downstream verb handler sees the same view it would on a plaintext envelope; on failure the row transitions to `status=rejected` with a new `reject_reason=decrypt_failed` (catalogued in `InboxStatus`) and the audit recorder fires `federation.inbox.decrypt_failed` with a typed `reason` field (catalogue: `no_keys_walked` / `sender_key_missing` / `recipient_unresolvable` / `no_key_worked`). The happy path fires `federation.inbox.decrypted` with `decrypted_with_key_version` + `attempt_count` (=1 in steady state; ≥2 means the rotation grace window saved a delivery in flight during a key rotation). Per-row observability: `federation_inbox.was_encrypted` (bool) + `decrypted_with_key_version` (nullable int) on every row that took the decrypt branch — the operator dashboard surfaces `is_current` rotation health by grouping on `decrypted_with_key_version`. **Rollout coordination**: this revision RESTORES `nacl-box` to `KnownCapabilities`. The on-disk capability set persisted at handshake time does NOT auto-refresh — operators MUST trigger a re-pair (or wait for the next handshake round-trip) for `CapNaClBox` to land in the intersection + the outbox resolver's per-peer gate (I-d) to light up the encryption branch. Until both sides re-pair the I-d gate emission-skips with reason `capability_missing_naclbox` — encrypted traffic stays paused, the wire-format remains backwards-compatible because the optional `encryption` block is absent from every plaintext envelope. Implementers MUST NOT skip the receiver-side decrypt path before advertising `nacl-box`: a peer that advertises the capability but doesn't decrypt would reject every encrypted envelope back to its sender. Reference implementation: schema in migration `00011_inbox_decryption_metadata.sql` (observability mirror of v0.5's `00010` outbox column), retained-key walk primitive in `federation/inbox.DecryptForUser` (also `federation/userkeys.ListUserKeysForDecrypt` — distinct from the public-key list query so private bytes can't leak via code drift), dispatcher integration in `federation/inbox.Dispatcher.dispatchOne` stage-4 (delegated to `tryDecryptInbound` for readability), three nil-safe boot hooks `SetSenderEncKey` / `SetRecipientUserRef` / `SetAudit` in `app/internal/http/api.go`, capability restoration in `federation/peer.KnownCapabilities`, audit events `audit.Recorder.FederationInboxDecrypted` + `FederationInboxDecryptFailed`. Conformance vector: an encrypted envelope from v0.5 round-trips through v0.6's decrypt walk, env.Extra recovers byte-identical, the verb handler dispatches unchanged. **Forward notice**: v0.7 (sender refusal when recipient has no published encryption key, 1.22.I-g) is the next wire-breaking change. |
| v0.7 | 2026-06-12 | 1.22.I-g (sender refusal flip) — **policy change, not a wire-format change**. v0.5's optional `encryption` field is still optional on the wire; v0.7 makes its ABSENCE a refusal cause for senders dispatching `restricted` or `embargo` sensitivity tiers. Decision matrix (codified in the reference implementation's `outbox.ChoosePathFor` pure function): `public` and `team` tiers continue to send plaintext when capability or key is missing (1.22.D backwards-compat with pre-I-f peers); `restricted` and `embargo` tiers REFUSE to dispatch when either the recipient peer hasn't negotiated `nacl-box` OR the recipient's published X25519 key isn't cached locally. Unknown / future sensitivity tiers default to "require encryption" (conservative — failure mode for an unrecognised tier is "refused + visible in operator audit log" rather than "leaked plaintext + invisible"). Refusal is TERMINAL: the sender's outbox row transitions to `status=refused` with `refused_reason=encryption_required_but_unavailable` (catalogued in `outbox.RefuseReason`), the partial-index on `status='queued'` filters refused rows out of redelivery automatically, no backoff fires, and capability changes on the peer DO NOT auto-trigger re-dispatch (operator action: re-pair → caps refresh OR move the share to a lower tier). Refusal is PER-RECIPIENT, not per-envelope: a single activity fanning out to multiple peers (one e2e-capable, one legacy) results in 1 encrypted dispatch + 1 refusal — the refusal does NOT poison the capable recipient's emission. **New audit event**: `federation.emission.refused` (distinct from the existing `federation.emission.skipped` — semantically `skipped`="informational, not relevant" / `refused`="policy DECISION blocked an emission"; operators grep on the two distinctly). Receiver-side defense-in-depth (a new reject reason `encryption_required` for plaintext envelopes arriving for restricted-share targets) is RESERVED at v0.7 + lights up in v0.8 / 1.22.I-h. From the receiver's perspective v0.7 reads as "the wire is unchanged, fewer envelopes arrive" — no parsing changes needed. Reference implementation: schema in migration `00012_outbox_refusal_policy.sql` (denormalised `sensitivity` + `refused_reason` columns on `federation_outbox` + `status='refused'` admitted in the CHECK), policy primitive in `federation/outbox/policy.go` (`RequiresEncryption` + `ChoosePathFor` + `EmissionPath` + `RefuseReason` + `ErrEmissionRefused`), Worker integration in `federation/outbox.Worker.tryEncryptFor` (refactored to consume `ChoosePathFor`; returns `ErrEmissionRefused` on the refused path; callers in `deliverOne` + `deliverOneBatch` catch + skip the POST), audit recorder `audit.Recorder.FederationEmissionRefused`. **Forward notice**: v0.8 (key rotation lifecycle + receiver-side defense-in-depth, 1.22.I-h) is the next revision. Operator inquiries about "why didn't my Update reach bob?" should land in the I-h policy UI bundle. |
| v0.8 | 2026-06-14 | 1.22.I-h (key rotation lifecycle + receiver-side defense gate) — **not a wire-format change** (the encryption block from v0.5 + the `encryption_required` reject reason reserved at v0.7 are both unchanged at the wire), but a substantive policy + observability addition. Three new local primitives: (a) per-user **rotation** — `POST /account/security/rotate-federation-keys` for self-service + `POST /admin/federation/users/{ref}/rotate-keys` for admin-initiated compromised-key recovery; both call the same atomic-tx primitive (`userkeys.RotateForUser`) that generates a fresh X25519 keypair, demotes the previous current row to retained-with-TTL state, and records `rotated_at` + `rotated_by_user_ref` on both rows (`subject == rotated_by` discriminates self-rotation from operator-initiated recovery in the audit feed). (b) Background **retained-key sweeper** — `userkeys.Sweeper` goroutine ticks every hour (boot-time first sweep covers expirations accumulated during downtime); reaps `federation_user_keys` rows where `is_current = FALSE AND retained_until < NOW()`; emits `federation.user.key_retained_expired` once per non-zero reap (zero-sweeps stay quiet to avoid audit-feed pollution). Default retention window is 30 days, sourced from `system_config.federation.user_keys.retained_until_days`. (c) Receiver-side **encryption_required gate** — counterpart to v0.7's sender-side refusal flip: when an inbound envelope arrives plaintext but its target object's sensitivity tier (resolved via the same `SensitivityLookup` callback contract used by the v0.5 outbox dispatcher) mandates encryption, the receiver rejects with `reject_reason=encryption_required` + fires `federation.inbox.encryption_required_rejected`. **Activation gated**: the `SensitivityLookup` is unwired in the v0.8 reference implementation because per-object sensitivity columns aren't on the local schema yet — `outbox/resolver.go` documents this as a pre-MVP gap. The gate code is fully tested + shipped; flipping it on is one boot-wire edit when those columns land. Peers reading this revision MUST handle the receiver-side reject reason gracefully (as for any reject reason — they were already required to at v0.6) but won't observe it in practice until activation lands. **Three new audit events** at v0.8: `federation.user.key_rotated` (per rotation, both flows), `federation.user.key_retained_expired` (per non-zero sweep), `federation.inbox.encryption_required_rejected` (per receiver-gate firing — dormant in v0.8 production, exercised by unit tests). **New admin observability surface**: `GET /admin/federation/key-health` returns aggregate tile counts (users-without-keypair, remote-actors-missing-enc-key, peers-missing-capabilities, retained-keys-near-expiry, users-total) plus drill-down arrays (users missing keypair, recent rotations) in a single round-trip. **Key-version handling on the wire is unchanged from v0.5/v0.6**: peers don't need to know whether a key version they're seeing is the result of a rotation versus the original mint — the version increment + the I-c cache upsert path handle the propagation transparently. **No bilateral rotation handshake**: a rotated sender's peers pick up the new key on the next inbound activity from the rotated sender via the standard I-c upsert path — we don't push, they pull. **Cross-instance retained-key decrypt** (peer A sends with sender's vN, sender rotated to vN+1, sender still decrypts vN via the retained walk): inherited unchanged from v0.6's `inbox.DecryptForUser` order (`is_current DESC, version DESC`) + the new sweeper's retained_until floor. Reference implementation: migration `00013_user_keys_rotation_metadata.sql` (ALTER `federation_user_keys` ADD `rotated_at` + `rotated_by_user_ref` + system_config retention default), `federation/userkeys.RotateForUser` (atomic-tx primitive), `federation/userkeys.Sweeper` (background goroutine + `SweepOnce` test handle), `federation/inbox.CheckInboundEncryptionPolicy` + `federation/inbox.SetSensitivityLookup` + `federation/inbox.ErrEncryptionRequired` (gate), `federation/userkeys.AdminHandler` (HTTP surface for all three endpoints), three audit events on `audit.Recorder`. Conformance: scenario 12 (`scripts/dogfood/scenarios/12-rotation-lifecycle.sh`) walks Phases A (self-rotation) → B (admin recovery) → C (sweeper reap) → D (admin observability); Phase E (receiver-gate firing) deferred to follow the `SensitivityLookup` activation. **Forward notice**: v1.0 stabilises the wire format + conformance vectors per Track A's dogfood-week findings. No more I-arc revisions planned. |
| v1.0-rc1 | 2026-06-15 | 1.22.I-i (encryption-arc cap-stone — receiver-side defense gate activated) — **not a wire-format change** (gate code + reject reason were both reserved at v0.8); v1.0-rc1 flips the gate from dormant to active by wiring the `SensitivityLookup` callback at boot in the reference implementation. New migration `00014_asset_sensitivity.sql` adds `assets.sensitivity` (NOT NULL DEFAULT 'public' + CHECK over the four tiers + partial index on restricted/embargo for the admin-side observability path). The boot wire (`inboxSensitivityLookup` in `app/internal/http/api.go`) resolves `asset`-kind objects to their tier via the new column; other kinds (post, collection, comment) currently pass through so plaintext federation traffic for those domains continues unchanged until they grow their own sensitivity columns in a future phase. **Behaviour change in production**: an inbound plaintext envelope targeting a restricted-tier (or embargo-tier) asset transitions to `status=rejected` with `reject_reason=encryption_required` + fires `federation.inbox.encryption_required_rejected`. Sender-side I-g refusal remains the primary enforcement; the receiver gate is the defense-in-depth that catches a misbehaving sender ignoring its own policy. **Conformance**: scenario 05 (`scripts/dogfood/scenarios/05-restricted-asset-roundtrip.sh`) is the negative-path test — provisions a restricted asset, injects a plaintext inbox row targeting it, asserts the gate rejects + audits. Appendix A's conformance test vector table is locked at v1.0-rc1; implementations claiming ArchivePub conformance MUST pass scenarios 01, 05, 06, 07, 08, 09, 11, 12 against a peer running the reference. **Intentional deviation from the I-i brief**: shares do NOT denormalise sensitivity at grant time (no `federation_shares.sensitivity` column, no downgrade-rejection in the grant path). The asset's intrinsic tier IS the source of truth; changing it post-share DOES retroactively affect outstanding shares. Tradeoff: simpler implementation + the user mental model "if I mark the asset restricted, all federated copies become restricted." A follow-up phase can layer copy-at-grant semantics on top if operator feedback demands the alternate behavior. **Forward notice**: v1.0 final ships after a 7-day soak window with the active gate. If a real bug surfaces during soak, v1.0-rc2 ships first; otherwise the conformance vectors lock + the document moves to a versioned-changelog cadence. Encryption arc (1.22.I-a through I-i) is feature-complete + dogfood-validated end-to-end at v1.0-rc1. |
