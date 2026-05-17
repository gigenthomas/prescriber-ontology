# Architecture diagrams

Visual companions to [features.md](features.md) and [../ARCHITECTURE.md](../ARCHITECTURE.md).
GitHub renders Mermaid natively — open this file in the GitHub UI to see the
diagrams.

---

## 1 · System architecture (with feature mapping)

Every box is a component that exists in the repo today. Labels in italics
are the features that component delivers; numbers in `§N` reference the
[features.md](features.md) sections.

```mermaid
flowchart TB
    %% ─────────────────────────────── External
    subgraph EXT["External"]
        CMS["CMS Part D CSV<br/>data.cms.gov"]
        USER["End user<br/>(web browser)"]
        CD["Claude Desktop<br/>/ Claude Code<br/>(MCP client)"]
        AGENTS["Other<br/>MCP-compatible<br/>agents"]
    end

    %% ─────────────────────────────── Config (YAML)
    subgraph CFG["Declarative config (§4, §5)"]
        MYAML["<b>metrics.yaml</b><br/><i>13 metrics × 7 dimensions</i>"]
        AYAML["<b>actions.yaml</b><br/><i>4 reference actions</i>"]
        QDIR["<b>queries/*.cypher</b><br/><i>9 named queries</i>"]
    end

    %% ─────────────────────────────── ETL
    subgraph ETL["ETL pipeline · Python (§2)"]
        FETCH["fetch.py<br/><i>streaming CSV download<br/>with content-type validation</i>"]
        STAGE["load.py<br/><i>state filter + Postgres COPY<br/>~40k rows/sec</i>"]
        SQL["sql.py<br/><i>9 SQL passes to derive<br/>5 entity types + 4 predicates</i>"]
        PROJ["to_neo4j.py<br/><i>idempotent MERGE projection</i>"]
        VER["verify.py<br/><i>13 sanity + cross-store checks</i>"]
    end

    %% ─────────────────────────────── Storage
    subgraph STORE["Canonical storage (§1)"]
        PG[("<b>PostgreSQL 16</b><br/>entity / relation / entity_label<br/>source / schema_term<br/>entity_state / action_invocation<br/>mup_dpr_staging")]
        NEO[("<b>Neo4j 5</b><br/>:Entity + per-type labels<br/>typed relationships<br/>flattened attrs as native props")]
    end

    %% ─────────────────────────────── Go application binary
    subgraph APP["Go application binary · web/* (§6, §7, §8)"]
        MCOMP["metric compiler<br/><i>web/metrics.go</i>"]
        AEXEC["action executor<br/><i>web/actions.go</i><br/><i>transactional audit+state</i>"]
        QRUN["query runner<br/><i>web/tools.go</i>"]
        TOOLS{{"<b>Tool surface</b><br/>list_queries · run_query<br/>list_metrics · query_metric<br/>list_actions · entity_actions<br/>search_entities · get_entity<br/>describe_schema<br/>+ N auto-generated action_*"}}
        AGENT["agent loop<br/><i>web/main.go runAgent</i><br/><i>up to 12 tool-use rounds</i><br/><i>prompt-cached prefix</i>"]
        HTTP["HTTP transport<br/><i>web/main.go runHTTP</i>"]
        MCP["MCP stdio transport<br/><i>web/mcp.go runMCP</i>"]
        CHATUI["Chat UI · /<br/><i>HTMX + tool trace</i>"]
        ACTUI["Actions audit UI · /actions<br/><i>filters · status badges<br/>summary cards</i>"]
    end

    %% ─────────────────────────────── LLM
    subgraph LLM["LLM (§6)"]
        CLAUDE["<b>Anthropic Claude</b><br/>Sonnet 4.6 (default)<br/>prompt caching enabled"]
    end

    %% ─────────────────────────────── Ingest flow
    CMS --> FETCH --> STAGE --> SQL --> PG
    PG -- "<i>project()</i>" --> PROJ --> NEO
    PG <-.-> VER
    NEO <-.-> VER

    %% ─────────────────────────────── Config feeds compilers
    MYAML --> MCOMP
    AYAML --> AEXEC
    QDIR  --> QRUN

    %% ─────────────────────────────── Compilers reach the right store
    MCOMP --> NEO
    QRUN  --> NEO
    AEXEC --> PG

    %% ─────────────────────────────── Tool surface
    MCOMP -. registered as .-> TOOLS
    AEXEC -. registered as .-> TOOLS
    QRUN  -. registered as .-> TOOLS

    %% ─────────────────────────────── Agent loop uses tools
    AGENT <--> TOOLS
    AGENT <-->|"prompt-cached<br/>system + tools"| CLAUDE

    %% ─────────────────────────────── Transports drive the agent
    HTTP --> AGENT
    MCP  --> AGENT

    %% ─────────────────────────────── UIs served by HTTP
    HTTP --> CHATUI
    HTTP --> ACTUI
    ACTUI -- "reads audit log" --> PG

    %% ─────────────────────────────── External clients
    USER   --> HTTP
    CD     --> MCP
    AGENTS --> MCP

    %% ─────────────────────────────── Styling
    classDef store fill:#fff3cd,stroke:#856404,stroke-width:1.5px,color:#000
    classDef config fill:#f3eaff,stroke:#5a3e8c,stroke-width:1.5px,color:#000
    classDef etl fill:#e3f0ff,stroke:#1d4ed8,stroke-width:1.5px,color:#000
    classDef app fill:#e7f7ec,stroke:#196e2a,stroke-width:1.5px,color:#000
    classDef llm fill:#fde6e6,stroke:#9b1c1c,stroke-width:1.5px,color:#000
    classDef ext fill:#f5f5f5,stroke:#444,stroke-width:1.2px,color:#000

    class PG,NEO store
    class MYAML,AYAML,QDIR config
    class FETCH,STAGE,SQL,PROJ,VER etl
    class MCOMP,AEXEC,QRUN,AGENT,HTTP,MCP,CHATUI,ACTUI,TOOLS app
    class CLAUDE llm
    class CMS,USER,CD,AGENTS ext
```

