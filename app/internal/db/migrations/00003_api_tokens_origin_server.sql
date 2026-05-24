-- artist-alley migration 00003 — federation prep for api_tokens.
--
-- Adds origin_server_id to api_tokens so a future federation layer can
-- record which peer minted a token. NULL means "this server" — the
-- existing behaviour for every row created so far.
--
-- See ADR 0007 (federation: thinking ahead) for context.

-- +goose Up

ALTER TABLE api_tokens
    ADD COLUMN origin_server_id UUID NULL;

COMMENT ON COLUMN api_tokens.origin_server_id IS
    'Federation: id of the artist-alley install that minted this token. NULL = this server. See ADR 0007.';

-- +goose Down

ALTER TABLE api_tokens
    DROP COLUMN origin_server_id;
