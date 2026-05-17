# prescriber-ontology

A hybrid PostgreSQL + Neo4j ontology over CMS Medicare Part D prescriber data,
exposed to AI agents through a tool-using LLM chatbot and a Model Context
Protocol (MCP) server.

> *"Top 5 cardiology drugs by cost?"*
> → Eliquis $406M, Xarelto $141M, Entresto $131M, Vyndamax $63M, Jardiance $60M

> *"Which specialties have the longest day-supply per claim?"*
> → Peripheral Vascular Disease (77.8) · Acupuncturist (74.6) · Interventional Cardiology (72.1)

For the executive-level narrative, see [ARCHITECTURE.md](ARCHITECTURE.md).

---

## What it is

- **A canonical entity-relation model** in PostgreSQL with provenance on every fact
  (`source_id` per row).
- **A graph projection** of the same data in Neo4j for fast relationship traversal.
- **A declarative semantic layer** in [metrics.yaml](metrics.yaml) — 13 metrics × 7 dimensions,
  compiled into Cypher at query time.
- **Two interfaces:**
  - A web chat UI (HTMX + Go + Anthropic SDK)
  - An MCP server exposing the same tools to any MCP-compatible agent

The LLM never writes raw SQL or Cypher — it picks from seven named tools
(`list_queries`, `run_query`, `list_metrics`, `query_metric`, `search_entities`,
`get_entity`, `describe_schema`). That's the safety boundary.

## Currently loaded

- **Dataset:** CMS Medicare Part D Prescribers by Provider and Drug — 2023 release
- **Scope:** California (~2.5 M source rows)
- **Entities:** 114,815 (110,430 prescribers · 2,412 drug brands · 897 generics · 130 specialties · 946 cities)
- **Relations:** 2,679,499 (`prescribed`, `generic_of`, `has_specialty`, `practices_in`)

Other states, other years, or other datasets (NPPES, Open Payments) can be
loaded under the same model — the `source_id` provenance keeps facts
attributable.

---

## Stack

| Layer | Tech |
|---|---|
| Canonical store | PostgreSQL 16 (`pg_trgm`, `pgcrypto`, JSONB) |
| Graph projection | Neo4j 5 + APOC |
| ETL | Python 3.11 (`psycopg`, `httpx`, `lxml`, `tqdm`, `typer`) |
| Web chatbot | Go 1.22 + Anthropic Go SDK + HTMX |
| Agent transport | Model Context Protocol over stdio (`mark3labs/mcp-go`) |
| LLM | Claude Sonnet 4.6 (configurable) |
| Auth (optional) | Keycloak (OIDC) + OPA (Rego policies) |
| Containers | Docker Compose (Postgres + Neo4j + Keycloak + OPA) |

---

## Prerequisites

- Docker Desktop (running)
- Python ≥ 3.11
- Go ≥ 1.22
- An Anthropic API key — get one at https://console.anthropic.com

Connectivity expectations:

- Inbound TCP `5432` (Postgres), `7474` + `7687` (Neo4j), `8080`/`8081` (chatbot), and — if auth is enabled — `8180` (Keycloak admin/login UI) and `8181` (OPA decision API)
- Outbound HTTPS to `data.cms.gov` (one-time ~3.8 GB download) and `api.anthropic.com`

---

## Quick start

```powershell
git clone https://github.com/gigenthomas/prescriber-ontology.git
cd prescriber-ontology

# 1. Configure environment
Copy-Item .env.example .env
# Edit .env and set ANTHROPIC_API_KEY=sk-ant-...

# 2. Start Postgres + Neo4j (Postgres applies migrations on first boot)
docker compose up -d

# 3. Python: venv, install, init Neo4j constraints
python -m venv .venv
.\.venv\Scripts\Activate.ps1
pip install -e .
ontology init

# 4. Download + ingest CMS Part D 2023 (California, ~5 min total)
ontology load --year 2023 --state CA

# 5. Project canonical store -> Neo4j graph (~10 min for 2.5M edges)
ontology project

# 6. Verify both stores are consistent (13 checks)
ontology verify

# 7. Build and start the chatbot on http://localhost:8080
cd web
go build -o ../prescriber-bot.exe .
cd ..
.\prescriber-bot.exe
```

Open http://localhost:8080 and ask away.

---

## Repo layout

```
.
├── ARCHITECTURE.md            Executive-level brief
├── README.md                  This file
├── docker-compose.yml         Postgres 16 + Neo4j 5 (with APOC)
├── metrics.yaml               Declarative metrics + dimensions
│
├── db/
│   ├── postgres/migrations/   Auto-applied on first Postgres boot
│   └── neo4j/init/            Constraints applied by `ontology init`
│
├── src/ontology/              Python ETL package
│   ├── cli.py                 `ontology` Typer CLI entry point
│   ├── config.py              pydantic-settings (reads .env)
│   ├── db.py                  Postgres pool + Neo4j driver
│   ├── verify.py              Sanity + cross-store consistency checks
│   ├── ingest/prescriber/     Fetch CMS CSV → COPY into staging → derive entities + relations
│   ├── ingest/mesh/           Older MeSH loader (kept for reference)
│   └── project/to_neo4j.py    Postgres → Neo4j projector (idempotent MERGE)
│
├── queries/                   Hand-written .cypher files used by run_query
│
└── web/                       Go application
    ├── main.go                HTTP chatbot mode (default)
    ├── mcp.go                 MCP stdio server mode (-mcp flag)
    ├── metrics.go             metrics.yaml compiler
    └── tools.go               Tool handlers (Postgres + Neo4j)
```

