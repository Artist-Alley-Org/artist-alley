-- 00004_drop_legacy_session_columns.sql
--
-- Phase 1.17.B — Drop dead PHP-coexistence columns on "user".
--
-- The user.session (varchar(50)) and user.logged_in (integer)
-- columns were dual-written by SessionManager during the PHP
-- coexistence window so legacy include/authenticate.php would
-- see Go-minted sessions. PHP was fully removed in Phase 1.49.B
-- (strangler-fig abandoned per memory project_strangler_fig_
-- abandoned), making both columns dead state. The dual-write
-- comments on SessionManager flagged them as "deleted in a
-- follow-up" — this is that follow-up.
--
-- The canonical session lives at sessions(id uuid PRIMARY KEY)
-- — lookup via token_hash, idle/expiry enforced in code. Nothing
-- else reads the dropped columns.
--
-- # Why now
--
-- Pre-MVP per ADR 0046 + feedback_pre_mvp_everything_is_volatile —
-- no live operators depending on these. Append-only migration
-- convention continues from 00002 / 00003.
--
-- # Why these two columns ride together
--
-- They were always written + cleared as a pair (SetUserSession
-- writes both; ClearUserSession clears both). Splitting the drop
-- across two migrations would leave the system in a half-dropped
-- state for the window in between — not useful when nothing reads
-- either column.

-- +goose Up
-- +goose StatementBegin

ALTER TABLE "user"
    DROP COLUMN session,
    DROP COLUMN logged_in;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Restoring just makes the columns present again — they'll be
-- empty for every existing row. The PHP coexistence pathway is
-- gone for good; this Down is for migration mechanics only.
ALTER TABLE "user"
    ADD COLUMN logged_in integer,
    ADD COLUMN session character varying(50);

-- +goose StatementEnd
