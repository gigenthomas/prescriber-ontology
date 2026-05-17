# Features

A running inventory of everything shipped, with where it lives and what it
enables. Group order roughly reflects build order.

---

## 1 · Data foundation

| Feature | Where | What it enables |
|---|---|---|
| Hybrid Postgres + Neo4j stack | [docker-compose.yml](../docker-compose.yml) | Typed canonical store + graph projection running locally with one command |
| Provenance per fact | `source` + `source_id` columns | Every entity and relation traceable back to its originating dataset |
| Controlled vocabulary | [`schema_term`](../db/postgres/migrations/0001_init.sql) table | Registered entity types and predicates; foundation for catalog discovery |
| Entity / relation / label tables | `entity`, `relation`, `entity_label` | Canonical graph in Postgres with JSONB attrs, indexed for trigram search |
| Neo4j projection | [src/ontology/project/to_neo4j.py](../src/ontology/project/to_neo4j.py) | Idempotent MERGE-based projection of Postgres into Neo4j; rebuildable anytime |

## 2 · Ingest pipeline (CMS Medicare Part D Prescribers)

| Feature | Where | What it enables |
|---|---|---|
| Streaming CSV fetch + cache | [src/ontology/ingest/prescriber/fetch.py](../src/ontology/ingest/prescriber/fetch.py) | Downloads ~3.8 GB CMS file once; resumable; validated as XML/CSV not error page |
| State-filtered COPY into staging | [src/ontology/ingest/prescriber/load.py](../src/ontology/ingest/prescriber/load.py) | Stream + filter + COPY at ~40k rows/sec into `mup_dpr_staging` |
| Entity + relation derivation | [src/ontology/ingest/prescriber/sql.py](../src/ontology/ingest/prescriber/sql.py) | 9 SQL passes turn 2.5M staging rows into 5 entity types + 4 relation types |
| MeSH loader (alternate dataset) | [src/ontology/ingest/mesh/](../src/ontology/ingest/mesh/) | Reference implementation for a non-prescriber domain (kept after pivot) |

**Currently loaded:** 114,815 entities · 2,679,499 relations · California 2023 only.

## 3 · Verification

| Feature | Where | What it enables |
|---|---|---|
| `ontology verify` CLI | [src/ontology/verify.py](../src/ontology/verify.py) | 13 checks: data presence, no null labels, no orphan relations, no duplicate keys, Neo4j constraints, cross-store counts match, 20-entity sample consistency |
| Pass/fail report with non-zero exit | `ontology verify` | CI-gateable; structured for automation |
| Scope filter | `--scope postgres | neo4j | all` | Run partial checks during development |

## 4 · Semantic layer (read side)

| Feature | Where | What it enables |
|---|---|---|
| Declarative metrics | [metrics.yaml](../metrics.yaml) | 13 metrics: total_cost, total_claims, total_beneficiaries, total_30day_fills, total_day_supply, prescription_count, unique_prescribers, unique_drugs, avg_cost_per_claim, senior_cost, senior_claims, avg_cost_per_beneficiary, avg_day_supply_per_claim |
| Declarative dimensions | [metrics.yaml](../metrics.yaml) | 7 dimensions: drug, generic, specialty, city, prescriber, name_length (demo), brand_vs_generic |
| On-the-fly Cypher compiler | [web/metrics.go](../web/metrics.go) | Composes metric × group_by × filters into Cypher at request time; returns compiled query alongside results |
| Named hand-written queries | [queries/](../queries/) | 9 .cypher files for questions that don't fit the metric pattern (co_prescribed, brands_per_generic, prescriber_overview, etc.) |
| Auto-discovered catalog | [web/main.go](../web/main.go) `loadQueryCatalog` | First comment block becomes description; `$param` references auto-extracted |

## 5 · Action layer (write side)

