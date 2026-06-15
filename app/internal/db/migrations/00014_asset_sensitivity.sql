-- artist-alley migration 00014 — assets.sensitivity.
--
-- Adds the intrinsic-sensitivity tier to local assets so the
-- federation gates (sender-side refusal from 1.22.I-g, receiver-
-- side defense from 1.22.I-h) can resolve "is this object's
-- sensitivity tier such that encryption is required?" at
-- dispatch time.
--
-- Phase 1.22.I-i.
--
-- # Why on assets, not (only) on federation_shares
--
-- The brief originally sketched copying sensitivity onto the
-- share row at grant time (rationale: "changing the asset's
-- sensitivity later doesn't retroactively affect outstanding
-- shares"). Pragmatic deviation for I-i: keep sensitivity
-- on the intrinsic-object axis only — the receiver-side gate
-- looks up the asset's current tier at dispatch time, so a
-- post-hoc operator tier change DOES retroactively affect
-- shares.
--
-- Tradeoff: the brief's design respects operator-explicit
-- decisions made at grant time; this design respects "the
-- asset's current classification IS its classification." Both
-- are defensible; the simpler one ships first. A follow-up
-- phase can add federation_shares.sensitivity + copy-at-grant
-- if operator feedback says otherwise.
--
-- # Why NOT NULL DEFAULT 'public'
--
-- Existing assets predate the federation arc; they were
-- effectively public per the pre-I-g plaintext-everywhere
-- behavior. Backfilling NULL would require either a special
-- "sensitivity not set" gate path or implicit-public semantics
-- somewhere — both leak. Making the default 'public' explicit
-- in the column declaration keeps the read paths simple.
--
-- # Why a CHECK constraint, not an enum type
--
-- Postgres enums are awkward to extend (every ALTER TYPE in a
-- transaction warning is a footgun) + sqlc treats them as a
-- generated Go type that's painful to use across packages.
-- The CHECK lets us add a future tier with a simple ALTER
-- TABLE; closed enumeration enforced at the DB level matches
-- the federation_outbox.sensitivity precedent (migration
-- 00012's status column shape).
--
-- # Why a partial index, not a full one
--
-- Admin observability ("show me all restricted + embargo
-- assets") is the primary read pattern. The vast majority of
-- assets are public/team; an index covering only the high-
-- sensitivity tiers stays tiny + the per-tier admin filter
-- query hits it directly. A full-table index would 4-5x the
-- on-disk size for queries that almost never run.
--
-- # Design references
--
--   * ADR 0049 §Track B Decision 6 + ArchivePub spec §3.6
--   * Phase 1.22.I-g shipped the sender-side refusal that
--     CONSUMES sensitivity; this column is the local
--     producer of the tier on the asset axis.

-- +goose Up
-- +goose StatementBegin

ALTER TABLE assets
    ADD COLUMN sensitivity TEXT NOT NULL DEFAULT 'public'
        CHECK (sensitivity IN ('public', 'team', 'restricted', 'embargo'));

COMMENT ON COLUMN assets.sensitivity IS
    'Intrinsic sensitivity tier (public / team / restricted / '
    'embargo). Consumed by the federation outbox sender-refusal '
    'gate (1.22.I-g) + the inbox receiver-defense gate (1.22.I-h '
    'activated at I-i) when activities target this asset. '
    'Default ''public'' matches the pre-arc plaintext-everywhere '
    'behavior; operator-explicit upgrades are the load-bearing '
    'flow.';

CREATE INDEX idx_assets_sensitivity_restricted
    ON assets (sensitivity)
    WHERE sensitivity IN ('restricted', 'embargo');

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_assets_sensitivity_restricted;
ALTER TABLE assets DROP COLUMN sensitivity;

-- +goose StatementEnd
