# Plan: Actions on Objects (v1)

**Status:** Proposed — awaiting decision on open questions before build
**Estimated effort:** ~1.5 days for v1
**Owner:** TBD
**Related:** [ARCHITECTURE.md](../ARCHITECTURE.md), [README.md](../README.md), [metrics.yaml](../metrics.yaml)

---

## Goal

Move the platform from "read-only Q&A over a knowledge graph" to "authorized,
audited operations against the graph." Today the LLM can answer questions; with
Actions it can also **change state** — flag a prescriber for review, add a drug
to a watchlist, attach a note to a case — with every change recorded and
traceable.

This is the Foundry / Gotham distinction: an ontology you can *do things to*,
not just discover.

## What's in v1

1. A canonical write path — schema, YAML config, invocation logger
2. A Go executor + tool surface — one Anthropic/MCP tool per action, dispatched
   through the existing handler infrastructure
3. Three reference actions to prove the model:
   - `flag_for_review` (Prescriber)
   - `add_to_watchlist` (Drug)
   - `add_note` (any entity)

## What's explicitly out of scope for v1

- Row-level authorization (trust the caller)
- Native undo / rollback (rely on inverse actions like `unflag`)
- Automatic Neo4j re-projection on each action (batch re-projection is enough)
- Business-rule validation beyond type checks
- Concurrent-action conflict resolution (last writer wins)

These are deferred to v2 and documented as known limitations.

---

## Concepts

| Term | Meaning |
|------|---------|
| **Action** | A typed, named operation with a target entity (or class of entities), parameters, effects, and authorization rules. Declared in `actions.yaml`. |
| **Action invocation** | A recorded execution of an action — who, what, when, with what params, what result. Stored in `action_invocation`. Append-only. |
| **State update** | An action's effect on the target entity's mutable state. Stored in `entity_state`, separate from source-derived `entity.attrs`. |

### Design decision: separate state from source attributes

Source-derived `entity.attrs` (from CMS, etc.) stays immutable. Mutable
state goes in a separate `entity_state` table. This keeps source reloads from
clobbering action state and keeps the audit story clean.

---

## Data model

Two new tables. No changes to existing tables.

```sql
-- Mutable, action-driven state per entity.
CREATE TABLE entity_state (
    entity_id        UUID PRIMARY KEY REFERENCES entity(id) ON DELETE CASCADE,
    state            JSONB NOT NULL DEFAULT '{}'::jsonb,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_action_id   UUID
);

-- Append-only audit log. Every action invocation lands here.
CREATE TABLE action_invocation (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    action_name         TEXT NOT NULL,
    target_entity_id    UUID REFERENCES entity(id) ON DELETE SET NULL,
    target_type         TEXT NOT NULL,
    target_external_id  TEXT NOT NULL,
    params              JSONB NOT NULL,
    actor               TEXT NOT NULL,              -- 'agent:claude' | 'user:alice' | 'system'
    session_id          TEXT,                       -- chat session for correlation
    invoked_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    status              TEXT NOT NULL,              -- 'applied' | 'rejected' | 'failed'
    error_msg           TEXT
);

CREATE INDEX action_invocation_target  ON action_invocation (target_entity_id, action_name);
CREATE INDEX action_invocation_recent  ON action_invocation (invoked_at DESC);
```

Migration filename: `db/postgres/migrations/0002_actions.sql`.

---

## Configuration — `actions.yaml`

Sibling to `metrics.yaml`. Same edit-restart-and-it's-live flow.