| Feature | Where | What it enables |
|---|---|---|
| Declarative actions | [actions.yaml](../actions.yaml) | 4 reference actions: flag_for_review, unflag, add_to_watchlist, add_note |
| Mutable entity state | [db/postgres/migrations/0002_actions.sql](../db/postgres/migrations/0002_actions.sql) | `entity_state` table separate from source-derived `entity.attrs` |
| Append-only audit log | `action_invocation` table | Every applied AND rejected invocation logged with actor, params, error, timestamp |
| Transactional executor | [web/actions.go](../web/actions.go) `executeAction` | Audit row + state update commit in one transaction |
| Type-checked params | `validateAndSubstitute` | string / enum / integer / number / boolean checks; defaults; required validation; unknown-param rejection |
| `$param` substitution | `substituteValue` | Placeholder syntax in `state_updates` resolved from validated input |
| Per-action Anthropic + MCP tool generation | [web/actions.go](../web/actions.go) + [web/mcp.go](../web/mcp.go) | Each YAML action auto-generates a typed `action_<name>` tool with full schema |
| Action history per entity | `doEntityActions` | `entity_actions(external_id, type)` returns invocations + current state |

## 6 · LLM interface (the chatbot)

| Feature | Where | What it enables |
|---|---|---|
| Web chat UI | [web/templates/index.html](../web/templates/index.html) + [web/main.go](../web/main.go) | HTMX-driven, single-page, server-rendered. No build step. |
| Tool-use loop | `runAgent` in main.go | Up to 12 rounds; full Anthropic SDK with tool dispatch |
| Per-session conversation state | `sessions sync.Map` keyed by cookie | 4-hour expiry; in-process |
| Inline tool trace | Chat UI yellow lines | Every tool call + result shown for transparency / auditability |
| Anthropic prompt caching | `runAgent` cache_control on system block | ~3,778 tokens cached per call after first; ~10× cheaper on cached portion |
| Usage logging | `logUsage` helper | input / output / cache_create / cache_read in web.log per call |
| Configurable model | `ANTHROPIC_MODEL` env (default `claude-sonnet-4-6`) | Swap to Opus 4.7 or others without rebuild |
| Configurable port | `ADDR` env (default `:8080`) | Multi-instance / different deployments |

## 7 · MCP server (same binary, stdio transport)

| Feature | Where | What it enables |
|---|---|---|
| `-mcp` flag dispatch | [web/main.go](../web/main.go) | One binary, two modes: HTTP chatbot or MCP stdio server |
| Full tool surface mirror | [web/mcp.go](../web/mcp.go) | Same tools available to MCP clients (Claude Desktop, Claude Code, etc.) |
| Read-only / write annotations | `readOnly` vs `writeAction` opts | Correct MCP tool annotations for downstream UI hints |
| Per-action MCP tools | `buildMCPActionTool` | Auto-generated from actions.yaml |
| Server instructions | `mcpInstructions()` | Workflow hints injected into MCP `initialize` response |

## 8 · Actions audit UI

| Feature | Where | What it enables |
|---|---|---|
| `/actions` web page | [web/templates/actions.html](../web/templates/actions.html) + [web/actions_ui.go](../web/actions_ui.go) | Browseable audit log of every invocation |
| Summary cards | Top of `/actions` | Total / applied / rejected / entities-with-state counts |
| Server-side filters | GET query params | action, status, target_type, target external_id |
| Joined state column | LEFT JOIN entity_state | Current state of the target entity displayed alongside the invocation |
| Status badges | CSS in actions.html | Green applied, red rejected, yellow failed; rejected rows show inline error |
| Chat-header link | index.html nav | One-click from chat to audit and back |

## 9 · Documentation

| Doc | Audience | Purpose |
|---|---|---|
| [README.md](../README.md) | Developers | Stack, prerequisites, quick-start, CLI cheat sheet, MCP wiring, extension recipes |
| [ARCHITECTURE.md](../ARCHITECTURE.md) | Executives | Strategic framing, agent-consumption model, decisions, semantic-layer explainer |
| [docs/features.md](features.md) | All audiences | This file — the running inventory |
| [docs/demo.md](demo.md) | Demo presenters | 5-act demo script, preflight, prompts that consistently land well, cleanup |
| [docs/actions-plan.md](actions-plan.md) | Implementers | Pre-built Actions-on-Objects spec (now built — historical reference) |
| [docs/events-plan.md](events-plan.md) | Implementers | LISTEN/NOTIFY events tier with documented Kafka migration path |

## 10 · Operational tooling

| Feature | Where | What it enables |
|---|---|---|
| `ontology` Typer CLI | [src/ontology/cli.py](../src/ontology/cli.py) | init, reset, fetch, load, project, verify, stats, list-queries, query subcommands |
| Docker Compose stack | [docker-compose.yml](../docker-compose.yml) | Postgres 16 + Neo4j 5 (with APOC) with healthchecks and persistent volumes |
| `.env.example` | Repo root | Documents every config knob |
| Healthcheck endpoint | `GET /healthz` | Currently returns "ok"; basis for richer DB-ping check |
| Reset command | `ontology reset --yes` | Wipes both stores while preserving schema and constraints |

