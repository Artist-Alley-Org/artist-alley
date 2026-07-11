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
- §7 vocabulary → `W3C-AS2-Vocab` + ADR 0043
- §8 URI shape → ADR 0043; Mastodon's deployed username-immutability convention is a behavioral observation cited via `Mastodon-dev-1`
- §10 HTTP-Sig algorithm allowlist → `draft-cavage-12`
- §13 at-rest encryption → `NIST-800-38D`

## How to add a new source

1. Open a PR adding a row to this file.
2. Provide URL + license + coverage + date.
3. Reviewer verifies the source is not source code of any
   implementation in the contamination gate above.
4. Once merged, the source is available for citation in v1.md.
