-- Tool-call telemetry: one row per LLM tool dispatch.
-- Written by the chatbot/MCP server's executeTool wrapper. Best-effort —
-- a telemetry write failure does not block the tool call.

CREATE TABLE tool_call_log (
    id              BIGSERIAL PRIMARY KEY,
    tool_name       TEXT NOT NULL,
    params          JSONB,
    actor           TEXT NOT NULL,           -- 'agent:claude' | 'agent:mcp' | etc.
    session_id      TEXT,                    -- chat session cookie (HTTP) or null (MCP)
    transport       TEXT NOT NULL,           -- 'http' | 'mcp'
    status          TEXT NOT NULL DEFAULT 'pending',  -- pending | ok | error
    invoked_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at     TIMESTAMPTZ,
    duration_ms     INTEGER,
    error_msg       TEXT,
    result_size     INTEGER                  -- byte count of returned JSON/text
);

CREATE INDEX tool_call_log_recent ON tool_call_log (invoked_at DESC);
CREATE INDEX tool_call_log_tool   ON tool_call_log (tool_name, invoked_at DESC);
CREATE INDEX tool_call_log_errors ON tool_call_log (status, invoked_at DESC) WHERE status = 'error';
CREATE INDEX tool_call_log_actor  ON tool_call_log (actor, invoked_at DESC);
