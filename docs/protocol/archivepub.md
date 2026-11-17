# ArchivePub

**Federated digital asset management protocol: overview of v1**

| | |
|---|---|
| **Title** | ArchivePub: Federated Digital Asset Management Protocol |
| **What this document is** | The non-normative overview and design rationale for protocol v1. |
| **Normative contract** | [`docs/spec/federation/v1.md`](../spec/federation/v1.md). Where this overview and `v1.md` differ, `v1.md` governs, and its *Implementation status* section is the authority on what the reference implementation does today. |
| **Latest revision** | v1.1, 2026-11-17 (see the change log) |
| **Reference implementation** | [Artist Alley](https://github.com/Artist-Alley-Org/artist-alley), [`app/internal/federation/`](../../app/internal/federation/) |
| **Editor** | The Artist Alley contributors |

## Abstract

ArchivePub lets independently operated digital asset management (DAM)
instances share assets and their workflow with each other: studios
reviewing each other's work, museums sharing condition reports, brand
teams handing kits to partner agencies, archives lending collections
without a central authority.

Federation is a walled garden. Instances pair explicitly, every message
is signed by its author and by the sending instance, restricted material
is encrypted or not sent at all, and access is granted per object. In v1
the protocol is **Artist Alley to Artist Alley**: the specification is
open and an independent implementation is welcome, but v1 does not claim
to interoperate with ActivityPub servers. Its vocabulary is modelled on
ActivityStreams 2.0 so that a future translator stays possible.

## Status of this document

This overview describes protocol v1, whose normative text is
[`v1.md`](../spec/federation/v1.md). It does not restate field-level
rules; each section points to the part of `v1.md` that defines them.
What is specified is not always implemented yet: `v1.md`'s
*Implementation status* lists the open gaps and the parts that are
specified but not yet built.

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
- **Multi-format** (a single asset can be an image, a 3D model, an
  audio track and reference documents, each with its own viewer and
  metadata)
- **Workflow-shaped** (assets move through draft, review, approved and
  archived states with audit trails)
- **Access-controlled per object** (not all collaborators see all
  assets; brand kits, restricted content and NDA-bound material need
  per-tier visibility)
- **Long-lived** (an asset's value accrues over decades, not the hours
  typical of social posts)

No existing federated protocol fits this shape. Studios, museums,
galleries, libraries, and archives that want to share work across
institutional boundaries today have two options: a proprietary SaaS
DAM that owns the storage, or filesystem copies. Neither preserves the
metadata, workflow and access controls that make the work meaningful.

### 1.2 Non-goals

ArchivePub does not:
- **Replace ActivityPub for social messaging.** An ArchivePub server is
  not a Mastodon instance, and v1 does not exchange messages with one.
  Public-fediverse compatibility is a separate, later concern
  ([#108](https://github.com/Artist-Alley-Org/artist-alley/issues/108)).
- **Define a global public namespace.** Peers pair explicitly; nothing
  is openly discoverable.
- **Specify storage.** It describes the wire format and activity
  semantics, not how bytes are stored.
- **Act as a preservation system.** Virus scanning, format
  identification, migration and storage stay local to each instance.
  The archival profile (section 6) defines how their results travel.
- **Define a viewer.** Rendering each format is an implementation
  concern.

### 1.3 Relationship to ActivityPub

ArchivePub borrows ActivityPub's shape, not its wire format. Messages
are plain JSON, not JSON-LD. `@context` is the exact, opaque string
`https://artist-alley.org/protocol/v1`, compared as a string; no JSON-LD
context is published for it (`v1.md` §3.1). Standard ActivityStreams
verbs keep their meaning, and the `aa:` prefix names the DAM-specific
types. Unknown fields are rejected rather than ignored (`v1.md` §3.2),
which is what keeps independent implementations from drifting apart;
the one sanctioned way to add fields is a negotiated profile
(`v1.md` §16).

## 2. Terminology

| Term | Definition |
|---|---|
| **Server** | An ArchivePub-implementing application instance. Holds local actors, objects, and a federation peer registry. |
| **Actor** | A user identity that authors activities, identified by an actor URI (`v1.md` §8.3). |
| **Peer** | A remote server this server has paired with. Peers have a trust tier (section 3.4). |
| **Walled-garden federation** | Federation where peer relationships are explicitly established, not openly discoverable. The only mode in v1. |
| **Envelope** | One signed JSON activity sent from one server to another (`v1.md` §3). |
| **Share** | The `aa:Share` activity by which an actor grants a peer access to one object (section 5.3). |
| **Profile** | An optional, frozen set of extra envelope members two peers agree on at pairing (`v1.md` §16). |
| **Brand workspace** | A grouping of assets representing a brand's identity (logo, colour palette, type, voice). Federation-shareable as `aa:BrandKit`. |
| **CAS** | Content-addressed storage: bytes are identified by their SHA-256, so two servers holding identical bytes can deduplicate them. |

## 3. Data model

### 3.1 Namespace and vocabulary

The prefix for ArchivePub's own types is `aa:`. It is final. It is a
naming convention inside the type strings; it does not refer to a
JSON-LD namespace, and `@context` carries only the version string.

### 3.2 Activity types

Standard ActivityStreams verbs: `Create`, `Update`, `Delete`, `Follow`,
`Accept`, `Reject`, `Undo`, `Like`, `Announce`, `Block`, plus `Add` and
`Remove` for collection membership.

ArchivePub's own types (`v1.md` §7.2):

| Type | Purpose |
|---|---|
| `aa:Share` | Grant a peer (or one user on it) access to one object, with a scope (section 5.3). |
| `aa:Unshare` | Revoke a share. |
| `aa:RevokeShare` | Reserved for a future scope-narrowing revoke; recognised, and treated as `aa:Unshare`. |
| `aa:Approve`, `aa:RequestChanges`, `aa:MarkReviewed` | Review verdicts. A verdict never changes the object or its workflow state. |
| `aa:WorkflowTransition` | Move an object to another workflow state. It changes the original, so no v1 share allows a peer to send it. |
| `aa:Annotation` | A region or time-range note on an asset; under the archival profile, a W3C Web Annotation on an IIIF canvas (section 6). |
| `aa:AssetVersion` | A new version of an asset. |
| `aa:Subscribe` | Ask to be included in a collection, workspace or feed; granted by `aa:Share`. |
| `aa:Mention` | An explicit cross-instance mention. |
| `aa:Provenance` | **Profile-gated** (archival profile): one PREMIS preservation event about an asset. Added in v1.1. |

That is twelve `aa:` types: eleven in the base catalogue and
`aa:Provenance`, which exists only between peers that negotiated the
archival profile. Being in the catalogue means the parser recognises
the type, not that a receiver applies it: a receiver refuses any type it
does not apply with `unsupported_activity` (`v1.md` §7.6), and today
the reference implementation applies only `Like` and comment `Create`.

### 3.3 Object types

| Type | Semantics |
|---|---|
| `aa:Asset` | A digital asset record. Under the archival profile it carries a file set (each file by CAS hash, with checksums and a role) and an immutable version (section 6). |
| `aa:Post` | A multi-asset bundle with title, description, author and workflow state. |
| `aa:Collection` | A named grouping of posts or assets with a visibility tier (`private`, `org-only`, `followers`, `explicit-share`, `public`). `public` is local read visibility, not a peer audience: a public object reaches a peer only through a live explicit share. |
| `aa:Workspace` | A studio-level grouping above collections, typically a department or project. |
| `aa:BrandKit` | A workspace carrying a brand identity (logo, colour, type, voice). |

### 3.4 Trust tiers

| Tier | Definition |
|---|---|
| `connected` | An explicitly paired, bilateral peer. The only tier implemented. |
| `directory-listed` | Reserved: a peer listed in a future curated directory. |
| `auto-sync` | Reserved: a peer with an automatic sharing policy ([#76](https://github.com/Artist-Alley-Org/artist-alley/issues/76)). |

### 3.5 Encryption policies

Envelopes are always signed. A per-recipient NaCl-box `encryption`
block replaces an envelope's type-specific fields when both sides can
encrypt (`v1.md` §6.6), and is mandatory for restricted and embargoed
material, as the next section shows.

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

This matrix is mirrored normatively in
[`v1.md`](../spec/federation/v1.md) §6.5, and
`outbox.ChoosePathFor` (`app/internal/federation/outbox/policy.go`)
implements it. A change MUST update both documents and the function
together.

## 4. Wire format

### 4.1 Envelope

An envelope is one JSON object (`v1.md` §3): `@context`, `type`, `id`,
`actor`, `published`, optional `to` and `cc`, an `object` URI when the
type needs one, the type's own `aa:` fields, an optional `encryption`
block, and a `signature`. Field order does not matter on the wire; the
signature is computed over the RFC 8785 canonical form (`v1.md` §4).

Envelopes are delivered with an HTTP `POST`, signed with the sending
instance's key (`v1.md` §10), to the receiving instance's
`/federation/inbox`, or to `/federation/inbox/batch` with several
envelopes at once (`v1.md` §10.4).

### 4.2 Signatures

Every envelope carries an Ed25519 signature by its actor, under the key
id `<actor URI>#main-key` (`v1.md` §5). The receiver fetches the
actor's public key once from the sending peer's actor document at the
actor URI and keeps it (`v1.md` §8.3). Separately, the HTTP request is
signed with the sending instance's key, which the two instances
exchanged when they paired. The instance signature says which peer is
speaking; the envelope signature says which of its users wrote it, and
a peer can only speak for its own users.

### 4.3 Encryption

See `v1.md` §6: a per-recipient NaCl-box block (X25519 key agreement,
XSalsa20-Poly1305), with the envelope signature covering the encrypted
form.

### 4.4 Reject reasons

The receiver answers a refused envelope with a typed reason. The list
and the meaning of each reason are in `v1.md` §12.1.

## 5. Federation lifecycle

### 5.1 Peer pairing

Pairing is bilateral. One instance's operator sends an offer to the
other instance's handshake endpoint (`POST
/api/v1/federation/peers/handshake`); the other instance records it as
pending, its operator accepts, and a confirm completes the pairing on
both sides. Each instance then knows the other's instance URL and
public key, and the trust tier is `connected`. There is no discovery:
both operators already know each other's URLs.

### 5.2 Capability negotiation

The offer and confirm each carry `supported_capabilities`, a list of
capability tokens (for example `e2e-encrypted`, `nacl-box`, `x25519`,
`ed25519-envelope-sig`). Each instance stores the intersection of the
two lists when the pairing completes; that stored intersection decides
what may be sent, including any profile (`v1.md` §11, §16). It is
computed at pairing, so a capability added later reaches an existing
pairing only when the two instances negotiate again.

### 5.3 Sharing model

Sharing is the primary access-control primitive. An `aa:Share` grants
one peer, or one user on it, access to one object with a scope and an
optional expiry; its fields are `scope`, `target_peer`, `target_user`
and `expires_at` (`v1.md` §7.5.1).

| Scope | Allowed actions |
|---|---|
| `view` | Read the object and its metadata; react (`Like`, `Announce`). |
| `comment` | Also add comments. |
| `annotate` | Also add annotations. |
| `remix` | Also reuse the shared material in the recipient's own objects on the recipient's own instance. Never modifies the original. |

No v1 scope allows editing the original on the instance that holds it.
The value `edit` is reserved for a possible future scope that would,
and a v1 instance refuses it rather than reading it as `remix`.

`aa:Unshare` revokes a grant, and defederating a peer cancels every
share to it. Whether a share also allows a peer to fetch and keep the
original bytes is a separate decision
([#1573](https://github.com/Artist-Alley-Org/artist-alley/issues/1573)).

A sender refuses, per recipient, to send restricted or embargoed
material that it cannot encrypt for that recipient (section 3.6). A
refusal is final for that delivery and is recorded in the audit log.

### 5.4 Activity emission

When a local activity has federation implications, the emitting server:

1. writes the activity to its activities ledger
   ([ADR 0044](../adr/0044-activities-ledger-cqrs-lite.md));
2. resolves the recipients from live shares;
3. builds one envelope per recipient, encrypting when required or
   possible, and signs it with the actor's key;
4. POSTs it, signed with the instance key, to the peer's inbox (or
   batches several per peer);
5. records delivery state and retries transient failures with backoff.

A receiver accepts the same envelope more than once: an `id` it already
stored is answered from its stored record (`v1.md` §7.6, §10.3).

### 5.5 Inbox processing

In the reference implementation, an incoming envelope goes through:

1. the instance HTTP signature, peer status and rate limit;
2. the stored-id answer for a repeated `id`;
3. strict parsing, `@context` and the signature block;
4. the actor signature (actor bound to the sending peer, key fetched and
   kept, signature verified);
5. the activity-type check (a type this receiver does not apply is
   refused with `unsupported_activity` and recorded, never applied);
6. the object check, then storage;
7. later dispatch: decryption, the encryption-required check, and the
   per-type handler.

Each refusal carries a reason from `v1.md` §12.1.

## 6. Archival assets (profile `archival-1`)

The archival profile (`v1.md` §17) carries what a receiving archive
needs to preserve a shared asset and prove that its files arrived
intact, using the vocabularies archives already use:

- each asset version lists its files by role (original, preservation
  copy, access copy), each by its SHA-256 CAS address, with a checksum
  list (SHA-256 required; MD5 only as an extra for older holdings, as
  BagIt manifests allow), size, declared media type, an optional PRONOM
  format identification with the tool that made it, and an optional
  original file name;
- versions are immutable and linked to the previous one, as in OCFL;
- rights are a Creative Commons or RightsStatements.org URI with a
  required statement, as in IIIF; persistent identifiers, an accession
  number and a Dublin Core description travel with the asset;
- deleting an asset sends a tombstone that keeps its identity and
  checksums, so links do not silently break;
- preservation events (ingestion, fixity checks, format identification,
  transfer, deaccession and others) travel as PREMIS events in
  `aa:Provenance` ([ADR 0101](../adr/0101-preservation-history-as-premis-events.md));
- annotations become W3C Web Annotations on IIIF canvases, with a
  region or time-range selector;
- a receiver re-hashes every file it receives before it is visible and
  rejects a mismatch with `fixity_mismatch`.

Two peers use the profile only when both advertised it at pairing; a
sender refuses to send an asset to a peer without it rather than send
it unverifiable. A file is listed only when its SHA-256 is recorded, so
no unhashed preview is ever presented as checked. A tombstone does not
recall bytes a peer already holds, and a manifest or rights link never
grants permission to fetch bytes. The profile is **specified, not yet
implemented**; `v1.md` *Implementation status* (S1) lists what remains
and which issues own it. The same section of `v1.md` maps the profile
to a BagIt export (§17.7).

## 7. Security considerations

The detailed analysis lives in
[ADR 0043](../adr/0043-federation-walled-garden-protocol.md) and
[ADR 0049](../adr/0049-encrypted-federation-and-dogfood.md). In short:

- every envelope is signed twice, by its actor and by the sending
  instance, and a peer can speak only for its own users;
- restricted and embargoed material is encrypted per recipient or not
  sent;
- a repeated `id` is answered from the stored record, and the HTTP
  signature's date window bounds replay;
- the receiver treats every claim inside an envelope (formats, file
  names, rights, events) as the sender's claim, and verifies fixity
  itself;
- links inside envelopes (manifests, actor documents) are fetched only
  from the sending peer's own origin, never from an arbitrary URL.

## 8. Privacy considerations

- Peers are not publicly discoverable; each pairing is explicit.
- What a peer sees is decided per object by shares and by the object's
  sensitivity tier.
- An original file name can reveal more than intended; the archival
  profile carries only a base name, never a path, and only when the
  sender chooses to.
- Defederating a peer stops future sharing; it cannot recall what the
  peer already received.

## 9. Conformance

The requirements are `v1.md`'s. Its conformance vectors
(`app/internal/federation/testdata/conformance/`, `v1.md` §9) test the
wire primitives and the archival profile's rules; the scenarios in
Appendix A test the reference implementation against itself. Neither is
evidence of interoperability with an independent implementation, which
has not yet been demonstrated.

## 10. Open questions

| Question | Resolution path |
|---|---|
| Domain home for the spec | The operator's call; provisional `archivepub.org`. |
| Public-fediverse compatibility (webfinger, nodeinfo) | Out of scope for v1; [#108](https://github.com/Artist-Alley-Org/artist-alley/issues/108). |
| Re-sharing | Not in v1; the reserved `auto-sync` tier work will surface the requirements. |

## 11. References

### Normative

- [`v1.md`](../spec/federation/v1.md), the protocol v1 contract, and
  its approved sources in
  [`SOURCES.md`](../spec/federation/SOURCES.md)
- [RFC 2119](https://www.rfc-editor.org/rfc/rfc2119), [RFC 8032](https://www.rfc-editor.org/rfc/rfc8032) (Ed25519), [RFC 8785](https://www.rfc-editor.org/rfc/rfc8785) (JSON canonicalization)

### Informative

- [Activity Vocabulary (W3C)](https://www.w3.org/TR/activitystreams-vocabulary/)
- [OCFL 1.1](https://ocfl.io/1.1/spec/), [BagIt (RFC 8493)](https://www.rfc-editor.org/rfc/rfc8493), [PREMIS 3](https://www.loc.gov/standards/premis/v3/), [IIIF Presentation 3.0](https://iiif.io/api/presentation/3.0/), [W3C Web Annotation](https://www.w3.org/TR/annotation-model/), [DCMI Metadata Terms](https://www.dublincore.org/specifications/dublin-core/dcmi-terms/)
- [ADR 0042](../adr/0042-distributed-catalogs-typed-per-package.md), vocabulary organisation in the reference implementation
- [ADR 0043](../adr/0043-federation-walled-garden-protocol.md), federation design rationale
- [ADR 0044](../adr/0044-activities-ledger-cqrs-lite.md), the activities ledger
- [ADR 0049](../adr/0049-encrypted-federation-and-dogfood.md), encrypted federation and the dogfood plan
- [ADR 0101](../adr/0101-preservation-history-as-premis-events.md), preservation history

## 12. Reference implementation

- Federation transport, inbox and outbox: [`app/internal/federation/`](../../app/internal/federation/)
- Vocabulary: [`app/internal/federation/vocab.go`](../../app/internal/federation/vocab.go)
- Archival profile validators: [`app/internal/federation/archival.go`](../../app/internal/federation/archival.go)
- Peer registry and handshake: [`app/internal/federation/peer/`](../../app/internal/federation/peer/)
- Share access control: [`app/internal/federation/shares/`](../../app/internal/federation/shares/)

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

Example envelopes are the conformance fixtures under
[`app/internal/federation/testdata/conformance/`](../../app/internal/federation/testdata/conformance/):
`envelope/` for signed activities and `archival-1/` for the archival
profile's members.

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
| v1.1 | 2026-11-17 | Overview rewritten as a short, non-normative companion to `v1.md`: the draft and JSON-LD descriptions, the old envelope example, the wrong inbox path and reject-reason list, the planned-feature wording and the errata block are removed, and each section points to the part of `v1.md` that defines it. The audience is stated as Artist Alley to Artist Alley at v1. `v1.md` gains negotiated profiles (§16) and the archival profile `archival-1` (§17), specified but not implemented, with the profile-gated twelfth custom type `aa:Provenance` and three reject reasons (#1513). No change to what any deployed instance sends. |
