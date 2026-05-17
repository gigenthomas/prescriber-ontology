# Plan: Events Tier (v1 — Postgres outbox, Kafka migration path)

**Status:** Proposed — awaiting decisions on open questions before build
**Estimated effort:** ~9 hours for v1
**Owner:** TBD
**Related:** [actions-plan.md](actions-plan.md), [ARCHITECTURE.md](../ARCHITECTURE.md)

---

## Goal

Build a durable, in-process events tier today using Postgres
`LISTEN/NOTIFY` and a transactional-outbox table, **shaped so that swapping
the transport to Kafka is a drop-in change** when scale demands it.

Concrete payoff in v1: the Neo4j projection becomes automatic — every change
in Postgres triggers an incremental update on the graph side, no more manual
`ontology project` runs.

## What's in v1

1. `change_event` outbox table + `consumer_cursor` for per-consumer offsets
2. Postgres triggers on `entity`, `relation`, and (when added) `entity_state`
   that write to `change_event` and fire `pg_notify`
3. A Go consumer framework (`EventConsumer` type) that listens, drains, and
   commits cursors
4. One reference consumer: `neo4j_reprojector` — reacts to entity/relation
   changes by incrementally re-projecting the affected nodes/edges
5. Migration path to Kafka documented explicitly so the design choices are
   forward-compatible

## What's explicitly out of scope for v1

- Kafka, RabbitMQ, NATS, Redis Streams — see migration section
- Dead-letter handling beyond a single retry (manual intervention for now)
- Per-consumer auth (single in-process consumer)
- Schema registry / event versioning beyond `topic` + JSONB payload
- Partitioning of `change_event` for retention (manual prune in v1)
- Streaming aggregations (this is event delivery, not stream processing)

---

## Concepts

| Term | Meaning |
|------|---------|
| **change_event** | Append-only outbox row representing one mutation: who, what, when, payload. Written by a trigger inside the same transaction as the source change. |
| **Topic** | Logical event stream name (`ontology.entity`, `ontology.relation`, `ontology.entity_state`). Maps 1:1 to a Kafka topic when we migrate. |
| **Consumer** | A named subscriber that reads new events for one or more topics and runs a handler. Tracks its own cursor; never blocks others. |
| **Cursor** | The last `change_event.id` a given consumer successfully processed. Persisted in `consumer_cursor`. |
| **Notify** | A Postgres-level wake-up signal so consumers don't poll. Fire-and-forget — consumers always drain on startup to catch missed signals. |

### Why outbox-and-LISTEN/NOTIFY (not just LISTEN/NOTIFY)

`LISTEN/NOTIFY` alone is lossy — if no listener is connected at the moment
of `NOTIFY`, the signal is dropped. The outbox table provides durability;
`NOTIFY` is the wake-up. Consumers always start with a drain, so missed signals
just delay processing, never lose data.

---

## Data model

```sql
-- Append-only event log. One row per mutation, written in the same
-- transaction as the source change.
CREATE TABLE change_event (
    id            BIGSERIAL PRIMARY KEY,            -- natural ordering, mirrors a Kafka offset
    occurred_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    topic         TEXT NOT NULL,                    -- maps to Kafka topic
    key           TEXT,                             -- maps to Kafka partition key
    op            TEXT NOT NULL,                    -- INSERT | UPDATE | DELETE
    record_id     UUID,
    payload       JSONB,                            -- to_jsonb(NEW) or to_jsonb(OLD)
    headers       JSONB NOT NULL DEFAULT '{}'::jsonb
);

CREATE INDEX change_event_topic_id ON change_event (topic, id);
CREATE INDEX change_event_recent   ON change_event (occurred_at DESC);

-- Each consumer tracks its own progress per topic. Mirrors Kafka consumer
-- group offsets. Multiple consumers never block each other.
CREATE TABLE consumer_cursor (
    consumer_name  TEXT NOT NULL,
    topic          TEXT NOT NULL,
    last_id        BIGINT NOT NULL DEFAULT 0,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (consumer_name, topic)
);
```

### Trigger function

