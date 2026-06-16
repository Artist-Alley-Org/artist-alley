// Package federation implements the artist-alley walled-garden
// federation protocol — ActivityPub-shaped, artist-alley-to-
// artist-alley only at v1.
//
// The wire-format contract lives in [docs/spec/federation/v1.md].
// When this package and the spec disagree, the spec wins and this
// package is the bug.
//
// # Scope of this package (Phase 1.22.A)
//
//   - Envelope serialization + strict parsing (envelope.go).
//   - RFC 8785 JSON canonicalization (canonical.go).
//   - Ed25519 sign + verify primitives (ed25519.go).
//   - Typed catalogues for activity types, object types, trust
//     tiers, encryption policies, share scopes, and inbox / outbox
//     status codes (vocab.go).
//   - NaCl-box multi-recipient encryption envelope (nacl/).
//   - Conformance vectors anyone re-implementing the spec validates
//     against (testdata/conformance/).
//
// # Out of scope here (later sub-phases)
//
// HTTP transport, peer registry + handshake, federation_shares
// access control, inbox + outbox + delivery, custom activity
// handlers (`Approve`, `Annotation`, etc.), CAS fetch endpoint,
// moderation hooks, curated directory, end-to-end encryption
// wiring above the box-primitive layer, auto-sync policies. Each
// lands in its own sub-phase (B through J) per
// [ADR 0043 §"Phase 1.22 sub-phase breakdown"].
//
// # Clean-room methodology
//
// All work in this package is performed under the project's
// clean-room methodology per [ADR 0040]. Contributors are
// registered in [docs/spec/federation/CONTRIBUTORS.md]; approved
// spec sources are catalogued in [docs/spec/federation/SOURCES.md].
// Reading the source of any other ActivityPub implementation
// contaminates the reader and excludes them from further Phase
// 1 / Phase 3 work on this package. See SOURCES.md for the
// approved spec-reading list; everything else (third-party
// server, library, or client implementation) is forbidden.
package federation