## 11 · Events tier (auto Postgres → Neo4j propagation)

| Feature | Where | What it enables |
|---|---|---|
| Transactional outbox | [db/postgres/migrations/0003_events.sql](../db/postgres/migrations/0003_events.sql) `change_event` | Append-only row written in the same transaction as the source change; Kafka-shaped fields (topic, key, payload, headers, BIGSERIAL id ≈ offset) |
| Per-consumer offsets | `consumer_cursor` table | Per-(consumer, topic) cursor; multiple consumers never block each other |
| Postgres triggers | `emit_change_event()` + variant for entity_state | `entity`, `relation`, `entity_state` INSERT/UPDATE/DELETE all produce events automatically; `pg_notify` fires the wake-up |
| Consumer framework | [web/events.go](../web/events.go) `EventConsumer` | LISTEN-based drain loop with 30s heartbeat fallback (missed NOTIFYs only delay processing); advance-only cursor commits |
| Reference consumer: `neo4j_reprojector` | [web/consumers.go](../web/consumers.go) | Incrementally MERGEs entity/relation/state changes into Neo4j; state mutations land with `state_` prefix so they stay visually distinct from source attrs |
| Startup goroutine wiring | `startConsumers(ctx)` in [web/main.go](../web/main.go) + [web/mcp.go](../web/mcp.go) | One `sync.Once`-guarded launch in both HTTP and MCP modes; no separate process to babysit |
| Kafka-migration shape | (see [docs/events-plan.md](events-plan.md)) | `change_event.{id,topic,key,payload,headers}` map 1:1 to Kafka record fields; flip via Debezium or a 50-line relay process when scale demands |

**End-to-end verified:** action_flag_for_review → entity_state UPDATE → change_event row → consumer drains → Neo4j node gets `state_flagged=true, state_flag_reason, state_flag_severity`. No manual `ontology project` needed.

**Known gap:** the Python bulk-load path (`ontology load`) doesn't yet use `SET LOCAL session_replication_role = replica` to bypass triggers during the 2.5M-row COPY, so re-running a full state load will fire 2.5M trigger executions and slow noticeably. Documented in [events-plan.md](events-plan.md); easy follow-up.

---

## What's planned but not built

| Plan | Doc | Status |
|---|---|---|
| Eval harness for LLM answer quality | (no plan doc yet) | Recommended |
| `pgvector` semantic entity resolution | (no plan doc yet) | Recommended |
| Tool-call telemetry table | (no plan doc yet) | Recommended |
| Idempotency keys on actions | (no plan doc yet) | Recommended |
| Batch action invocation | (no plan doc yet) | Recommended |
| `describe_capability` macro tool | (no plan doc yet) | Recommended |
| Hot-reload of metrics.yaml / actions.yaml | (no plan doc yet) | Recommended |
| Real `/healthz` with DB ping | (no plan doc yet) | Recommended |
| Read-only Postgres role for chatbot | (no plan doc yet) | Recommended |
| CI in GitHub Actions | (no plan doc yet) | Recommended |
| Nationwide + multi-year data | (no plan doc yet) | Recommended |
| Open Payments / NPPES integration | (no plan doc yet) | Recommended |
| Streaming LLM responses (SSE) | (no plan doc yet) | Recommended |
| Bulk-load trigger bypass on ETL | [events-plan.md](events-plan.md) | Partial — plan documented, Python helper not yet wired |
| Kafka transport migration | [events-plan.md](events-plan.md) | Plan only — flip when at least two of the trigger criteria become true |

---

## Numbers worth memorizing

- **114,815** entities · **2,679,499** relations (California 2023 only)
- **5** entity types · **4** relation types · **13** metrics × **7** dimensions × **9** named queries × **4** actions
- **7** core tools + **N** auto-generated per-action tools exposed to the LLM
- **13/13** cross-store consistency checks passing
- **~3,778** prompt-cached tokens per call after the first; ~10× cheaper on the cached portion
- **Postgres → Neo4j auto-sync latency: ~1 second** (LISTEN/NOTIFY wake-up + drain)
