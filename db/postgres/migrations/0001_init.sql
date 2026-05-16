CREATE EXTENSION IF NOT EXISTS "pgcrypto";
CREATE EXTENSION IF NOT EXISTS "pg_trgm";

CREATE TABLE source (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT NOT NULL UNIQUE,
    uri         TEXT,
    version     TEXT,
    ingested_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    checksum    TEXT
);

CREATE TABLE schema_term (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    kind        TEXT NOT NULL CHECK (kind IN ('entity_type', 'predicate', 'attribute')),
    name        TEXT NOT NULL,
    label       TEXT,
    description TEXT,
    UNIQUE (kind, name)
);

CREATE TABLE entity (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    external_id     TEXT,
    type            TEXT NOT NULL,
    canonical_label TEXT NOT NULL,
    attrs           JSONB NOT NULL DEFAULT '{}'::jsonb,
    source_id       UUID REFERENCES source(id),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    version         INT NOT NULL DEFAULT 1,
    UNIQUE (source_id, external_id)
);

CREATE INDEX entity_type_idx        ON entity (type);
CREATE INDEX entity_label_trgm_idx  ON entity USING gin (canonical_label gin_trgm_ops);
CREATE INDEX entity_attrs_idx       ON entity USING gin (attrs);

CREATE TABLE entity_label (
    entity_id   UUID NOT NULL REFERENCES entity(id) ON DELETE CASCADE,
    label       TEXT NOT NULL,
    label_type  TEXT NOT NULL DEFAULT 'synonym',
    lang        TEXT NOT NULL DEFAULT 'en',
    PRIMARY KEY (entity_id, label, label_type, lang)
);

CREATE INDEX entity_label_label_trgm_idx ON entity_label USING gin (label gin_trgm_ops);

CREATE TABLE relation (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    src_entity_id   UUID NOT NULL REFERENCES entity(id) ON DELETE CASCADE,
    dst_entity_id   UUID NOT NULL REFERENCES entity(id) ON DELETE CASCADE,
    predicate       TEXT NOT NULL,
    attrs           JSONB NOT NULL DEFAULT '{}'::jsonb,
    valid_from      TIMESTAMPTZ,
    valid_to        TIMESTAMPTZ,
    source_id       UUID REFERENCES source(id),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX relation_src_idx       ON relation (src_entity_id, predicate);
CREATE INDEX relation_dst_idx       ON relation (dst_entity_id, predicate);
CREATE INDEX relation_predicate_idx ON relation (predicate);

CREATE TABLE projection_log (
    id              BIGSERIAL PRIMARY KEY,
    target          TEXT NOT NULL,
    started_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at     TIMESTAMPTZ,
    entities_count  BIGINT,
    relations_count BIGINT,
    status          TEXT NOT NULL DEFAULT 'running',
    notes           TEXT
);