---

## CLI commands

```powershell
ontology init                          # Apply Neo4j constraints
ontology reset --yes                   # Wipe all data from both stores
ontology fetch                         # Just download the CSV
ontology load --year 2023 --state CA   # Full ingest pipeline
ontology project                       # Project Postgres -> Neo4j
ontology stats                         # Entity/relation counts in both stores
ontology verify [--scope postgres|neo4j|all]
ontology list-queries                  # List available .cypher queries
ontology query <name> [-p key=value]   # Run a named query directly
```

## Web chatbot

```powershell
# Default port 8080
.\prescriber-bot.exe

# Custom port via ADDR env
$env:ADDR=":8081"; .\prescriber-bot.exe

# Override model
$env:ANTHROPIC_MODEL="claude-opus-4-7"; .\prescriber-bot.exe
```

The UI shows every tool call inline as a yellow trace so you can see *how* the
LLM is reasoning, not just the final answer.

## MCP server

Same binary, different mode. Reads JSON-RPC on stdin, writes on stdout, logs to stderr.

```powershell
.\prescriber-bot.exe -mcp
```

### Wiring into Claude Desktop

Edit `%APPDATA%\Claude\claude_desktop_config.json`:

```json
{
  "mcpServers": {
    "prescriber-ontology": {
      "command": "C:\\Dev\\ontology\\prescriber-bot.exe",
      "args": ["-mcp"],
      "env": {
        "ONTOLOGY_QUERIES_DIR": "C:\\Dev\\ontology\\queries",
        "ONTOLOGY_METRICS_FILE": "C:\\Dev\\ontology\\metrics.yaml",
        "POSTGRES_USER": "ontology",
        "POSTGRES_PASSWORD": "ontology",
        "POSTGRES_DB": "ontology",
        "POSTGRES_HOST": "localhost",
        "POSTGRES_PORT": "5432",
        "NEO4J_URI": "bolt://localhost:7687",
        "NEO4J_USER": "neo4j",
        "NEO4J_PASSWORD": "ontology-dev"
      }
    }
  }
}
```

Restart Claude Desktop. Tools become available in any conversation.

### Wiring into Claude Code

```powershell
claude mcp add prescriber-ontology -- C:\Dev\ontology\prescriber-bot.exe -mcp
```

---

## Extending the semantic layer

Both metrics and dimensions live in [metrics.yaml](metrics.yaml). No code, no
rebuild — edit, save, restart the server.

**Add a metric** (just a Cypher aggregation expression):

```yaml
metrics:
  senior_share_pct:
    description: Percentage of cost driven by 65+ beneficiaries
    cypher: "100.0 * sum(toFloat(coalesce(r.ge65_tot_drug_cst, 0))) / nullif(sum(toFloat(coalesce(r.tot_drug_cst, 0))), 0)"
```

**Add a dimension** (an `expression` and optionally a `match` clause that brings
its entity into scope; the base pattern always has `p:Prescriber`,
`r:prescribed`, `d:Drug`):

```yaml
dimensions:
  state:
    description: US state of practice
    match: "(p)-[:practices_in]->(:Location)-[:in_state]->(st:State)"
    expression: "st.code"
```

Smoke-test before restarting the chatbot:

```powershell
'{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"s","version":"0"}}}
{"jsonrpc":"2.0","method":"notifications/initialized"}
{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"query_metric","arguments":{"metric":"senior_share_pct","group_by":"specialty","limit":5}}}' `
| .\prescriber-bot.exe -mcp
```

The response includes the compiled Cypher under `cypher` for inspection.

## Adding a named graph query

Drop a `.cypher` file into `queries/`. The first `//` comment block becomes the
description; `$param` references in the body become declared parameters. The
catalog is rebuilt on each chatbot restart.

```cypher
// Drugs frequently co-prescribed with a target drug.
// "Co-prescribed" means the same prescriber prescribed both.
// Params: $brand
MATCH (target:Drug {external_id: $brand})<-[:prescribed]-(p:Prescriber)-[:prescribed]->(other:Drug)
WHERE other <> target
RETURN other.canonical_label AS brand, count(DISTINCT p) AS co_prescribers
ORDER BY co_prescribers DESC LIMIT 25;
```

---

## Inspecting the data

```powershell
# psql in the running container
docker compose exec postgres psql -U ontology -d ontology

# Then, inside psql:
\dt
SELECT type, count(*) FROM entity GROUP BY type;
SELECT * FROM entity WHERE canonical_label = 'Aspirin' LIMIT 1;
```

Neo4j browser: http://localhost:7474 (user `neo4j`, password `ontology-dev`).

---

## Trust posture

- Every fact carries a `source_id` traceable to a single CMS file.
- The LLM cannot execute arbitrary queries — it picks from canned tools.
- Every tool call is visible in the chat UI as a trace line.
- `ontology verify` runs 13 sanity + cross-store consistency checks; CI can
  gate merges on it.
- Database credentials are never exposed to the LLM (only to the Go process
  via environment variables).

## License

Personal / educational use of public CMS data. Not affiliated with CMS or NLM.
