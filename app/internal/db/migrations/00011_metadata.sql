-- artist-alley migration 00011 — admin-extensible metadata.
--
-- See ADR 0012. Three new tables (field_definition, asset_field_value,
-- asset_field_value_history) plus a generated tsvector column on
-- assets and the triggers that keep it fresh.
--
-- Field values use typed columns (value_text/num/date/options/ref)
-- rather than a single jsonb so we can index per-field-per-type and
-- avoid type coercion at query time.

-- +goose Up

-- ---------------------------------------------------------------------------
-- Schema definition (admin-managed).
-- ---------------------------------------------------------------------------

CREATE TABLE field_definition (
    id                          UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    code                        TEXT         NOT NULL UNIQUE,
    label                       TEXT         NOT NULL,
    description                 TEXT         NOT NULL DEFAULT '',
    type                        TEXT         NOT NULL
                                             CHECK (type IN (
                                                'text','longtext','rich_text',
                                                'number','boolean',
                                                'date','datetime',
                                                'select','multi_select','tree',
                                                'reference')),
    options                     JSONB        NOT NULL DEFAULT '{}'::jsonb,
    required                    BOOLEAN      NOT NULL DEFAULT FALSE,
    searchable                  BOOLEAN      NOT NULL DEFAULT TRUE,
    applies_to                  BIGINT[]     NOT NULL DEFAULT '{}',
    field_set_id                UUID         NULL,

    read_capability             TEXT         NULL,
    write_capability            TEXT         NULL,

    display_order               INTEGER      NOT NULL DEFAULT 100,
    display_group               TEXT         NOT NULL DEFAULT 'general',

    source                      JSONB        NULL,

    status                      TEXT         NOT NULL DEFAULT 'active'
                                             CHECK (status IN ('active','deprecated','archived')),
    deprecated_replacement_id   UUID         NULL REFERENCES field_definition(id),

    origin_server_id            UUID         NULL,

    created_at                  TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at                  TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by_user_ref         BIGINT       NULL,
    updated_by_user_ref         BIGINT       NULL
);

CREATE INDEX field_definition_status_idx
    ON field_definition (status) WHERE status = 'active';
CREATE INDEX field_definition_group_idx
    ON field_definition (display_group, display_order);
CREATE INDEX field_definition_applies_to_gin
    ON field_definition USING gin (applies_to);
CREATE INDEX field_definition_options_gin
    ON field_definition USING gin (options);

COMMENT ON TABLE  field_definition IS
    'Admin-managed metadata schema. Each row defines one field that can be set on assets.';
COMMENT ON COLUMN field_definition.code IS
    'Federation-stable slug. Globally unique within an instance; peers coordinate by adopting the same codes.';
COMMENT ON COLUMN field_definition.options IS
    'Type-dependent constraints/values JSONB. select/multi_select carry their values; number carries min/max; text carries pattern, etc.';

-- ---------------------------------------------------------------------------
-- Field values.
-- ---------------------------------------------------------------------------

CREATE TABLE asset_field_value (
    asset_id        UUID         NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    field_id        UUID         NOT NULL REFERENCES field_definition(id) ON DELETE CASCADE,

    value_text      TEXT             NULL,
    value_num       DOUBLE PRECISION NULL,
    value_date      TIMESTAMPTZ      NULL,
    value_options   TEXT[]       NULL,
    value_ref       UUID         NULL,

    set_by          TEXT         NOT NULL DEFAULT 'manual'
                                 CHECK (set_by IN ('manual','exif','iptc','xmp','api','import','computed')),
    set_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    set_by_user_ref BIGINT       NULL,

    PRIMARY KEY (asset_id, field_id)
);

CREATE INDEX asset_field_value_asset_idx
    ON asset_field_value (asset_id);
CREATE INDEX asset_field_value_field_idx
    ON asset_field_value (field_id);
CREATE INDEX asset_field_value_text_idx
    ON asset_field_value (field_id, value_text)
    WHERE value_text IS NOT NULL;
CREATE INDEX asset_field_value_num_idx
    ON asset_field_value (field_id, value_num)
    WHERE value_num IS NOT NULL;
CREATE INDEX asset_field_value_date_idx
    ON asset_field_value (field_id, value_date)
    WHERE value_date IS NOT NULL;
CREATE INDEX asset_field_value_options_gin
    ON asset_field_value USING gin (value_options)
    WHERE value_options IS NOT NULL;
CREATE INDEX asset_field_value_ref_idx
    ON asset_field_value (value_ref)
    WHERE value_ref IS NOT NULL;

COMMENT ON TABLE asset_field_value IS
    'One row per (asset, field). Exactly one of the value_* columns is populated, dictated by field_definition.type.';

-- ---------------------------------------------------------------------------
-- Append-only history. No UPDATE/DELETE in normal flow.
-- ---------------------------------------------------------------------------

CREATE TABLE asset_field_value_history (
    id                  UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    asset_id            UUID         NOT NULL,
    field_id            UUID         NOT NULL,
    old_value           JSONB        NULL,
    new_value           JSONB        NULL,
    changed_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    changed_by_user_ref BIGINT       NULL,
    set_by              TEXT         NOT NULL DEFAULT 'manual'
);

CREATE INDEX afvh_asset_idx ON asset_field_value_history (asset_id, changed_at DESC);
CREATE INDEX afvh_field_idx ON asset_field_value_history (field_id, changed_at DESC);