```yaml
actions:
  flag_for_review:
    description: Flag a prescriber for compliance review
    target_type: Prescriber
    params:
      reason:
        type: string
        required: true
        description: Why this prescriber is being flagged
      severity:
        type: enum
        values: [low, medium, high]
        default: medium
    state_updates:
      flagged:       true
      flag_reason:   "$reason"
      flag_severity: "$severity"

  unflag:
    description: Clear the review flag from a prescriber
    target_type: Prescriber
    state_updates:
      flagged:       false
      flag_reason:   null
      flag_severity: null

  add_to_watchlist:
    description: Add a drug to a named watchlist
    target_type: Drug
    params:
      list_name:
        type: enum
        values: [cost_watch, controlled_substance, off_label]
        required: true
    state_updates:
      watchlist: "$list_name"

  add_note:
    description: Attach a free-text note (audit trail only — does not change state)
    target_type: any
    params:
      note: {type: string, required: true}
    state_updates: {}    # log-only action; the note lives in action_invocation.params
```

Param substitution uses `$name` placeholders that get replaced with caller-supplied
values before the state update is applied.

---

## Code surface

```
db/postgres/migrations/
└── 0002_actions.sql              # NEW: tables + indexes

web/
├── actions.go                    # NEW: YAML loader, executor, tool generator
├── main.go                       # +loadActions(), +action tool registration
├── mcp.go                        # +mcpListActions, +mcpApplyAction, action tools
└── tools.go                      # +dispatch for action_* tool names

actions.yaml                      # NEW: declarative config at repo root
```

No Python ETL changes — Actions are a chatbot/MCP concern.

---

## Tool surface

**Pattern:** one Anthropic/MCP tool per declared action, plus two helpers.

The executor reads `actions.yaml` at startup and auto-generates a tool spec
per action with the right JSON schema. The chatbot and MCP server both register
them.

| Tool | Purpose |
|------|---------|
| `list_actions` | Discovery: returns every declared action with target type, params, description |
| `entity_actions` | History: last N actions applied to a specific entity |
| `action_flag_for_review` | Auto-generated from `actions.yaml` |
| `action_unflag` | Auto-generated |
| `action_add_to_watchlist` | Auto-generated |
| `action_add_note` | Auto-generated |

System prompt is updated to list available actions (similar to how metrics
and queries are surfaced today) so the LLM knows they exist without having to
call `list_actions` first.

---

## Executor flow

Pseudocode for the core execute path:

```go
func executeAction(ctx context.Context, name string, in actionInput) (*actionResult, error) {
    // 1. Look up action definition
    def, ok := actionCfg.Actions[name]
    if !ok {
        return nil, fmt.Errorf("unknown action %q", name)
    }

    // 2. Resolve and validate target entity
    target, err := lookupEntity(ctx, in.ExternalID, def.TargetType)
    if err != nil {
        return nil, err
    }

    // 3. Validate params and substitute placeholders
    params, err := validateParams(def.Params, in.Params)
    if err != nil {
        return nil, err
    }
    stateUpdates := substituteParams(def.StateUpdates, params)

    // 4. Single transaction: audit row + state update
    tx, _ := pgPool.Begin(ctx)
    defer tx.Rollback(ctx)

    inv, err := insertActionInvocation(tx, name, target, params, in.Actor, in.SessionID)
    if err != nil {
        return nil, err
    }
    if len(stateUpdates) > 0 {
        if err := applyStateUpdate(tx, target.ID, stateUpdates, inv.ID); err != nil {
            return nil, err
        }
    }
    if err := tx.Commit(ctx); err != nil {
        return nil, err
    }

    return &actionResult{InvocationID: inv.ID, Updates: stateUpdates}, nil
}
```

Both audit row and state update happen in one transaction. Either both land or
neither does.

---

## Phased delivery

| Phase | Scope | Effort |
|-------|-------|--------|
| **1. Plumbing** | Migration `0002_actions.sql`; `loadActions()` reading `actions.yaml`; executor with param substitution + state-update writes + audit logging | ~4 hours |
| **2. Tool surface** | Auto-generate per-action tools for both Anthropic web mode and MCP; wire `list_actions` and `entity_actions`; update system prompt | ~3 hours |
| **3. Reference actions + tests** | Ship the three sample actions; add three smoke tests exercising them via MCP; extend `ontology verify` to include `action_invocation` FK integrity check | ~2 hours |
| **4. Documentation** | Update [ARCHITECTURE.md](../ARCHITECTURE.md) with a new "Actions" section; update [README.md](../README.md) with the extension recipe; flag the v1 limitations | ~1 hour |

