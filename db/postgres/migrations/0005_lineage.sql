-- Entity-level lineage: track ETL pipeline runs that produced/modified data.
--
-- Combined with existing tables, this gives us a complete lineage view for
-- any entity:
--   - entity.source_id        -> which dataset (catalog level)
--   - pipeline_run            -> which ETL run (job level, NEW)
--   - change_event            -> every row-level mutation (already exists)
--   - action_invocation       -> every write-back action (already exists)
--   - entity_state            -> current action-driven state (already exists)

CREATE TABLE pipeline_run (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name         TEXT NOT NULL,                  -- 'prescriber.load' | 'prescriber.project' | etc.
    source_id    UUID REFERENCES source(id) ON DELETE SET NULL,
    started_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at  TIMESTAMPTZ,
    inputs       JSONB NOT NULL DEFAULT '{}'::jsonb,   -- {year, state, csv_path, ...}
    outputs      JSONB NOT NULL DEFAULT '{}'::jsonb,   -- {Prescriber: 110430, prescribed: 2457348, ...}
    status       TEXT NOT NULL DEFAULT 'running',      -- running | succeeded | failed
    error_msg    TEXT,
    commit_sha   TEXT,                                  -- the code that ran (set by caller if known)
    actor        TEXT NOT NULL DEFAULT 'system'        -- 'user:alice' | 'agent:mcp' | 'system'
);

CREATE INDEX pipeline_run_recent     ON pipeline_run (started_at DESC);
CREATE INDEX pipeline_run_by_source  ON pipeline_run (source_id, started_at DESC);
CREATE INDEX pipeline_run_by_name    ON pipeline_run (name, started_at DESC);

-- Optional: connect each change_event to the pipeline_run that caused it.
-- The trigger reads a per-transaction setting (`SET LOCAL ontology.pipeline_run_id = '...'`)
-- and stamps the column. Backfill is impossible for events written before this
-- migration; nullable column makes that explicit.
ALTER TABLE change_event
    ADD COLUMN pipeline_run_id UUID REFERENCES pipeline_run(id) ON DELETE SET NULL,
    ADD COLUMN action_invocation_id UUID REFERENCES action_invocation(id) ON DELETE SET NULL;

CREATE INDEX change_event_by_pipeline_run ON change_event (pipeline_run_id, id);
CREATE INDEX change_event_by_action       ON change_event (action_invocation_id, id);

-- Update the trigger functions to read the per-transaction setting if present.
-- current_setting(name, missing_ok=true) returns NULL when unset.
CREATE OR REPLACE FUNCTION emit_change_event() RETURNS TRIGGER AS $$
DECLARE
    event_topic  TEXT;
    rec_id       UUID;
    payload      JSONB;
    pipeline_run UUID;
    action_inv   UUID;
BEGIN
    event_topic := 'ontology.' || TG_TABLE_NAME;
    IF TG_OP = 'DELETE' THEN
        rec_id  := OLD.id;
        payload := to_jsonb(OLD);
    ELSE
        rec_id  := NEW.id;
        payload := to_jsonb(NEW);
    END IF;

    pipeline_run := NULLIF(current_setting('ontology.pipeline_run_id', true), '')::UUID;
    action_inv   := NULLIF(current_setting('ontology.action_invocation_id', true), '')::UUID;

    INSERT INTO change_event
        (topic, "key", op, record_id, payload, pipeline_run_id, action_invocation_id)
    VALUES
        (event_topic, rec_id::text, TG_OP, rec_id, payload, pipeline_run, action_inv);

    PERFORM pg_notify('ontology_events', event_topic);
    RETURN COALESCE(NEW, OLD);
END $$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION emit_change_event_entity_state() RETURNS TRIGGER AS $$
DECLARE
    rec_id       UUID;
    payload      JSONB;
    pipeline_run UUID;
    action_inv   UUID;
BEGIN
    IF TG_OP = 'DELETE' THEN
        rec_id  := OLD.entity_id;
        payload := to_jsonb(OLD);
    ELSE
        rec_id  := NEW.entity_id;
        payload := to_jsonb(NEW);
    END IF;

    pipeline_run := NULLIF(current_setting('ontology.pipeline_run_id', true), '')::UUID;
    action_inv   := NULLIF(current_setting('ontology.action_invocation_id', true), '')::UUID;
    -- entity_state changes are usually action-driven; if last_action_id is set
    -- on the row, use it as a stronger signal than the per-tx variable.
    IF TG_OP <> 'DELETE' AND NEW.last_action_id IS NOT NULL THEN
        action_inv := NEW.last_action_id;
    END IF;

    INSERT INTO change_event
        (topic, "key", op, record_id, payload, pipeline_run_id, action_invocation_id)
    VALUES
        ('ontology.entity_state', rec_id::text, TG_OP, rec_id, payload, pipeline_run, action_inv);

    PERFORM pg_notify('ontology_events', 'ontology.entity_state');
    RETURN COALESCE(NEW, OLD);
END $$ LANGUAGE plpgsql;