```sql
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

    INSERT INTO change_event (topic, key, op, record_id, payload)
    VALUES (event_topic, rec_id::text, TG_OP, rec_id, payload);

    PERFORM pg_notify('ontology_events', event_topic);
    RETURN COALESCE(NEW, OLD);
END $$ LANGUAGE plpgsql;

CREATE TRIGGER entity_change       AFTER INSERT OR UPDATE OR DELETE ON entity        FOR EACH ROW EXECUTE FUNCTION emit_change_event();
CREATE TRIGGER relation_change     AFTER INSERT OR UPDATE OR DELETE ON relation      FOR EACH ROW EXECUTE FUNCTION emit_change_event();
-- entity_state trigger added in Actions plan migration
```

Migration filename: `db/postgres/migrations/0003_events.sql`.

### Performance escape hatch for bulk loads

Bulk ETL writes 2.5M rows into `entity`+`relation`. Firing 2.5M triggers slows
ingest noticeably. The mitigation is a per-session bypass flag:

```sql
-- In the ETL transaction:
SET LOCAL session_replication_role = replica;   -- skips user triggers
-- COPY / INSERT bulk data
RESET session_replication_role;
-- Emit a single 'bulk_load_completed' event so consumers can do a full re-project
INSERT INTO change_event (topic, op, payload)
VALUES ('ontology.bulk_load_completed', 'INSERT', '{"source":"prescriber.load"}');
NOTIFY ontology_events;
```

Documented as a Python helper: `with bulk_load_mode(): ...`

---

## Code surface

```
db/postgres/migrations/
└── 0003_events.sql              # NEW: change_event, consumer_cursor, triggers

web/
├── events.go                    # NEW: ChangeEvent, EventConsumer, drain loop, cursor I/O
├── consumers.go                 # NEW: registered consumers (neo4j_reprojector)
├── main.go                      # +startConsumers() goroutine in both runHTTP() and runMCP()
└── tools.go                     # (no chatbot tool changes — events are infrastructure)

src/ontology/
├── ingest/prescriber/load.py    # +bulk_load_mode() context manager
└── verify.py                    # +check that change_event has rows after a load
```

No new YAML — consumer registration is in Go (compile-time, like routes).

---

## Consumer framework

### Type

```go
type ChangeEvent struct {
    ID         int64           `json:"id"`
    OccurredAt time.Time       `json:"occurred_at"`
    Topic      string          `json:"topic"`
    Key        string          `json:"key"`
    Op         string          `json:"op"`
    RecordID   uuid.UUID       `json:"record_id"`
    Payload    json.RawMessage `json:"payload"`
}

type EventConsumer struct {
    Name    string                          // identity for cursor tracking
    Topics  []string                        // subscribed topics
    Handler func(context.Context, ChangeEvent) error
}
```

### Run loop

```go
func (c *EventConsumer) Run(ctx context.Context) error {
    conn := acquireListenConn(ctx)
    defer conn.Release()

    if _, err := conn.Exec(ctx, "LISTEN ontology_events"); err != nil {
        return err
    }

    for {
        if err := c.drain(ctx); err != nil {
            log.Printf("[%s] drain: %v", c.Name, err)
        }
        select {
        case <-ctx.Done():
            return ctx.Err()
        case <-time.After(30 * time.Second):
            // heartbeat — safety net in case NOTIFY was missed
        case <-conn.NotifyChan():
            // signaled
        }
    }
}

func (c *EventConsumer) drain(ctx context.Context) error {
    cursors := loadCursors(ctx, c.Name, c.Topics)
    rows, err := pgPool.Query(ctx, `
        SELECT id, occurred_at, topic, key, op, record_id, payload
        FROM change_event
        WHERE topic = ANY($1)
          AND id > (SELECT min(last_id) FROM consumer_cursor WHERE consumer_name=$2 AND topic=ANY($1))
        ORDER BY id
        LIMIT 1000`, c.Topics, c.Name)
    if err != nil { return err }
    defer rows.Close()

    for rows.Next() {
        var ev ChangeEvent
        // scan...
        if cursors[ev.Topic] >= ev.ID { continue }
        if err := c.Handler(ctx, ev); err != nil {
            log.Printf("[%s] handle id=%d: %v", c.Name, ev.ID, err)
            return err   // bail; retry on next drain
        }
        updateCursor(ctx, c.Name, ev.Topic, ev.ID)
    }
    return rows.Err()
}
```

