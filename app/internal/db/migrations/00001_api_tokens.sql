-- artist-alley migration 00001 — API tokens table.
--
-- Owned by Go (this table will never appear in RS's dbstruct/). Stores
-- one row per issued Personal Access Token. Tokens themselves are
-- never stored; only sha256(token) is persisted, so a leaked DB
-- snapshot cannot be replayed against the API.

-- +goose Up
CREATE TABLE api_tokens (
    id            UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    rs_user_id    BIGINT       NOT NULL,
    name          TEXT         NOT NULL,
    token_hash    BYTEA        NOT NULL UNIQUE,
    scopes        TEXT[]       NOT NULL DEFAULT '{}',
    expires_at    TIMESTAMPTZ  NULL,
    last_used_at  TIMESTAMPTZ  NULL,
    revoked_at    TIMESTAMPTZ  NULL,
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX api_tokens__user_idx
    ON api_tokens (rs_user_id);

CREATE INDEX api_tokens__active_idx
    ON api_tokens (token_hash)
    WHERE revoked_at IS NULL;

COMMENT ON TABLE  api_tokens IS 'Personal Access Tokens for the artist-alley API.';
COMMENT ON COLUMN api_tokens.rs_user_id    IS 'References RS user.ref. No FK during transition.';
COMMENT ON COLUMN api_tokens.token_hash    IS 'sha256(token) as raw bytes; the token value is never stored.';
COMMENT ON COLUMN api_tokens.scopes        IS 'List of capability codes this token may use. Empty = all caps the user has.';

-- +goose Down
DROP TABLE IF EXISTS api_tokens;
