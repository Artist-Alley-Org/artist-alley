# Federation v1 conformance vectors

These fixtures are the contract for cross-implementation
interoperability. Anyone re-implementing
[the federation v1 spec](../../../../../docs/spec/federation/v1.md)
validates their implementation against this directory.

The Go reference implementation in `app/internal/federation/`
loads + checks every fixture via `conformance_test.go`. A change
to a fixture is a wire-format change — it MUST come with a
corresponding spec doc update and a coordinated rollout to every
deployed implementation.

## Directory layout

- `rfc8785/` — Canonicalization conformance pack. Each entry is
  the pair `<name>.input.json` + `<name>.canonical.bin`. The
  canonical byte output of an implementation's canonicalizer
  applied to `input.json` MUST equal `canonical.bin`
  byte-for-byte. Vectors copied verbatim from
  [RFC 8785 Appendix B](https://datatracker.ietf.org/doc/html/rfc8785#appendix-B),
  which is public-domain per IETF Trust Legal Provisions.
- `envelope/` — Activity envelope sign-then-verify round-trip
  vectors. Each fixture is a JSON document describing a known
  actor keypair (hex), an unsigned envelope, the canonical bytes
  the envelope produces, the signed envelope with the expected
  signature value, and the expected verify result.
- `reject/` — Negative-path vectors. Each fixture is an envelope
  that MUST be rejected by the parser, with the documented
  rejection reason (one of the inbox status codes in
  `app/internal/federation/vocab.go`).
- `nacl/` — Multi-recipient NaCl-box envelope vectors. Each
  fixture documents a sender keypair, N recipient keypairs,
  ephemeral keys (for determinism), per-recipient nonces, the
  plaintext, and the expected sealed envelope. Implementations
  encrypt with the supplied ephemeral/nonces and confirm bytes
  match; decrypt with each recipient key and confirm plaintext
  matches.

- `members/`: Envelope member vectors (v1.md §9.6, #1580). Each
  fixture is an envelope with the profile tokens the sender
  `negotiated`, the activity types the receiver `applied`, and the
  `expected_reason` from §12.1 (empty: accepted). They cover undefined
  members (`unknown_field`), malformed or repeated ones
  (`invalid_member`), a `Create` that is not a comment
  (`unsupported_activity`), unnegotiated profile members and types
  (`profile_not_negotiated`) and an unsupported `@context`.

- `archival-1/`: Profile archival-1 vectors (v1.md §16-§17, §9.5;
  #1513). Each fixture is an otherwise valid v1 envelope carrying an
  archival-1 member, plus `stage` (`schema`, `negotiation`, `fixity`
  or `round-trip`), `negotiated`, `expected` (`outcome`, `reason`) and
  `enforcement`: `now` names the check `TestConformance_Archival1`
  runs today, `pending` names the open issue that enforces the rule in
  a running receiver. A passing vector proves the rule is executable,
  not that the reference implementation enforces it (it does not yet;
  v1.md *Implementation status* S1). Fixity vectors carry the received
  bytes inline (`received.bytes_b64`); keep them small.

## Adding a new fixture

1. The fixture lives under one of the five categories above.
2. If the fixture introduces new wire-format behaviour, the
   spec doc gets a §-cross-reference comment in the fixture's
   JSON `description` field.
3. `conformance_test.go` automatically picks up new files via
   directory walk — no test-code change needed for additions
   within an existing category.
4. The PR introducing the fixture MUST be reviewed against the
   spec doc to confirm the fixture is correct (not just
   round-trippable by the current implementation).
