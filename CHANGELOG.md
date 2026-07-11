# Changelog

All notable user-facing + wire-format changes to artist-alley.
Format loosely follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
versions track the ArchivePub federation spec ([docs/protocol/archivepub.md](docs/protocol/archivepub.md))
where applicable, otherwise note "no-spec-impact."

## [Unreleased] — Encryption arc (Phase 1.22.I) complete

The full encrypted-federation arc (1.22.I-a through 1.22.I-i) is
shipped + dogfood-validated end-to-end. ArchivePub spec at
**v1.0-rc1** with Appendix A conformance test vectors locked.
Seven-day soak window through **2026-06-22**; v1.0 final ships
as a no-code spec-only commit if soak is clean (otherwise
v1.0-rc2 first).

### Operator-facing changes

- **New** `POST /account/security/rotate-federation-keys` —
  user self-rotation of the X25519 federation keypair. Previous
  key is retained for the configured grace window (default 30
  days) so in-flight envelopes still decrypt.
- **New** `POST /admin/federation/users/{ref}/rotate-keys` —
  operator-initiated rotation for compromised-key recovery.
  `rotated_by_user_ref` records the admin's `user.ref` so the
  audit feed distinguishes recovery from self-rotation.
- **New** `GET /admin/federation/key-health` — aggregate
  observability dashboard data: users without a keypair, remote
  actors missing encryption keys, peers without negotiated
  capabilities, retained keys near expiry. Drill-down rows for
  the first + last categories ride along.
- **Behavior** Federation activities for `restricted`-tier
  assets are now encrypted end-to-end via NaCl-box. Senders
  refuse to dispatch when the recipient peer hasn't negotiated
  the `nacl-box` capability OR the recipient's pubkey isn't
  cached locally.
- **Behavior** Receivers reject plaintext envelopes targeting
  `restricted`-tier assets with `reject_reason=encryption_required`
  + audit `federation.inbox.encryption_required_rejected`.
- **Behavior** Asset sensitivity is set at create time (default
  `public`) and consulted by both sender + receiver gates.
  Changing the tier post-create propagates to in-flight
  emissions automatically (intentional: simpler than copy-at-
  grant semantics; a follow-up phase can layer the alternate
  behavior on top if operator feedback demands).

### Wire-format additions

- `aa:encryptionPublicKey` block in actor profile JSON (v0.3).
- `supported_capabilities` field in peer handshake offer /
  confirm envelopes (v0.4).
- `encryption` block in envelope JSON — per-recipient NaCl-box
  ciphertext + sender/recipient key id+version + nonce (v0.5).
- New reject reasons: `decrypt_failed` (v0.6),
  `encryption_required` (active at v1.0-rc1).

### New conformance test vectors

Appendix A of the spec now lists the 8 active scenarios under
`scripts/dogfood/scenarios/` that any conformant ArchivePub
implementation MUST pass against a peer running the reference:

- `01-like-cross-instance` — wire signature + dispatch
- `05-restricted-asset-roundtrip` — receiver-side defense gate
- `06-wire-dispatch` — outbox dispatcher + sub-1s p99
- `07-encryption-key-distribution` — actor profile + remote-actor cache
- `08-capability-negotiation` — handshake intersection
- `09-outbox-encryption-sender-side` — NaCl-box envelope shape
- `11-refusal-flip` — sensitivity-driven sender refusal
- `12-rotation-lifecycle` — rotation + sweeper + admin observability

Scenarios 02, 03, 04 remain outline scripts pending product
wiring (collection share UI, cascade observability).

### Migrations

| # | Schema change | Phase |
|---|---|---|
| 00007 | `federation_user_keys` table — X25519 keypair storage with `is_current` partial unique + multi-version retention | 1.22.I-b |
| 00008 | `federation_remote_actors.encryption_public_key` columns | 1.22.I-c |
| 00009 | `federation_peers.capabilities` + `capabilities_negotiated_at` | 1.22.I-d |
| 00010 | `federation_outbox.was_encrypted` + sender/recipient key version observability | 1.22.I-e |
| 00011 | `federation_inbox.was_encrypted` + `decrypted_with_key_version` | 1.22.I-f |
| 00012 | `federation_outbox.refused_reason` + `status='refused'` admission | 1.22.I-g |
| 00013 | `federation_user_keys.rotated_at` + `rotated_by_user_ref` + `system_config.federation.user_keys.retained_until_days` | 1.22.I-h |
| 00014 | `assets.sensitivity` (tier vocabulary + partial index on restricted/embargo) | 1.22.I-i |

### Backend admin observability

- 3 new audit events: `federation.user.key_rotated`,
  `federation.user.key_retained_expired`,
  `federation.inbox.encryption_required_rejected`.
- Background `userkeys.Sweeper` goroutine — ticks every hour
  with a boot-time first sweep covering downtime expirations;
  emits one audit per non-zero reap (quiet steady state).
- Receiver-side dispatcher stage-3.5 — gates plaintext envelopes
  against the target object's sensitivity tier via the
  `SensitivityLookup` callback (currently resolves `asset`-kind
  objects; other kinds pass through pending their own
  sensitivity columns).

### Out of scope / deferred

- Per-peer policy overrides ("always encrypt to peer X")
- Cross-instance key revocation broadcasts
- Hardware-token / HSM integration
- Algorithm migration mechanics (X25519 → P256 / PQ)
- `federation_shares.sensitivity` copy-at-grant semantics
  (asset-axis sensitivity is the single source of truth at v1.0-rc1)
