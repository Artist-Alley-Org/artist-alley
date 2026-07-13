# Catalogue meta-index

Per [ADR 0042](adr/0042-distributed-catalogs-typed-per-package.md), Artist Alley keeps named-constant catalogues in the packages that own them rather than in a central god-file. This page is the **single navigation surface** that tells you which package owns which catalogue.

This is not the catalogue itself. The values live in the listed file; this index just tells you where to look.

## How to read this page

- **Catalogue** — the conceptual set (event types, status codes, etc.).
- **Owner** — the package or file that holds the typed Go constants.
- **DB mirror** — the migration whose `CHECK` constraint pins the same set at the storage layer, or `—` for Go-only catalogues with no database enforcement.
- **Used by** — packages that read from the catalogue (informational; the owner is the source of truth for what's in it).

When you add a new catalogue: add a row here in the appropriate section, in the same PR that introduces it.

## Identity & access

| Catalogue | Owner | DB mirror | Used by |
|---|---|---|---|
| User lifecycle states (`pending`, `approved`, `disabled`, `deleted`) | `app/internal/users/` | `00001_baseline_v0_1.sql` (`users` lifecycle CHECK) | `auth/`, `audit/`, admin UI |
| Capability codes (`users.read`, `system.admin`, `share.create`, …) | seeded into `capabilities` table by migration | `00001_baseline_v0_1.sql` (`capabilities` table + seed section) | every handler that calls `acls.RequireCapability` |
| API-token scopes | `app/internal/auth/` | column on `api_tokens` row | token validation middleware |
| Session source kinds (cookie / api-token / legacy) | `app/internal/auth/` | — (Go-only discriminator) | middleware, audit |

## Assets & metadata

| Catalogue | Owner | DB mirror | Used by |
|---|---|---|---|
| Archive states (`active`, `archived`, `deleted`, …) | `app/internal/assets/` | `00001_baseline_v0_1.sql` (`assets.archive_state` CHECK) | every consumer of `assets.Asset.archive_state` |
| Asset sensitivity tiers (`public` / `team` / `restricted` / `embargo`) | `app/internal/assets/` (planned with ADR 0020) | per-row column when the phase lands | viewer, share-links |
| Metadata field types (`text`, `longtext`, `number`, `boolean`, `date`, `datetime`, `select`, `multi_select`, `tree`, `reference`, …) | `app/internal/metadata/` | `00001_baseline_v0_1.sql` (CHECK on `field_definition.type`) | metadata validation, admin field-editor |
| Field-value provenance (`manual` / `exif` / `iptc` / `xmp` / `api` / `import` / `computed`) | `app/internal/metadata/` | `00001_baseline_v0_1.sql` (CHECK on `asset_field_value.set_by`) | importers, EXIF extraction |

## Jobs & async work

| Catalogue | Owner | DB mirror | Used by |
|---|---|---|---|
| Job types (`preview.image`, `preview.video`, `import.run`, `caption.transcribe`, …) | `app/internal/jobs/` (`JobType` constants) | `00001_baseline_v0_1.sql` (`jobs` table; no enforced enum — types are open) | every package that enqueues work |
| Job statuses (`queued`, `running`, `success`, `partial`, `failed`, `cancelled`) | `app/internal/jobs/` (`Status*` constants) | `00001_baseline_v0_1.sql` (`jobs.status` CHECK) | worker, admin job inspector |
| Job priorities (`PriorityHigh = 50`, `PriorityNormal = 100`, `PriorityLow = 200`, `PriorityBackfill = 500`) | `app/internal/jobs/` | — (Go-only int) | enqueue sites |

## Audit & notifications

| Catalogue | Owner | DB mirror | Used by |
|---|---|---|---|
| Audit event types (`auth.login`, `asset.uploaded`, `share.created`, …) | `app/internal/audit/events.go` (`Recorder` methods + event-name strings) | — (events stored as JSON; type is the discriminator) | every domain that calls `audit.Recorder` |
| Audit categories (`auth.*` / `asset.*` / `share.*` / `commerce.*` / `platform.*` / …) | `app/internal/audit/` | — | admin audit filter UI |
| Notification event types (`comment.on_my_post`, `like.on_my_post`, `resource_request.received`, `license.expiring_soon`, …) | `app/internal/userprefs/prefs.go` (`KnownEventTypes`) | — | user-preference UI, notification router |
| Notification channels (`in_app`, `email`, `chat`, …) | `app/internal/userprefs/` | — | notification routing |

## Workflow & lifecycle

| Catalogue | Owner | DB mirror | Used by |
|---|---|---|---|
| Workflow states (per-deployment configurable) | `app/internal/workflow/` rows in `workflow_states` table — not a Go enum | `00001_baseline_v0_1.sql` (`workflow_states` schema; values are data, not code) | review modal, admin |
| Workflow transition kinds | `app/internal/workflow/` | `00001_baseline_v0_1.sql` (workflow tables) | workflow service |
| Sensitivity-tier transitions (NDA expiry / embargo lift, planned) | `app/internal/scheduledactions/` when the phase lands | — | scheduled-action engine |

## Storage & preview

| Catalogue | Owner | DB mirror | Used by |
|---|---|---|---|
| Storage backend kinds (`fs` / `s3` / `import-ref` planned) | `app/internal/storage/` | — (resolved at process boot from config) | storage abstraction |
| Preview-variant kinds (`thumb`, `preview`, `hires`, `waveform`, `sprite`, `subs/{lang}.vtt`, …) | `app/internal/preview/` | — (rows in `storage_variants` keyed on variant name) | preview pipeline, viewer |
| Asset companion roles | `app/internal/storage/` | `00001_baseline_v0_1.sql` (`asset_companions` table) | storage abstraction, 3D loaders |

## ACLs & sharing

| Catalogue | Owner | DB mirror | Used by |
|---|---|---|---|
| ACL principal kinds (`team` / `role` / `user`) | `app/internal/acls/` | `00001_baseline_v0_1.sql` (ACL tables) | post / collection ACL checks |
| ACL grant levels (`read` / `comment` / `annotate` / `edit` / `admin`) | `app/internal/acls/` | `00001_baseline_v0_1.sql` (ACL tables) | every gated endpoint |
| Share-link scopes (`view` / `comment` / `annotate` / `download`, planned ADR 0018) | `app/internal/sharelinks/` when the phase lands | per-row column | share-link viewer |

## Federation (Phase 1.22)

| Catalogue | Owner | DB mirror | Used by |
|---|---|---|---|
| Activity types (`Create`, `Update`, `Delete`, `Follow`, …, `aa:Share`, `aa:Approve`, `aa:Annotation`, …) | `app/internal/federation/vocab.go` (`ActivityType` constants + `KnownActivityTypes`) | `activities.activity_type` CHECK (`00001_baseline_v0_1.sql`) + `federation_outbox.activity_type` + `federation_inbox.activity_type` (Phase 1.22.D) | envelope parser, inbox dispatch, outbox emitters, activities ledger |
| Activity object kinds (`post` / `comment` / `asset` / `user` / `collection` / `workspace` / `brand_kit` / `message` / `activity`) | `app/internal/activities/activities.go` (`ActivityObjectKind` + `KnownObjectKinds`) | `activities.object_kind` CHECK (`00001_baseline_v0_1.sql`) | activities writer, federation outbox, admin audit UI |
| Object types (`Note`, `Image`, …, `aa:Asset`, `aa:Post`, `aa:Workspace`, `aa:BrandKit`, `aa:Collection`) | `app/internal/federation/vocab.go` (`ObjectType` constants + `KnownObjectTypes`) | — (Go-only at v1; object-shape validation is per-type code) | per-activity handlers |
| Signature algorithms (`Ed25519`) | `app/internal/federation/vocab.go` (`SignatureAlgorithm`) | — | envelope `Sign` + `Verify` |
| Encryption algorithms (`nacl-box`) | `app/internal/federation/vocab.go` (`EncryptionAlgorithm`) | — | NaCl-box envelope |
| Object kinds for share rows (`post`, `collection`, `workspace`, `brand_kit`, `asset`, `user`) | `app/internal/federation/vocab.go` (`ObjectKind`) | `federation_shares.object_kind` CHECK (Phase 1.22.C, not yet shipped) | aa:Share / aa:Unshare authoring + access checks |
| Trust tiers (`connected`, `directory-listed`, `auto-sync`) | `app/internal/federation/vocab.go` (`TrustTier`) | `federation_peers.trust_tier` CHECK (Phase 1.22.B) | peer registry, admin surface |
| Encryption policies (`plaintext`, `e2e-encrypted`) | `app/internal/federation/vocab.go` (`EncryptionPolicy`) | `federation_peers.encryption_policy` CHECK (Phase 1.22.B) | outbox dispatcher (chooses plain vs NaCl-box) |
| Share scopes (`view`, `comment`, `annotate`, `edit`) | `app/internal/federation/vocab.go` (`ShareScope` + `ShareScopeOrdered` + `ShareScope.AtLeast`) | `federation_shares.scope` CHECK (Phase 1.22.C) | inbox access checks, share-action UI |
| Inbox status / rejection reasons (`pending`, `processed`, `invalid_context`, `sig_invalid`, `unshared_object`, …) | `app/internal/federation/vocab.go` (`InboxStatus`) | `federation_inbox.status` CHECK (Phase 1.22.D) | inbox dispatcher, audit emission |
| Outbox dispatch statuses (`queued`, `sent`, `failed`, `cancelled`) | `app/internal/federation/vocab.go` (`OutboxStatus`) | `federation_outbox.status` CHECK (Phase 1.22.D) | delivery worker, admin queue inspector |

## Licensing (Phase 1.17.O)

| Catalogue | Owner | DB mirror | Used by |
|---|---|---|---|
| License tiers (`community` / `pro` / `enterprise`) | `app/internal/licensing/` | embedded in signed `.lic` envelope | every feature gate |
| License feature flags | `app/internal/licensing/features.go` | embedded in `.lic` envelope | feature-gate checks |
| Identity-provider kinds (`oidc` / `saml` / `ldap` / specific vendors) | `app/internal/identityproviders/` (Phase 1.17.P) | per-row column | SSO bootstrap |

## Frontend catalogues

| Catalogue | Owner | DB mirror | Used by |
|---|---|---|---|
| Icon registry (admin icons, viewer toolbar icons) | `web/src/lib/components/AdminIcon.svelte` + sibling registries | — | every UI surface that takes an icon name |
| Theme tokens (M3 palette + font slots) | `web/src/lib/stores/theme.ts` + admin appearance config | persisted in `system_config` rows | every component |
| i18n keys (en.json catalogue) | `web/src/lib/i18n/en.json` (flat keys) | — | every translatable surface |
| Viewer-kind discriminators (image / video / audio / pdf / font / 3d / archive / doc / sprite) | `web/src/lib/components/viewers/registry.ts` | — | universal asset viewer |

## Not catalogues (intentionally excluded)

Per [ADR 0042 §5](adr/0042-distributed-catalogs-typed-per-package.md#5-what-is-not-a-catalogue-and-doesnt-need-this-treatment), the following don't belong here:

- **Per-instance configuration** (SMTP server, site name, AI provider URLs) — lives in `system_config` or environment variables, not in code.
- **Tunable thresholds** (cache sizes, retry counts, rate limits) — literal numbers in config; named only when shared across packages.
- **Identifier values** (user IDs, asset hashes, session tokens) — these are values, not catalogue members.
- **OpenAPI schema enums** — those live in `app/api/openapi.yaml` and the strict-server generator types them automatically.

## When to update this page

- A new catalogue is added → new row in the appropriate section.
- A catalogue moves package → update the Owner column.
- A catalogue is deleted → remove the row.
- A catalogue is extended or shrunk → no change here (the values themselves aren't listed; just the location).

This index is hand-maintained on purpose. Generators are not worth the complexity at this scale.
