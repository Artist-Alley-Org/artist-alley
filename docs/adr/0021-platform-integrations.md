# ADR 0021: External platform integrations — Vimeo, YouTube, Adobe CC, bidirectional

- Date: 2026-05-30
- Status: Accepted

## Context

Earlier framing dismissed platform integrations as "platform lock-in"
and proposed federation as the substitute. That framing was wrong for
the actual use case. Studios need to:

- **Publish** approved videos to Vimeo (private review with publisher),
  YouTube (public reveal), Adobe Creative Cloud Libraries (designers
  using existing Adobe-suite assets in production), Falcon.io
  (marketing scheduling).
- **Pull** content from Adobe CC and similar (designers maintain
  Libraries that artists in Artist Alley need to reference), and
  optionally treat hosted videos as first-class Artist Alley assets
  for review (a Vimeo cut becomes the source-of-truth video that
  annotations attach to).

Federation is the right pattern for *Artist-Alley-to-Artist-Alley*
content flow. It does NOT replace integration with platforms whose
audiences the studio specifically wants to reach.

## Decision

Add Phase 1.29 — External platform integrations — as a provider-
abstraction layer over outbound publishing and inbound asset ingestion.

### Provider model

A `platforms.Provider` interface in Go:

```go
type Provider interface {
    Name() string                                   // "vimeo", "youtube", "adobe-cc", "falcon"
    AuthFlow() AuthFlow                             // OAuth 2.0 dance specifics
    Capabilities() Capabilities                     // {Publish, Pull, Sync, Embed}
    Publish(ctx, asset, settings) (RemoteRef, error)
    Pull(ctx, remoteRef) (asset, error)
    Sync(ctx, remoteRef, asset) (Action, error)     // for two-way mirroring
    Embed(ctx, remoteRef) (HTML, error)             // for in-AA viewer
    Webhook(ctx, payload) ([]Event, error)          // upstream webhook → AA events
}
```

Each provider lives in `app/internal/platforms/<name>/`. Adding YouTube
is dropping a new package + implementing the interface + wiring an
OAuth client ID config.

### Initial providers

| Provider | Publish | Pull | Sync | Webhook |
|---|---|---|---|---|
| Vimeo | ✓ | ✓ | ✓ | ✓ |
| YouTube | ✓ | ✓ | — | ✓ |
| Adobe CC Libraries | ✓ | ✓ | ✓ | ✓ |
| Falcon.io | ✓ | — | — | — |

Two-way sync is delicate; only enabled where the upstream API
genuinely supports change notifications (Vimeo + Adobe CC).

### Linked-asset model

When an asset is published to Vimeo, AA stores the remote URL + a
sync state on the asset row:

```jsonc
{
  "platform_links": [
    {
      "provider": "vimeo",
      "remote_id": "987654321",
      "remote_url": "https://vimeo.com/987654321",
      "published_at": "2026-05-30T12:00:00Z",
      "published_by": 42,
      "sync_state": "in-sync|drifted|deleted-remotely",
      "last_seen": "2026-05-31T03:00:00Z"
    }
  ]
}
```

The badge surfaces in the post-detail modal: "Published to Vimeo (in
sync)." If remote is deleted or drifts, the badge flags it and the
operator can re-publish.

### Pull-as-asset

A "Connect Vimeo / Adobe CC" admin surface authorizes via OAuth and
exposes inbound items as if they were local assets — but with the
`origin_server_id` analogue (`origin_platform_id`) set so they're
clearly marked as external. Federation and external platforms use the
same `origin_*` mechanism — the storage layer doesn't care which.

### Auth secrets

OAuth client secrets stored in Cloudflare Worker Secrets — the same
infrastructure ADR 0017 uses for the licensing private key. Per-studio
OAuth tokens (refresh + access) stored encrypted at rest in the
local DB using the existing secret-encryption helper.

### Federation interaction

Federation (Phase 1.22) is for AA↔AA links. Platform integrations are
for AA↔third-party. The two systems use the same `origin_*` flag on
the resource row so a unified "show me everything from external
sources" filter works without provider branching.

### Per-tier behaviour

- **Community:** 1 active platform connection (any provider).
- **Pro:** 5 active platform connections (any combination).
- **Enterprise:** unlimited connections + organization-level OAuth
  apps (for studios that want their own Vimeo / Google Cloud client,
  not the artist-alley.org-issued one).

Enforced via the tangled-derivation model from ADR 0017 (a
`platformConnectionCap` derived value).

## Consequences

**Positive**

- Studios get a single tool that hosts internal review AND publishes
  to the platforms they actually need to reach.
- Provider abstraction means adding TikTok / Instagram / X is a
  drop-in package, not a re-architecture.
- Linked-asset model preserves the post-detail thread / annotations
  even when the published video lives on Vimeo.

**Negative**

- Each provider's API changes break us. Mitigation: provider packages
  are isolated; a Vimeo break does not block YouTube. Versioned API
  contracts per provider.
- OAuth credential rotation is per-provider, per-studio. Cron job to
  refresh tokens before expiry.

## Alternatives considered

- **Skip outbound publishing; rely on federation.** Doesn't reach the
  publishers / marketing audiences who live on Vimeo / YouTube.
  Rejected per user override 2026-05-30.
- **One opinionated provider (Vimeo only).** Insufficient.
- **Pure webhook-out (no SDK integration).** Works for notification
  but not for content publishing or two-way sync.

## Reference

- Phase 1.29 in [`docs/roadmap.md`](../roadmap.md).
- Federation (Phase 1.22) uses the same `origin_*` mechanism on the
  resource row.
- Chat integrations (Phase 1.30, ADR 0022) use a sibling provider
  abstraction for notification-class providers.