### The reference consumer

```go
func newNeo4jReprojector() *EventConsumer {
    return &EventConsumer{
        Name:   "neo4j_reprojector",
        Topics: []string{"ontology.entity", "ontology.relation", "ontology.entity_state", "ontology.bulk_load_completed"},
        Handler: func(ctx context.Context, ev ChangeEvent) error {
            switch ev.Topic {
            case "ontology.entity":               return reprojectEntity(ctx, ev)
            case "ontology.relation":             return reprojectRelation(ctx, ev)
            case "ontology.entity_state":         return reprojectEntityState(ctx, ev)
            case "ontology.bulk_load_completed":  return runFullProjection(ctx)
            }
            return nil
        },
    }
}
```

`runFullProjection` re-uses the existing batched projector — the bulk-load
event is a signal to do a full pass rather than 2.5M individual updates.

---

## Phased delivery

| Phase | Scope | Effort |
|-------|-------|--------|
| **1. Schema + triggers** | Migration `0003_events.sql`; trigger function on `entity` and `relation`; smoke test that an `INSERT` produces a `change_event` row | ~3 hours |
| **2. Consumer framework** | `events.go` — `EventConsumer`, `Run`, `drain`, cursor I/O; LISTEN/NOTIFY plumbing; graceful shutdown | ~3 hours |
| **3. Neo4j reprojector** | `consumers.go` with incremental entity/relation/state handlers + bulk-load fallback; wired into both `runHTTP()` and `runMCP()` via goroutine; tested end-to-end | ~2 hours |
| **4. Docs + verify** | Update [ARCHITECTURE.md](../ARCHITECTURE.md) with an Events section; extend `ontology verify` with a `change_event` integrity check; document the bulk-load bypass | ~1 hour |

**Total: ~9 hours.** Phases are independently shippable.

---

## User-facing change

**Before:** the operator runs `ontology project` manually after every load.

