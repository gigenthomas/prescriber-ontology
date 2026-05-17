-- Idempotency keys on action invocations.
--
-- Agents retry on network errors / timeouts. Without an idempotency key,
-- the same conceptual action gets applied twice. With it, the second
-- invocation finds the prior row by key and returns the original result.
--
-- The key is OPTIONAL — callers without retry concerns can omit it.

ALTER TABLE action_invocation
    ADD COLUMN idempotency_key UUID;

-- Unique only when set (NULL allowed many times).
CREATE UNIQUE INDEX action_invocation_idempotency_key
    ON action_invocation (idempotency_key)
    WHERE idempotency_key IS NOT NULL;
