-- 00009_ai_inference_foundation.sql
--
-- Phase 1.14.A — AI provider abstraction + inference foundation.
--
-- # What this migration adds
--
--   1. Two capability seeds — ai.use (default Base) + ai.admin
--      (default Admin) — that gate every AI surface added in 1.14.A.
--   2. ai_provider_call — typed per-call audit + cost tracking table.
--      Every AI call records here (provider, model, prompt template
--      version, input hash, tokens in/out, duration, est cost, status).
--      Source of truth for operator analytics + budget enforcement.
--   3. jobs.idempotency_key column + partial UNIQUE INDEX — re-enqueuing
--      the same (asset, prompt version) work returns the existing job
--      id rather than queueing duplicate inference calls.
--   4. system_config defaults for the AI subsystem: master switch OFF,
--      per-task routing, fallback chains, privacy-locks-sensitive-to-
--      local-providers, $0 default budget (fail-closed; operator must
--      raise explicitly), per-job-type concurrency caps.
--
-- # Audit-first deltas vs the original brief
--
--   - capabilities table has (code, description, created_at,
--     required_license_feature) — NOT deprecated_at. INSERT shape
--     adjusted to match.
--   - system_config table has (key, value, updated_at) only — NO
--     description column. The brief's description argument is dropped
--     from the INSERT VALUES list; the rationale lives in this comment
--     block instead.
--   - FK target is `assets(id)` (plural) — the asset table is plural
--     in our schema (per the 1.9.B audit + asset_field_value FK
--     precedent).
--   - pgvector extension is NOT enabled in this migration (1.14.A
--     doesn't need vectors; 1.14.B does).

-- +goose Up
-- +goose StatementBegin

-- 1. AI capability seeds.
--    ai.use is per-call (any authenticated user with the cap can
--    trigger inference); ai.admin gates configuration + budget
--    management.
INSERT INTO capabilities (code, description) VALUES
    ('ai.use',   'Trigger AI inference on assets (tagging, captioning, etc.)'),
    ('ai.admin', 'Configure AI providers, routing, budgets, and privacy policy')
ON CONFLICT (code) DO NOTHING;

INSERT INTO role_capabilities (role_id, capability_code)
SELECT id, 'ai.use'   FROM roles WHERE name = 'Base'
ON CONFLICT DO NOTHING;

INSERT INTO role_capabilities (role_id, capability_code)
SELECT id, 'ai.admin' FROM roles WHERE name = 'Admin'
ON CONFLICT DO NOTHING;

-- 2. ai_provider_call — typed audit + cost tracking per call.
--
--    estimated_cost_usd_micros is integer math (1_000_000 micros =
--    $1.00) so monthly rollups + budget comparisons don't drift on
--    floating-point. Per-call rows; aggregation lives in the
--    cost.Tracker cache layer for hot reads.
--
--    status enumerates the call outcomes the cost tracker + audit
--    dashboards need to distinguish — `budget_blocked` and
--    `privacy_blocked` are recorded even though no HTTP call went out,
--    so the operator dashboard can surface "5 calls blocked by privacy
--    policy on Team Diablo last week" as an actionable signal.
CREATE TABLE ai_provider_call (
    id                          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    provider                    TEXT NOT NULL,
    model                       TEXT NOT NULL,
    concern                     TEXT NOT NULL
        CHECK (concern IN ('complete', 'embed', 'transcribe', 'tag', 'caption')),
    prompt_template             TEXT,
    prompt_version              TEXT,
    asset_id                    UUID,
    job_id                      UUID REFERENCES jobs(id) ON DELETE SET NULL,
    input_hash                  TEXT,
    input_tokens                INTEGER,
    output_tokens               INTEGER,
    duration_ms                 INTEGER NOT NULL,
    estimated_cost_usd_micros   BIGINT,
    status                      TEXT NOT NULL DEFAULT 'success'
        CHECK (status IN ('success', 'rate_limited', 'transient_error', 'permanent_error', 'budget_blocked', 'privacy_blocked')),
    error_message               TEXT,
    actor_user_ref              BIGINT,
    triggered_at                TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Hot read path: "what did $provider spend this month?"
CREATE INDEX idx_ai_provider_call_billing
    ON ai_provider_call (provider, triggered_at DESC);

-- Per-asset call history for the operator's regenerate UI + provenance lookups.
CREATE INDEX idx_ai_provider_call_asset
    ON ai_provider_call (asset_id, concern, triggered_at DESC)
    WHERE asset_id IS NOT NULL;

-- Job audit: "what calls did $job_id make?" — drives the per-job AI
-- trace surfaced when an operator inspects a failed inference job.
CREATE INDEX idx_ai_provider_call_job
    ON ai_provider_call (job_id)
    WHERE job_id IS NOT NULL;

-- 3. Idempotency on jobs table.
--
--    The partial UNIQUE constraint covers only pending + running rows
--    so a successful (or failed) historical job doesn't block a fresh
--    re-enqueue. Re-enqueuing with the same key while a prior job is
--    in-flight returns that job's id (handler-layer detection on
--    23505 unique violation).
ALTER TABLE jobs
    ADD COLUMN idempotency_key TEXT;

CREATE UNIQUE INDEX uq_jobs_idempotency_key
    ON jobs (type, idempotency_key)
    WHERE idempotency_key IS NOT NULL
      AND status IN ('pending', 'running');

-- 4. AI default config seeds.
--
--    Rationale for each key:
--      ai.enabled                       master switch; fresh installs ship OFF
--                                       so an admin opts in before any cloud
--                                       traffic can fire
--      ai.routing                       per-task default provider; tag→ollama
--                                       (private+free), caption→claude (quality),
--                                       embed→clip_local (deterministic +
--                                       cacheable), transcribe→whisper_local
--                                       (private)
--      ai.fallback_chains               operator-configured walk if primary
--                                       fails; mirrors the routing defaults
--                                       per task with cloud + local fallbacks
--      ai.privacy.lock_sensitive_to_local
--                                       restricted + embargo assets route to
--                                       local providers ONLY; default TRUE so
--                                       the wire never carries sensitive bytes
--                                       to cloud APIs without explicit opt-out
--      ai.privacy.local_providers       which provider names are considered
--                                       local (not cloud) for the gate above
--      ai.budgets.default               $0 hard cap on new providers; first
--                                       cloud call fails-closed until operator
--                                       explicitly raises — no accidental bills
INSERT INTO system_config (key, value) VALUES
    ('ai.enabled',                       'false'::jsonb),
    ('ai.routing',                       '{"tag":"ollama","caption":"claude","embed":"clip_local","transcribe":"whisper_local","complete":"claude"}'::jsonb),
    ('ai.fallback_chains',               '{"complete":["claude","openai","ollama"],"embed":["clip_local","ollama","openai"],"transcribe":["whisper_local","openai"],"tag":["ollama","gemini","openai"],"caption":["claude","openai","ollama"]}'::jsonb),
    ('ai.privacy.lock_sensitive_to_local', 'true'::jsonb),
    ('ai.privacy.local_providers',       '["ollama","vllm","whisper_local","clip_local"]'::jsonb),
    ('ai.budgets.default',               '{"soft_warning_usd":0,"hard_cap_usd":0}'::jsonb)
ON CONFLICT (key) DO NOTHING;

-- 5. Per-job-type concurrency caps for the worker pool.
--    Pool size is global; these caps scope per-type so a flood of
--    ai.embed jobs can't starve ai.tag (or vice versa).
INSERT INTO system_config (key, value) VALUES
    ('jobs.type_concurrency.ai.tag',        '4'::jsonb),
    ('jobs.type_concurrency.ai.caption',    '2'::jsonb),
    ('jobs.type_concurrency.ai.embed',      '8'::jsonb),
    ('jobs.type_concurrency.ai.transcribe', '1'::jsonb)
ON CONFLICT (key) DO NOTHING;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DELETE FROM system_config WHERE key LIKE 'jobs.type_concurrency.ai.%';
DELETE FROM system_config WHERE key LIKE 'ai.%';
DROP INDEX IF EXISTS uq_jobs_idempotency_key;
ALTER TABLE jobs DROP COLUMN IF EXISTS idempotency_key;
DROP TABLE IF EXISTS ai_provider_call;
DELETE FROM role_capabilities WHERE capability_code IN ('ai.use', 'ai.admin');
DELETE FROM capabilities WHERE code IN ('ai.use', 'ai.admin');
-- +goose StatementEnd
