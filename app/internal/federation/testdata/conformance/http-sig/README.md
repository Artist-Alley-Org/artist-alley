# HTTP-Signatures conformance vectors

Pins the canonical signing-string format for the Cavage-style
HTTP-Signatures profile used at the inbox endpoint per
`docs/spec/federation/v1.md` §10 + the 1.22.D design proposal
§5.5 Q5 lock-in (hs2019 + ed25519 only, hand-rolled, no
GPL-deps).

Each vector is the pair:
- `<name>.input.json` — the inbound request shape
  (`method`, `path`, `headers`, `signedHeaders`)
- `<name>.canonical.txt` — the exact byte-for-byte canonical
  signing-string a verifier MUST produce when applying
  `BuildSigningString` to the input

`canonical.txt` files have NO trailing newline. The signing
string per Cavage §2.3 is `headers.join("\n")` exactly.

## Vectors

- `inbox_post.input.json` / `inbox_post.canonical.txt`:
  minimal `POST /federation/inbox` shape with the
  spec-required signed-header set.

A new vector lands when the spec adds a required signed
header (e.g. content-type) or changes the
request-target serialization. Coordinated wire-format change
per `../README.md`'s policy.
