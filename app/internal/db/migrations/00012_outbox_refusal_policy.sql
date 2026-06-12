-- artist-alley migration 00012 — federation_outbox sender-refusal
-- policy state for Phase 1.22.I-g.
--
-- Two columns + one CHECK expansion. The dispatcher Worker's
-- stage where it formerly soft-failed encryption to plaintext now
-- consults [outbox.ChoosePathFor] per row and may refuse delivery
-- entirely when the sensitivity tier mandates encryption + the
-- recipient peer can't honor it.
--
-- # Why sensitivity lives on the outbox row (denormalized)
--
-- The delivery Worker needs the share sensitivity at decision
-- time so it can call [outbox.ChoosePathFor]. The activities
-- table carries the source of truth (via the per-domain
-- resolveSensitivity lookup in the resolver dispatcher); copying
-- it onto the outbox row at INSERT time costs ~8 bytes per row
-- and saves a per-delivery JOIN against the activities table
-- (which is the hot path — the dispatcher's tx-bound activity-
-- ledger scan already touched activities; the Worker shouldn't
-- re-touch it just to look up one column).
--
-- NULL means "pre-I-g row" — the Worker treats those as the
-- conservative SensitivityPublic default per the existing
-- 1.22.D resolver fallback. After all queued rows from prior
-- versions drain through, in-flight rows always have sensitivity
-- populated.
--
-- # Why refused_reason is text NULL, not an enum
--
-- The catalogue today is one value (encryption_required_but_
-- unavailable). Future reasons may surface (e.g.,
-- key_unfetchable_after_retry, peer_refused_capability) without
-- a schema migration. The audit log's metadata->>'reason' uses
-- the same vocabulary string for grep-friendly observability.
--
-- # Why 'refused' is a status value, not a separate column
--
-- Refusal is terminal — the row never gets retried. Modelling it
-- as a status keeps the existing partial-index on status='queued'
-- doing the right thing automatically (refused rows aren't picked
-- up by ListDueOutbox). 'failed' would also work shape-wise but
-- conflates "we tried + the recipient rejected" with "we never
-- tried because policy said no" — distinct operator-side
-- diagnoses warrant distinct states.
--
-- # Design references
--
--   * ADR 0049 §Track B — Decision 5 ("Sender refusal flip"); ADR
--     0020 §"Sensitivity tier governs encryption requirements".
--   * Phase 1.22.I-e shipped the sender-side encrypt + the
--     federation_outbox.was_encrypted column (migration 00010);
--     I-g adds the refusal complement on the same table.

-- +goose Up

ALTER TABLE federation_outbox
    ADD COLUMN sensitivity     text NULL,
    ADD COLUMN refused_reason  text NULL;

-- Drop + recreate the status CHECK so 'refused' is admitted.
ALTER TABLE federation_outbox
    DROP CONSTRAINT federation_outbox_status_check;

ALTER TABLE federation_outbox
    ADD CONSTRAINT federation_outbox_status_check CHECK (
        status IN ('queued', 'sent', 'failed', 'cancelled', 'refused')
    );

COMMENT ON COLUMN federation_outbox.sensitivity IS
    'Phase 1.22.I-g: denormalized share sensitivity (public/team/'
    'restricted/embargo) carried from the activities ledger at '
    'INSERT time so the delivery Worker can consult '
    'outbox.ChoosePathFor without a per-row JOIN. NULL on pre-I-g '
    'rows — Worker treats absence as conservative-public.';

COMMENT ON COLUMN federation_outbox.refused_reason IS
    'Phase 1.22.I-g: catalogue string explaining why the Worker '
    'refused to dispatch this row. Populated alongside '
    'status=''refused''. Today: encryption_required_but_unavailable; '
    'future reasons may land without a schema change.';

-- +goose Down

ALTER TABLE federation_outbox
    DROP CONSTRAINT federation_outbox_status_check;

ALTER TABLE federation_outbox
    ADD CONSTRAINT federation_outbox_status_check CHECK (
        status IN ('queued', 'sent', 'failed', 'cancelled')
    );

ALTER TABLE federation_outbox
    DROP COLUMN refused_reason,
    DROP COLUMN sensitivity;
