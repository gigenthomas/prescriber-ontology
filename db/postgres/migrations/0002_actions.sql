-- Action invocation audit log (append-only).
-- Every applied or rejected action call lands here.
CREATE TABLE action_invocation (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    action_name         TEXT NOT NULL,
    target_entity_id    UUID REFERENCES entity(id) ON DELETE SET NULL,
    target_type         TEXT NOT NULL,
    target_external_id  TEXT NOT NULL,
    params              JSONB NOT NULL DEFAULT '{}'::jsonb,
    actor               TEXT NOT NULL,
    session_id          TEXT,
    invoked_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    status              TEXT NOT NULL CHECK (status IN ('applied', 'rejected', 'failed')),
    error_msg           TEXT
);

CREATE INDEX action_invocation_target  ON action_invocation (target_entity_id, action_name);
CREATE INDEX action_invocation_recent  ON action_invocation (invoked_at DESC);
CREATE INDEX action_invocation_by_name ON action_invocation (action_name, invoked_at DESC);

-- Mutable, action-driven state per entity. Separate from entity.attrs
-- (which is source-derived and immutable) so source reloads don't clobber
-- action state.
CREATE TABLE entity_state (
    entity_id        UUID PRIMARY KEY REFERENCES entity(id) ON DELETE CASCADE,
    state            JSONB NOT NULL DEFAULT '{}'::jsonb,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_action_id   UUID REFERENCES action_invocation(id) ON DELETE SET NULL
);

CREATE INDEX entity_state_attrs_gin ON entity_state USING gin (state);
