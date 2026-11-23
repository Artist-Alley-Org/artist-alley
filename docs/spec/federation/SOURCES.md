# Federation protocol — approved spec sources

Per [ADR 0040 §"Approved spec sources catalogue"](../../adr/0040-clean-room-reverse-engineering-methodology.md),
this file is the **exhaustive** catalogue of sources contributors are
permitted to read while drafting [v1.md](./v1.md) or implementing
`app/internal/federation/`.

**A spec claim in v1.md citing a source not listed here does not
land.** New sources are added by PR with review.

The contributor register at [CONTRIBUTORS.md](./CONTRIBUTORS.md)
tracks who has read what.

## Standards documents (public domain or open-license)

| ID | Source | License | Coverage | First read |
|---|---|---|---|---|
| RFC-8785 | [RFC 8785 — JSON Canonicalization Scheme](https://datatracker.ietf.org/doc/html/rfc8785) | IETF Trust (public domain) | Canonicalization rules for the signed envelope payload (§4). Appendix B vectors are the baseline conformance pack. | 2026-06-05 |
| RFC-8032 | [RFC 8032 — Edwards-Curve Digital Signature Algorithm (EdDSA)](https://datatracker.ietf.org/doc/html/rfc8032) | IETF Trust (public domain) | Pure Ed25519 signing primitive used by the envelope signature (§5). | 2026-06-05 |
| RFC-8410 | [RFC 8410 — Algorithm Identifiers for Ed25519, Ed448, X25519, and X448 for Use in the Internet X.509 Public Key Infrastructure](https://datatracker.ietf.org/doc/html/rfc8410) | IETF Trust (public domain) | PEM encoding of Ed25519 public keys (§5.2). | 2026-06-05 |
| RFC-7748 | [RFC 7748 — Elliptic Curves for Security](https://datatracker.ietf.org/doc/html/rfc7748) | IETF Trust (public domain) | X25519 key agreement + birational map from Ed25519 to X25519 (§6.2). | 2026-06-05 |
| RFC-4648 | [RFC 4648 — The Base16, Base32, and Base64 Data Encodings](https://datatracker.ietf.org/doc/html/rfc4648) | IETF Trust (public domain) | base64url-no-padding encoding for signature values (§5.2). | 2026-06-05 |
| RFC-3339 | [RFC 3339 — Date and Time on the Internet: Timestamps](https://datatracker.ietf.org/doc/html/rfc3339) | IETF Trust (public domain) | `published` field timestamp format (§3). | 2026-06-05 |
| RFC-7033 | [RFC 7033 — WebFinger](https://datatracker.ietf.org/doc/html/rfc7033) | IETF Trust (public domain) | Actor URL resolution (Phase 1.22.B). Cited here for §8 actor URI design. | 2026-06-05 |
| draft-cavage-12 | [draft-cavage-http-signatures-12](https://datatracker.ietf.org/doc/html/draft-cavage-http-signatures-12) | IETF Trust (draft) | HTTP-Signatures transport-layer authentication scheme. Cited for §10 algorithm allowlist. The full HTTP-Sig usage spec lands when 1.22.D wires HTTP transport. | 2026-06-05 |
| NIST-800-38D | [NIST SP 800-38D — Recommendation for Block Cipher Modes of Operation: Galois/Counter Mode (GCM)](https://csrc.nist.gov/publications/detail/sp/800-38d/final) | Public domain (US Government work) | AES-GCM authenticated encryption used by the at-rest master-key wrapper (§13). | 2026-06-05 |
| W3C-ActivityPub | [ActivityPub W3C Recommendation](https://www.w3.org/TR/activitypub/) | W3C Document License | The data-model shape (Actor / Activity / Object / Collection) that v1 is shaped around. v1 deliberately diverges from ActivityPub on serialization (no JSON-LD), addressing (CAS URIs), and vocabulary (`aa:*` extensions); v1.md documents each divergence. | 2026-06-05 |
| W3C-AS2-Core | [Activity Streams 2.0 Core](https://www.w3.org/TR/activitystreams-core/) | W3C Document License | Core JSON shape Activity Streams 2.0 defines for activities + objects. | 2026-06-05 |
| W3C-AS2-Vocab | [Activity Streams 2.0 Vocabulary](https://www.w3.org/TR/activitystreams-vocabulary/) | W3C Document License | Standard activity types we carry through (Create / Update / Delete / Follow / Accept / Reject / Undo / Like / Announce / Block) and standard object types (Note / Image / Video / Document / Collection / OrderedCollection). | 2026-06-05 |
| OCFL-1.1 | [Oxford Common File Layout Specification 1.1](https://ocfl.io/1.1/spec/) | CC BY 4.0 (OCFL editors) | §3.3 immutable, continuously numbered versions and §3.4 SHA-256 / SHA-512 content digests plus additional fixity; the version rules of §17.2.1 and the checksum rules of §17.2.3. | 2026-11-17 |
| RFC-8493 | [RFC 8493: The BagIt File Packaging Format (V1.0)](https://www.rfc-editor.org/rfc/rfc8493) | IETF (Informational RFC) | §2.1.3 payload manifests (`manifest-<algorithm>.txt`, several algorithms per bag) and `bag-info.txt`; §17.2.3 and the §17.7 export mapping. | 2026-11-17 |
| PREMIS-3 | [PREMIS Data Dictionary for Preservation Metadata, Version 3.0](https://www.loc.gov/standards/premis/v3/) | Library of Congress (US Government work) | The Objects, Events, Rights and Agents data model; the §17.4 event shape and agent roles. | 2026-11-17 |
| LoC-PreservationEvents | [Preservation Events Controlled Vocabulary (release 22 June 2017)](https://www.loc.gov/standards/premis/v3/preservation-events.pdf) | Library of Congress (US Government work) | The primary list of PREMIS preservation event terms, with definitions. It confirms the eight term labels §17.4 uses: ingestion, fixity check, format identification, virus check, migration, replication, transfer, deaccession. Six of them also carry an `id.loc.gov/vocabulary/preservation/eventType` link in that release; format identification and transfer do not, which is why §17.4 maps its tokens to term labels rather than to identifier URIs. | 2026-11-17 |
| IIIF-P3 | [IIIF Presentation API 3.0](https://iiif.io/api/presentation/3.0/) | CC BY 4.0 (IIIF Consortium) | The `rights` property (Creative Commons or RightsStatements.org URIs) and `requiredStatement` language maps (§17.2.5); manifests and canvases (§17.2.7, §17.5). | 2026-11-17 |
| W3C-WebAnno | [Web Annotation Data Model](https://www.w3.org/TR/annotation-model/) | W3C Document License | `Annotation`, `TextualBody`, `SpecificResource` and §4.2.1 FragmentSelector (§17.5). | 2026-11-17 |
| W3C-MediaFrags | [Media Fragments URI 1.0 (basic)](https://www.w3.org/TR/media-frags/) | W3C Document License | The `xywh=` and `t=` fragment syntax the §17.5 selector uses. | 2026-11-17 |
| DCMI-Terms | [DCMI Metadata Terms](https://www.dublincore.org/specifications/dublin-core/dcmi-terms/) | CC BY 4.0 (DCMI) | The descriptive terms of §17.2.6 and the rights terms behind §17.2.5. | 2026-11-17 |
| RightsStatements | [RightsStatements.org vocabulary](https://rightsstatements.org/page/1.0/) | Published by RightsStatements.org | The rights statement URIs §17.2.5 accepts. | 2026-11-17 |
| CC-Licenses | [Creative Commons licenses](https://creativecommons.org/licenses/) | Published by Creative Commons | The licence URIs §17.2.5 accepts. | 2026-11-17 |
| PRONOM | [PRONOM technical registry](https://www.nationalarchives.gov.uk/PRONOM/) | Published by The National Archives (UK) | PRONOM persistent unique identifiers (`fmt/N`, `x-fmt/N`) in §17.2.4. | 2026-11-17 |
| RFC-6838 | [RFC 6838: Media Type Specifications and Registration Procedures](https://datatracker.ietf.org/doc/html/rfc6838) | IETF Trust (public domain) | The `type/subtype` syntax of `mediaType` (§17.2.2). | 2026-11-17 |
| RFC-5646 | [RFC 5646: Tags for Identifying Languages (BCP 47)](https://datatracker.ietf.org/doc/html/rfc5646) | IETF Trust (public domain) | Language tags in §17.2.5, §17.2.6 and §17.5. | 2026-11-17 |

## Reference-implementation pages (READ AS SPEC ONLY — NO SOURCE CODE)

| ID | Source | License | Coverage | First read |
|---|---|---|---|---|
| NaCl-box-ref | [NaCl: Networking and Cryptography library — box construction](https://nacl.cr.yp.to/box.html) | Public domain (Bernstein) | The reference description of the box construction: ephemeral keypair + per-recipient nonce + Curve25519 ECDH + Salsa20-Poly1305 / XSalsa20-Poly1305. Used as the design reference for §6. **The NaCl C source code itself is NOT in this approved-sources list.** Our implementation uses `golang.org/x/crypto/nacl/box` which is the Go-stdlib-affiliated re-implementation; we read its public docs but not its source. | 2026-06-05 |

## Narrative writeups (text describing protocol behaviour, not code)

| ID | Source | License | Coverage | First read |
|---|---|---|---|---|
| Mastodon-dev-1 | Eugen Rochko + Renaud Chaput's published interop-pain-points posts on the Mastodon blog and ActivityPub conformance discussions in the SocialCG mailing list archives | Public web content; cited as narrative observation | §3.2 — the rationale for rejecting unknown fields is informed by the documented divergence between Mastodon and Pleroma over silently-extended fields. Cited as observation of failure mode, not as a source for any specific algorithm. | 2026-06-05 |

## Source-code reading log (CONTAMINATION GATE)

Contributors who have read the source of any of the following are
**excluded from Phase 1 (spec) and Phase 3 (implementation)** work
on this federation project per ADR 0040 §"Contributor quarantine":

- `go-fed/activity` — the Go ActivityPub library cited as a spec
  reference. Reading its **source** contaminates; reading its
  README / godoc / spec citations does not.
- Mastodon (any language; Ruby / Crystal / Elixir implementations).
- Pleroma / Akkoma source.
- Misskey / FoundKey / Calckey / Sharkey / etc. source.
- GoToSocial source.
- PeerTube federation code.
- Pixelfed federation code.
- BookWyrm federation code.
- Lemmy / Kbin / Mbin / piefed federation code.
- Hubzilla / Streams / Forte federation code.
- Friendica / Red Matrix federation code.
- Any commercial ActivityPub implementation source.

Reading the **published wire payloads** these implementations
produce (in HAR captures, packet dumps, or their published HTTP API
docs) is **not** source reading. The wire format is the spec we
target; observing it is Phase 2 of clean-room methodology.

## Source citations linkable from v1.md

For each section of v1.md, the citations resolve as:

- §3 envelope shape → `W3C-ActivityPub`, `W3C-AS2-Core`
- §3.1 `@context` as version string → ADR 0043 §"Serialization"
- §3.2 unknown-field rejection → ADR 0043 + `Mastodon-dev-1`
- §4 canonicalization → `RFC-8785`
- §5 Ed25519 → `RFC-8032`, `RFC-8410`
- §5.2 base64url encoding → `RFC-4648`
- §6 NaCl-box envelope → `NaCl-box-ref`, `RFC-7748`
- §6.5 policy matrix and §6.6 per-recipient `encryption` block → ADR 0049 (Track B, Decisions 2, 4, 5)
- Implementation status and errata → first-party observation of the reference implementation; not a third-party source, so the contamination gate does not apply
- §7 vocabulary → `W3C-AS2-Vocab` + ADR 0043
- §8 URI shape → ADR 0043; Mastodon's deployed username-immutability convention is a behavioral observation cited via `Mastodon-dev-1`
- §10 HTTP-Sig algorithm allowlist → `draft-cavage-12`
- §13 at-rest encryption → `NIST-800-38D`
- §16 negotiated profiles → ADR 0043 (amendment 2026-11-17, #1513) and the §11 capability exchange (ADR 0049, Decision 3)
- §17.2 asset versions, files and checksums → `OCFL-1.1`, `RFC-8493`, `RFC-6838`, `PRONOM`
- §17.2.5 rights and required statement → `IIIF-P3`, `RightsStatements`, `CC-Licenses`, `DCMI-Terms`, `RFC-5646`
- §17.2.6 description → `DCMI-Terms`, `RFC-5646`
- §17.3 tombstone → `W3C-AS2-Vocab` (`Tombstone`)
- §17.4 preservation events → `PREMIS-3`, `LoC-PreservationEvents`; ADR 0101
- §17.5 annotations → `W3C-WebAnno`, `W3C-MediaFrags`, `IIIF-P3`
- §17.7 BagIt mapping → `RFC-8493`
- §18 profile `asset-metadata-1` → ADR 0043 (amendment 2026-11-17, #1513) and ADR 0083 (amendments 2026-11-23) for the negotiation, placement, snapshot, privacy and provenance rules, which are first-party protocol decisions; `W3C-AS2-Vocab` (`Update`); `DCMI-Terms` for `title` and `description`; `IIIF-P3`, `RightsStatements` and `CC-Licenses` for `rights` and `requiredStatement`; `RFC-5646` for the language-map tags

## How to add a new source

1. Open a PR adding a row to this file.
2. Provide URL + license + coverage + date.
3. Reviewer verifies the source is not source code of any
   implementation in the contamination gate above.
4. Once merged, the source is available for citation in v1.md.