COMMENT ON TABLE asset_field_value_history IS
    'Append-only audit trail of field-value changes. Background sweeper archives rows older than 1 year (separate phase).';

-- ---------------------------------------------------------------------------
-- search_text TSVECTOR on assets + trigger that maintains it.
-- ---------------------------------------------------------------------------
--
-- A single materialised search column on `assets` aggregating every
-- searchable field value. Replaces RS's node_keyword denormalised
-- index — one source of truth, no consistency drift.

ALTER TABLE assets ADD COLUMN search_text TSVECTOR NULL;

CREATE INDEX assets_search_text_gin
    ON assets USING gin (search_text)
    WHERE deleted_at IS NULL;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION rebuild_asset_search_text(p_asset_id UUID) RETURNS VOID AS $$
DECLARE
    new_text TEXT;
BEGIN
    SELECT COALESCE(
        STRING_AGG(
            CASE
                WHEN v.value_text     IS NOT NULL THEN v.value_text
                WHEN v.value_options  IS NOT NULL THEN array_to_string(v.value_options, ' ')
                ELSE NULL
            END,
            ' '
        ),
        ''
    )
    INTO new_text
    FROM asset_field_value v
    JOIN field_definition f ON f.id = v.field_id
    WHERE v.asset_id = p_asset_id
      AND f.searchable = TRUE
      AND f.status = 'active';

    -- Include the asset's own title + description so they're searchable
    -- even before any field values land.
    UPDATE assets
       SET search_text = to_tsvector('english',
                            COALESCE(title, '') || ' ' ||
                            COALESCE(description, '') || ' ' ||
                            COALESCE(new_text, ''))
     WHERE id = p_asset_id;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION asset_field_value_trigger() RETURNS trigger AS $$
DECLARE
    target_asset UUID;
BEGIN
    IF (TG_OP = 'DELETE') THEN
        target_asset := OLD.asset_id;
    ELSE
        target_asset := NEW.asset_id;
    END IF;

    PERFORM rebuild_asset_search_text(target_asset);

    -- Broadcast invalidation so app-instance LRUs can drop their copy.
    PERFORM pg_notify(
        'cache_invalidate',
        json_build_object(
            'domain', 'asset_by_id',
            'key',    target_asset::TEXT,
            'op',     'upsert'
        )::TEXT
    );

    IF (TG_OP = 'DELETE') THEN RETURN OLD; ELSE RETURN NEW; END IF;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER asset_field_value_search_text
AFTER INSERT OR UPDATE OR DELETE ON asset_field_value
FOR EACH ROW EXECUTE FUNCTION asset_field_value_trigger();

-- Also rebuild when the asset's title/description change. Cheaper to
-- do this in code, but the trigger keeps consistency even when PHP
-- writes assets during the transition.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION asset_changed_trigger() RETURNS trigger AS $$
BEGIN
    IF (NEW.title IS DISTINCT FROM OLD.title)
       OR (NEW.description IS DISTINCT FROM OLD.description)
       OR (OLD.search_text IS NULL) THEN
        PERFORM rebuild_asset_search_text(NEW.id);
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER assets_search_text_trigger
AFTER INSERT OR UPDATE ON assets
FOR EACH ROW EXECUTE FUNCTION asset_changed_trigger();

-- ---------------------------------------------------------------------------
-- Default seed fields (ADR 0012).
-- ---------------------------------------------------------------------------

INSERT INTO field_definition (code, label, description, type, required, display_group, display_order, source) VALUES
    ('title',         'Title',         'Primary display title for the asset.',          'text',         TRUE,  'core',      10,
        '{"type":"iptc","tag":"ObjectName"}'::jsonb),
    ('description',   'Description',   'Long-form description of the work.',             'longtext',     FALSE, 'core',      20, NULL),
    ('credit',        'Credit',        'Person or studio credited for the work.',        'text',         FALSE, 'rights',    10,
        '{"type":"iptc","tag":"Credit"}'::jsonb),
    ('copyright',     'Copyright',     'Copyright notice / rights statement.',           'text',         FALSE, 'rights',    20,
        '{"type":"xmp","tag":"dc:rights"}'::jsonb),
    ('capture_date',  'Capture date',  'When the original was captured (EXIF).',          'datetime',     FALSE, 'technical', 10,
        '{"type":"exif","tag":"DateTimeOriginal"}'::jsonb),
    ('keywords',      'Keywords',      'Multi-value tagging.',                             'multi_select', FALSE, 'core',      30,
        '{"type":"iptc","tag":"Keywords"}'::jsonb),
    ('country',       'Country',       'Country / region / city tree.',                    'tree',         FALSE, 'general',   40,
        '{"type":"iptc","tag":"Country-PrimaryLocationName"}'::jsonb)
ON CONFLICT (code) DO NOTHING;

-- +goose Down

DROP TRIGGER IF EXISTS assets_search_text_trigger ON assets;
DROP TRIGGER IF EXISTS asset_field_value_search_text ON asset_field_value;
DROP FUNCTION IF EXISTS asset_changed_trigger();
DROP FUNCTION IF EXISTS asset_field_value_trigger();
DROP FUNCTION IF EXISTS rebuild_asset_search_text(UUID);
ALTER TABLE assets DROP COLUMN IF EXISTS search_text;
DROP TABLE IF EXISTS asset_field_value_history;
DROP TABLE IF EXISTS asset_field_value;
DROP TABLE IF EXISTS field_definition;
