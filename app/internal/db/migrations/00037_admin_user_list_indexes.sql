-- artist-alley migration 00037 — admin user list indexes.
--
-- Phase 1.17.A — supports `GET /admin/users` cursor pagination +
-- case-insensitive search across username / fullname / email.
--
-- The cursor walks (created DESC, ref DESC); the index supports
-- both that order and the natural "users created this week" range
-- queries. The lower(*) expression indexes back the LIKE filter
-- so a 100k-user instance still answers admin search in <100ms.

-- +goose Up
-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS user_created_ref_desc_idx
    ON "user" (created DESC NULLS LAST, ref DESC);

CREATE INDEX IF NOT EXISTS user_username_lower_idx
    ON "user" (LOWER(username));

CREATE INDEX IF NOT EXISTS user_fullname_lower_idx
    ON "user" (LOWER(fullname));

CREATE INDEX IF NOT EXISTS user_email_lower_idx
    ON "user" (LOWER(email));

CREATE INDEX IF NOT EXISTS user_approved_idx
    ON "user" (approved);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS user_approved_idx;
DROP INDEX IF EXISTS user_email_lower_idx;
DROP INDEX IF EXISTS user_fullname_lower_idx;
DROP INDEX IF EXISTS user_username_lower_idx;
DROP INDEX IF EXISTS user_created_ref_desc_idx;
-- +goose StatementEnd