**After v1:** every change to `entity`, `relation`, or `entity_state` (whether
from ETL, the chatbot's Actions, or direct SQL) automatically propagates to
Neo4j within a second. The operator never needs to think about it.

```
ontology load --year 2024 --state NY
  -> load (Postgres, ~3 min)
  -> bulk_load_completed event emitted
  -> neo4j_reprojector consumes event
  -> incremental projection runs in background (~10 min)
  -> operator returns to other work, Neo4j catches up automatically
```

---

## Migration path to Kafka

This is what makes the v1 design future-proof. The `change_event` row is
**shaped like a Kafka record on purpose** — the migration becomes incremental
rather than a rewrite.

### Field-level mapping

| Postgres outbox | Kafka equivalent |
|---|---|
| `change_event.id` | Offset (per partition) |
| `change_event.topic` | Topic name |
| `change_event.key` | Message key (partition routing) |
| `change_event.payload` | Message value |
| `change_event.headers` | Message headers |
| `consumer_cursor` | Consumer group offsets |

### Migration steps

1. **Stand up Kafka or Redpanda** alongside Postgres. No application changes.
2. **Run a relay process** that reads new `change_event` rows in order and
   publishes them to Kafka with the corresponding topic + key. This is either
   Debezium (CDC on the outbox table) or a 50-line Go goroutine — either way,
   it's tiny.
3. **Existing consumers keep running** unchanged on Postgres. New consumers
   can be added on the Kafka side at any time without coordinating with
   existing ones.
4. **Cut over one consumer at a time** — start by mirroring an existing
   consumer onto Kafka, run both in parallel to verify they produce identical
   results, then turn off the Postgres-side instance.
5. **Eventually retire the outbox** if all consumers are on Kafka, or keep it
   as a durable replay buffer.

### What does NOT change

- The trigger functions
- The `change_event` table
- Any consumer's handler logic
- The cursor tracking model (Kafka has equivalent offsets per consumer group)

### What does change

- The transport between producer (Postgres) and consumer (Go)
- Operational surface (broker monitoring, partition strategy, retention)
- Cost (infrastructure + ops)

That's it. The schema, the consumer code, and the application semantics
survive the transition unchanged.

### When to actually flip

Defer to the "When to flip to Kafka" criteria in the original architecture
discussion. Restated for the record: flip when at least two of the following
become true.

1. Three or more downstream consumers, each owned by a different team
2. Sustained event volume above ~10k events/sec
3. Cross-team replay over multi-month windows
4. Hard exactly-once across multiple sinks
5. An operational team that already runs Kafka professionally

Until then, the Postgres outbox is strictly cheaper, simpler, and sufficient.

---

## Risks and mitigations

| Risk | Mitigation |
|------|------------|
| Trigger overhead slows bulk ETL | `session_replication_role = replica` bypass during `COPY`; emit single bulk-load event afterwards |
| `change_event` grows unbounded | Phase 4 documents monthly partitioning + drop-old-partition cron; v1 acceptable up to ~50M rows |
| Consumer crashes mid-batch | Cursor only advances on successful handler return; next drain reprocesses from the last committed cursor (at-least-once delivery) |
| Handler is not idempotent | Reference consumer (`reprojectEntity`) uses MERGE in Neo4j, which is idempotent. Documented invariant for any future consumer. |
| `NOTIFY` is dropped (no listener connected) | Consumers drain on startup *before* listening, and heartbeat every 30 seconds — missed `NOTIFY` only delays processing, never loses it |
| Cursor lag goes unnoticed | Phase 4 extends `ontology verify` to alert when `max(change_event.id) - max(consumer_cursor.last_id) > N` |

---

## Open decisions

1. **Multi-consumer support from day one.** Use the cursor table (per-consumer
   offsets), or single-consumer with a `consumed_at` column on each event?
   *Default: cursor table — small cost, ready for the Kafka migration.*

2. **Retention.** How long do we keep `change_event` rows?
   *Default: 90 days, monthly partitions, drop oldest via cron.*

3. **Failure handling.** After how many handler errors do we move an event to
   a dead-letter table (or skip it)?
   *Default: 5 retries with exponential backoff, then skip and log loudly;
   build the dead-letter table in v2.*

4. **Should Actions emit explicit business events?** Today every change to
   `entity_state` produces a `change_event`. Should Actions also emit
   higher-level events like `prescriber.flagged`?
   *Default: not in v1 — derive from `entity_state` events. Add explicit
   business events in v2 when consumers want type-safe subscriptions.*

5. **Where the relay-to-Kafka logic lives if we ever migrate.** A new Go
   service, an existing service, Debezium, or another sidecar?
   *Decide at migration time — depends on the operational footprint then.*

---

## Verification plan

Before declaring v1 done:

- [ ] `ontology verify` passes with the new tables present
- [ ] Inserting a row into `entity` produces exactly one `change_event` row
- [ ] The reference consumer drains a fresh `change_event` row within 5 seconds
      and reflects the change in Neo4j
- [ ] A bulk load with the `session_replication_role` bypass produces one
      `bulk_load_completed` event and triggers a full Neo4j re-projection
- [ ] Killing the chatbot process mid-batch and restarting it resumes from the
      committed cursor; no events are lost or double-processed
- [ ] `ontology verify` includes a check that `consumer_cursor.last_id` is
      within ~1000 events of `max(change_event.id)` per topic (lag bound)

---

## Beyond v1

- **Dead-letter table + retry policy** for events that handlers can't process
- **`pg_partman`-based monthly partitioning** for `change_event` with cron-driven retention
- **Schema versioning** — `change_event.headers.schema_version` so consumers can evolve
- **Multiple reference consumers** — Slack notifier, audit log writer, search-index indexer (each one validates the multi-consumer story)
- **Explicit business-level events** (`prescriber.flagged`, `drug.watchlisted`) emitted by the Actions executor in addition to the `entity_state` mutation event
- **Kafka migration** — when at least two of the trigger criteria above become true
