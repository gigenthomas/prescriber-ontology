-- Auth integration: capture OPA decision per tool call + user cache.
-- See docs/auth-plan.md (phase 2 — authorization).

-- Every tool-call log entry now records the policy outcome.
-- opa_allow may be NULL on rows written before this migration; tooling
-- treats NULL as "no policy engine consulted" (pre-phase-2 behaviour).
ALTER TABLE tool_call_log
    ADD COLUMN opa_allow  BOOLEAN,
    ADD COLUMN opa_reason TEXT;

CREATE INDEX tool_call_log_denied ON tool_call_log (invoked_at DESC)
    WHERE opa_allow = false;

-- Cached user identity mirrored from Keycloak so audit-log queries can
-- render display names without round-tripping the IdP. Populated on login
-- and refreshed lazily.
CREATE TABLE user_cache (
    subject     UUID PRIMARY KEY,
    email       TEXT,
    name        TEXT,
    roles       TEXT[] NOT NULL DEFAULT '{}',
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
