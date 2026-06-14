-- artist-alley migration 00013 — federation_user_keys rotation
-- metadata for Phase 1.22.I-h.
--
-- Two columns + one system_config default. The rotation primitive
-- (app/internal/federation/userkeys/rotation.go) records WHEN the
-- previous current key was flipped aside + WHO triggered the flip
-- (self-rotation vs admin-initiated recovery). The sweeper reaps
-- expired retained keys; its retention window defaults to the
-- system_config row this migration inserts.
--
-- # Why two new columns, not one
--
-- rotated_at answers "when was the most recent rotation?" — the
-- admin observability surface (/admin/federation/key-health) lists
-- recent rotations sorted by this column. rotated_by_user_ref
-- answers "who triggered it?" — distinguishes the user's own
-- /account/security/rotate action from an admin's compromised-key
-- recovery via /admin/federation/users/{ref}/rotate-keys.
--
-- Both columns are nullable: rotation is post-I-h behavior, and
-- pre-I-h keys (every row that exists today) have neither value.
-- A non-NULL rotated_at means "this row was either inserted by
-- the rotation primitive OR the previous current key that was
-- flipped aside by one." Read paths interpret NULL as "rotation
-- metadata unknown — treat as legacy."
--
-- # Why rotated_by_user_ref is a separate FK to "user"(ref)
--
-- The audit feed already records both userIDs on the rotation
-- event (subject = the key owner, actor = whoever triggered the
-- rotation). Keeping rotated_by_user_ref on the row too lets the
-- admin "recent rotations" list show "rotated by admin Y on
-- behalf of user X" without joining audit_events at render time.
-- Cheap denormalisation; the alternative is a 30-row LEFT JOIN
-- per page render against a much hotter table.
--
-- ON DELETE SET NULL preserves the historical fact that a
-- rotation happened even if the operator who triggered it later
-- gets deleted; the row still describes the key.
--
-- # Why the system_config insert uses ON CONFLICT DO NOTHING
--
-- Idempotent migration replay safety. If an operator hand-set the
-- retention window before this migration ran (rare but possible),
-- we don't clobber their setting; the DO NOTHING leaves the
-- existing value alone. New installs land the 30-day default.
--
-- # Why a string-encoded integer in the JSONB value
--
-- system_config.value is JSONB; the sysconfig package's read path
-- unmarshals into a typed struct per key, but this single-value
-- key doesn't justify a struct + handler wrapper. Storing the
-- duration as the JSONB number 30 (not a string) keeps the
-- in-Go decode path trivial: `var days int; json.Unmarshal(val, &days)`.
--
-- # Design references
--
--   * ADR 0049 §Track B Decision 6 — rotation primitive lives in
--     the userkeys package; sweeper is a separate goroutine that
--     starts with the outbox + inbox dispatchers.
--   * Phase 1.22.I-b shipped the federation_user_keys table
--     (migration 00007); this migration extends it with the
--     rotation metadata that was deferred to I-h.

-- +goose Up
-- +goose StatementBegin

ALTER TABLE federation_user_keys
    ADD COLUMN rotated_at          TIMESTAMPTZ NULL,
    ADD COLUMN rotated_by_user_ref BIGINT      NULL
        REFERENCES "user"(ref) ON DELETE SET NULL;

COMMENT ON COLUMN federation_user_keys.rotated_at IS
    'When the rotation that produced (or flipped aside) this row '
    'occurred. NULL on pre-I-h rows. Non-NULL on the new current '
    'row AND on the previously-current row that was demoted in '
    'the same rotation.';

COMMENT ON COLUMN federation_user_keys.rotated_by_user_ref IS
    'user.ref of whoever triggered the rotation. Equals user_id '
    'for self-rotation (/account/security); differs for admin-'
    'initiated compromised-key recovery (/admin/federation/users/'
    '{ref}/rotate-keys). NULL on pre-I-h rows.';

INSERT INTO system_config (key, value)
VALUES (
    'federation.user_keys.retained_until_days',
    to_jsonb(30)
) ON CONFLICT (key) DO NOTHING;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DELETE FROM system_config
 WHERE key = 'federation.user_keys.retained_until_days';

ALTER TABLE federation_user_keys
    DROP COLUMN rotated_by_user_ref,
    DROP COLUMN rotated_at;

-- +goose StatementEnd
