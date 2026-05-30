# ADR 0018: Share links — signed, expiring resource + collection URLs

- Date: 2026-05-30
- Status: Accepted

## Context

Game studios collaborate with people who do not have accounts on their
Artist Alley instance: contractors, freelancers, publishers, IP owners,
QA partners, agencies, festival juries, press embargoes. The post-detail
permission model (capability + team membership) does not solve "send this
post / this collection to a publisher for sign-off, with the link
expiring after 14 days and revoking on a single click."

ResourceSpace ships first-class share links for both resources and
collections, with password gating, expiry, per-link access scope, and
usage tracking. The audit (2026-05-30) flagged share links as the
single most-impactful gap on the Artist Alley roadmap — every studio
hits this within the first week.

## Decision

Add Phase 1.26 — Share links — as a distinct, first-class feature
sitting alongside the federation share-with-anyone path (Phase 1.18.B-14)
which is a *cluster-of-instances* path. Share links are the
*single-instance public-URL* path. The two coexist: share links are the
fast turn-on for external collaboration, federation is the durable
multi-instance topology.

### URL shape

```
GET /share/{token}
```

`{token}` is a URL-safe random 32-byte identifier. The token is NOT
the secret — the share-link record on the server is the secret. We do
NOT sign the URL with HMAC; the database lookup is the source of truth
on validity. This is simpler than signed URLs, supports immediate
revocation, and trivially supports "show me all my active links."

### Scope vocabulary

A share link binds:

- **Target** — a single asset, a single post, or a collection.
- **Scope** — one of `view`, `comment`, `annotate`, `download`. Scopes
  are additive (a `download` link implies `view`).
- **Expiry** — optional `nbf` and `exp` timestamps. Default `exp` is
  `now() + 14d`; no default `nbf`.
- **Password** — optional Argon2id hash. When present, the share page
  shows a password gate before the target.
- **Max uses** — optional integer ceiling on the number of views.
- **Creator** — the user who minted the link; auditable.
- **Audit trail** — every fetch (with IP, user-agent, password
  result, scope satisfied) is logged for the link's lifetime.

### Revocation

Single button in the admin / account UI invalidates the token. Anyone
holding the URL gets a 410 Gone immediately. No grace period, no client
caching — the binary checks the share-link table on every request to
`/share/{token}`.

### Storage backend tie-in

Downloads from a share link go through a short-lived presigned URL on
the storage backend (S3 / R2 / B2) so the binary does not have to
proxy gigabytes. Filesystem backend serves the file directly with the
appropriate ACL headers.

### Federation interaction

Share links live on the issuing instance. If a federated peer surfaces
the underlying asset, the peer does NOT inherit the share link — the
link is bound to the issuing host. This keeps the security model
simple: one revoke button, one host of record.

### Anti-scraping / rate limit

Share-link requests are rate-limited per IP + per token. A burst that
looks like enumeration trips a soft block (CAPTCHA challenge or hard
deny depending on admin config).

## Consequences

**Positive**

- Solves the single most-requested external-collaboration use case
  without forcing accounts.
- Same database mechanism gates resources, posts, and collections —
  one shape, one revocation surface.
- Built-in usage tracking surfaces "this link has been opened 47 times
  by 6 distinct IPs" so studios can see real-world reach.
- Password + expiry + max-uses are all optional, so most links are
  cheap to mint.
- No HMAC signing → no key rotation problem, no signed-URL leak risk.

**Negative**

- DB lookup on every share-link request means caching at the edge is
  harder. Mitigation: cache the *resolution* (target + scope + auth
  needed) in `cache.Registry` with a short TTL and immediate
  invalidation on revoke.
- A share link with `download` scope on a 50 GB video does need
  presigned-URL plumbing per storage backend. Storage interface adds
  one method: `PresignDownload(ctx, key, ttl)`.

## Alternatives considered

- **Signed-URL only (no DB row).** Smaller infra footprint but no
  revocation, no usage tracking, no password gating, no max-use
  ceiling. Rejected — these are all features studios specifically want.
- **Use only federation share-with-anyone (1.18.B-14).** Federation
  requires the recipient run an Artist Alley instance. Game studios
  share with publishers and agencies who will never self-host. The
  single-host share link is what fits the actual use case.
- **JWT-encoded share tokens.** Self-contained validity but again no
  revocation. Rejected.

## Reference

- Phase 1.26 in [`docs/roadmap.md`](../roadmap.md).
- Related: federation share-with-anyone — Phase 1.18.B-14.
- License gating: share-link creation respects per-tier asset-cap and
  active-seat limits via the tangled-derivation model in ADR 0017.
