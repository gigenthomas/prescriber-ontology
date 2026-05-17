-- Events tier (v1): Postgres transactional outbox + LISTEN/NOTIFY.
-- Designed so the row schema matches a Kafka record 1:1 — when scale demands,
-- the migration to Kafka is a transport swap, not a rewrite.
-- See docs/events-plan.md.

-- Append-only event log. One row per source-table mutation, written in the
-- same transaction as the source change.
CREATE TABLE change_event (
    id            BIGSERIAL PRIMARY KEY,             -- mirrors a Kafka offset
    occurred_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    topic         TEXT NOT NULL,                     -- maps to Kafka topic
    "key"         TEXT,                              -- maps to Kafka partition key
    op            TEXT NOT NULL,                     -- INSERT | UPDATE | DELETE | EVENT
    record_id     UUID,
    payload       JSONB,                             -- to_jsonb(NEW) or to_jsonb(OLD)
    headers       JSONB NOT NULL DEFAULT '{}'::jsonb
);

CREATE INDEX change_event_topic_id ON change_event (topic, id);
CREATE INDEX change_event_recent   ON change_event (occurred_at DESC);

-- Per-consumer offsets. Equivalent to a Kafka consumer-group offset table.
-- Each consumer tracks its own progress per topic.
CREATE TABLE consumer_cursor (
    consumer_name  TEXT NOT NULL,
    topic          TEXT NOT NULL,
    last_id        BIGINT NOT NULL DEFAULT 0,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (consumer_name, topic)
);

-- Trigger function: writes a change_event row and fires pg_notify.
-- Triggered tables: entity, relation, entity_state.
CREATE OR REPLACE FUNCTION emit_change_event() RETURNS TRIGGER AS $$
DECLARE
    event_topic TEXT;
    rec_id      UUID;
    payload     JSONB;
BEGIN
    event_topic := 'ontology.' || TG_TABLE_NAME;
    IF TG_OP = 'DELETE' THEN
        rec_id  := OLD.id;
        payload := to_jsonb(OLD);
    ELSE
        rec_id  := NEW.id;
        payload := to_jsonb(NEW);
    END IF;

    INSERT INTO change_event (topic, "key", op, record_id, payload)
    VALUES (event_topic, rec_id::text, TG_OP, rec_id, payload);

    PERFORM pg_notify('ontology_events', event_topic);
    RETURN COALESCE(NEW, OLD);
END $$ LANGUAGE plpgsql;

-- entity_state uses entity_id as PK (not id), handle that variant.
CREATE OR REPLACE FUNCTION emit_change_event_entity_state() RETURNS TRIGGER AS $$
DECLARE
    rec_id  UUID;
    payload JSONB;
BEGIN
    IF TG_OP = 'DELETE' THEN
        rec_id  := OLD.entity_id;
        payload := to_jsonb(OLD);
    ELSE
        rec_id  := NEW.entity_id;
        payload := to_jsonb(NEW);
    END IF;

    INSERT INTO change_event (topic, "key", op, record_id, payload)
    VALUES ('ontology.entity_state', rec_id::text, TG_OP, rec_id, payload);

    PERFORM pg_notify('ontology_events', 'ontology.entity_state');
    RETURN COALESCE(NEW, OLD);
END $$ LANGUAGE plpgsql;

CREATE TRIGGER entity_change       AFTER INSERT OR UPDATE OR DELETE ON entity        FOR EACH ROW EXECUTE FUNCTION emit_change_event();
CREATE TRIGGER relation_change     AFTER INSERT OR UPDATE OR DELETE ON relation      FOR EACH ROW EXECUTE FUNCTION emit_change_event();
CREATE TRIGGER entity_state_change AFTER INSERT OR UPDATE OR DELETE ON entity_state  FOR EACH ROW EXECUTE FUNCTION emit_change_event_entity_state();