**Total: ~10 hours of focused work.** Each phase is independently shippable.

---

## User-facing change

**Before** (today): the chatbot is purely conversational.

> *"Who prescribes the most Eliquis in California?"* → answer.

**After Actions ship:**

> *"NPI 1003000126 has three controlled-substance drugs in their top 5. Flag them for compliance review."*
>
> LLM calls `search_entities("1003000126", "Prescriber")` → confirms the exact match
> LLM calls `action_flag_for_review(external_id="1003000126", reason="Three controlled substances in top-5 by claims", severity="high")`
> Chat UI shows the tool trace: `→ action_flag_for_review(...)` then `← invocation_id=... status=applied`
> Postgres now has a row in `action_invocation` and `entity_state.state.flagged = true`

The platform shifts from "answer questions about the data" to "do work against
the data, with full audit."

---

## Risks and mitigations

| Risk | Mitigation |
|------|------------|
| LLM applies a consequential action it shouldn't | Every consequential action requires a `reason` param (forces the LLM to articulate intent); the chat UI surfaces every invocation as a trace line; `action_invocation` is permanent audit log |
| State drifts from source data | `entity.attrs` (source) and `entity_state` (action-driven) are separate tables — a source reload never touches action state |
| Neo4j projection diverges from Postgres | `entity_state` keys are projected with a `state_` prefix on Neo4j nodes; the projector is rebuildable any time |
| Concurrent actions on the same entity | v1 accepts last-writer-wins; documented limitation. v2 to add optimistic concurrency via `last_action_id`. |
| Verify suite doesn't catch action-table regressions | Phase 3 extends `ontology verify` with FK integrity checks on `action_invocation` |

---

## Open decisions

These should be answered before Phase 1 starts. Defaults below if no preference.

1. **Neo4j synchronization cadence.** Re-project on every state change (requires
   the `LISTEN/NOTIFY` outbox infrastructure from the events tier), or batched
   (run `ontology project` periodically)?
   *Default: batched.*

2. **Authorization model.** Does v1 trust the caller, or do we want a
   `role` → `allowed_actions` table from day one?
   *Default: trust the caller; mark every invocation with `actor` for audit.*

3. **Undo / rollback semantics.** Inverse-action only (`unflag` clears `flag`),
   or native per-invocation rollback via event replay?
   *Default: inverse-action only.*

4. **Where actions.yaml lives.** Project root (next to metrics.yaml), or a
   dedicated `config/` directory?
   *Default: project root, for symmetry with metrics.yaml.*

---

## Verification plan

Before declaring v1 done, all of these must pass:

- [ ] `ontology verify` passes with the new tables present
- [ ] Calling `action_flag_for_review` via MCP writes one row each to
      `action_invocation` and `entity_state`
- [ ] Calling `action_unflag` clears the previous flag (idempotent)
- [ ] Calling an unknown action returns a structured error, not a 500
- [ ] Calling an action with a wrong target type returns a structured error
- [ ] Re-running `ontology project` after actions correctly projects
      `state_*` properties onto the corresponding Neo4j nodes
- [ ] `list_actions` returns all four reference actions with their schemas
- [ ] `entity_actions` returns the invocation history for a prescriber that
      has been flagged and then unflagged

---

## Beyond v1

Once v1 ships, the natural extensions are:

- **Role-based authorization** — `role`, `role_action` tables; per-action allow-lists
- **Native undo** — event-sourced state; each invocation becomes reversible
- **Composite actions** — declare a sequence of state updates + side effects
   (e.g., "flag + open case + notify Slack")
- **Action triggers** — fire an action automatically when a condition is met
   (composes with the events tier)
- **Approval workflows** — actions that go to `status=pending` and wait for a
   human approver before being applied
- **Time-travel** — query "what was this entity's state on date X" by replaying
   `action_invocation` up to that timestamp