### How to read it

- **Yellow** boxes are the two databases — the only persistent state.
- **Purple** boxes are declarative configuration files. Editing them and
  restarting the binary changes capability without code.
- **Blue** boxes are the Python ETL pipeline — runs out-of-band, not in the
  request path.
- **Green** boxes all live in one Go binary. The same compiled artifact runs
  as either an HTTP server (`./prescriber-bot.exe`) or an MCP stdio server
  (`./prescriber-bot.exe -mcp`) depending on a flag.
- **Red** is the only external dependency that costs money per call.
- The `Tool surface` hexagon is the **safety boundary** — agents cannot
  bypass it.

---

## 2 · A chatbot turn end-to-end

This is the sequence inside one user message. Most calls hit the prompt cache
and skip the expensive re-encoding of system + tools.

```mermaid
sequenceDiagram
    autonumber
    participant U as User (browser)
    participant H as HTTP server
    participant A as Agent loop
    participant C as Claude (Anthropic)
    participant T as Tool handler
    participant DB as Postgres / Neo4j

    U->>H: POST /chat (message + session cookie)
    H->>A: append user msg to session history
    loop tool-use rounds (≤ 12)
        A->>C: Messages.New<br/>(cached system + tools, fresh messages)
        C-->>A: response (text and/or tool_use blocks)
        alt response includes tool_use
            A->>T: dispatch by tool name
            T->>DB: SQL or Cypher
            DB-->>T: rows
            T-->>A: JSON result string
            A->>A: append tool_result to messages
        else end_turn
            A-->>H: final text + tool trace
        end
    end
    H-->>U: HTML chunk (user msg + traces + bot msg)
    Note over H,DB: Actions UI at /actions reads<br/>action_invocation any time
```

**Cost note:** steps 4–5 reuse the cached prefix (~3.8k tokens at ~10×
cheaper). Only the deltas — recent messages, tool results — pay full rate.

---

## 3 · Component → feature map

A table you can scan vertically. Cross-reference with [features.md](features.md)
for one-line value statements.

| Component | File(s) | Features delivered |
|---|---|---|
| Postgres schema | `db/postgres/migrations/0001_init.sql` | §1 entity/relation/source/schema_term, provenance |
| Postgres actions schema | `db/postgres/migrations/0002_actions.sql` | §5 entity_state + action_invocation tables |
| Docker compose stack | `docker-compose.yml` | §10 local Postgres + Neo4j + APOC |
| Python config | `src/ontology/config.py` | §10 env-driven, .env loading |
| Python DB clients | `src/ontology/db.py` | §1, §2 connection pooling for both stores |
| MeSH ingest | `src/ontology/ingest/mesh/*` | §2 alternate-dataset reference |
| Prescriber ingest | `src/ontology/ingest/prescriber/*` | §2 streaming load, COPY, derivation |
| Neo4j projector | `src/ontology/project/to_neo4j.py` | §1 idempotent MERGE, attr flattening |
| Verify suite | `src/ontology/verify.py` | §3 13 checks, pass/fail exit code |
| CLI entry point | `src/ontology/cli.py` | §10 init, load, project, verify, query |
| Metrics config | `metrics.yaml` | §4 13 metrics, 7 dimensions |
| Metric compiler | `web/metrics.go` | §4 Cypher composition, params |
| Hand-written queries | `queries/*.cypher` | §4 9 named queries |
| Query runner | `web/tools.go` | §4 run_query dispatch |
| Actions config | `actions.yaml` | §5 4 reference actions |
| Action executor | `web/actions.go` | §5 validation, substitution, transactional apply |
| Web chatbot | `web/main.go` runHTTP | §6 HTMX UI, session state, prompt cache |
| Agent loop | `web/main.go` runAgent | §6 tool-use loop, usage logging |
| MCP server | `web/mcp.go` runMCP | §7 stdio transport, tool mirror, instructions |
| Actions UI | `web/actions_ui.go` + `web/templates/actions.html` | §8 audit log browser |
| Chat template | `web/templates/index.html` | §6 chat input + tool trace render |
| Smoke test seed | (manual via MCP stdio) | §3 end-to-end check |

---

## 4 · What this diagram does NOT show

These exist as plans, not as code yet — see [features.md](features.md)
"planned but not built" section.

- **Events tier** (`change_event` + LISTEN/NOTIFY consumer) — would sit
  between Postgres and Neo4j, replacing the manual `project` arrow
- **`pgvector`** index on `entity.canonical_label` — would wire into
  `search_entities` for semantic resolution
- **Tool-call telemetry** — a `tool_call_log` table fed by the agent loop
- **Eval harness** — a fixture file + runner that exercises the tool surface
  and reports pass/fail
- **Idempotency keys** — a new column on `action_invocation`
- **Result caching** — TTL'd LRU between the agent and the metric compiler

Adding any of these is additive to the diagram, not a redesign.
