-- artist-alley migration 00009 — system_config key/value store.
--
-- One table to hold runtime-tunable settings the admin manages via the
-- UI: site name, base URL, SMTP credentials, default behaviours, etc.
-- Boot-time concerns (DB connection, scramble key, HTTP listener)
-- stay in env vars — those have to be set before the app can serve
-- the page that would configure them.
--
-- The value column is JSONB so each key can carry whatever shape its
-- domain needs (e.g. smtp is an object with host/port/encryption/auth,
-- site is an object with name/base_url, etc.). Code in
-- internal/sysconfig defines the typed Go structs and the per-key
-- schema.
--
-- Sensitive values (notably SMTP password) live here in plaintext for
-- now. The DB is already the trust boundary — it holds password
-- hashes and session token hashes. A future phase can wrap secrets in
-- envelope encryption keyed off AA_SCRAMBLE_KEY (or a separate
-- AA_DATA_ENCRYPTION_KEY) if/when the threat model warrants it.

-- +goose Up

CREATE TABLE system_config (
    key        TEXT         PRIMARY KEY,
    value      JSONB        NOT NULL DEFAULT '{}'::jsonb,
    updated_at TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE  system_config IS
    'Runtime-tunable settings written by admins (site, smtp, etc.). Boot-time settings stay in env vars.';
COMMENT ON COLUMN system_config.key IS
    'Dotted-namespace key, e.g. site, smtp, signup.policy. See internal/sysconfig for the typed value schema per key.';

-- +goose Down

DROP TABLE IF EXISTS system_config;
