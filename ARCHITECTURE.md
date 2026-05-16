# Prescriber Knowledge Platform — Architecture Brief

**Date:** 2026-05-16
**Status:** Working prototype, end-to-end, on real public data

---

## What we built

A natural-language assistant that answers structured questions about prescribing
behavior across California's ~110,000 Medicare-billing providers, using the
official 2023 Centers for Medicare & Medicaid Services (CMS) Part D dataset.

Ask in plain English:

> *"Which specialties generate the most drug spend in California?"*
> *"Who prescribes the most Eliquis, and where do they practice?"*
> *"What does Cardiology prescribe most often?"*

The system returns specific numbers, attributes them to the source dataset, and
shows its reasoning trace so the answer is auditable.

---

## Why this is interesting

Healthcare data is **relational** (typed records, contracts, regulations) **and
also networked** (prescribers, drugs, payments, organizations, locations all
connect to each other). Most existing tools handle one or the other. This
platform handles both, and exposes them through a single conversational
interface that an analyst, executive, or auditor can use without writing SQL.

Concrete use cases:

- **Compliance** — surface unusual prescribing patterns by specialty or geography
- **Market intelligence** — identify top prescribers for a given drug or therapy class
- **Cost analysis** — rank specialties or geographies by drug spend
- **Network analysis** — find prescribers with similar prescribing profiles (co-prescribing)
- **Future** — overlay payments-from-pharma data (Open Payments) to detect potential conflicts of interest

The underlying design is **data-agnostic** — the same plumbing handles any
domain that has typed records plus rich relationships (claims, supply-chain,
research, fraud networks).

---

## How it works

```
┌─────────────────────────────────────────────────────────────┐
│ Web chat UI (HTMX, single-page)                             │
└──────────────────┬──────────────────────────────────────────┘
                   │
┌──────────────────┴──────────────────────────────────────────┐
│ Go application server                                       │
│  • Routes browser requests                                  │
│  • Maintains per-user conversation history                  │
│  • Runs the LLM agent loop                                  │
└─┬───────────────────────────────┬───────────────────────────┘
  │                               │
  │ AI reasoning                  │ Data access (canned tools)
  ▼                               ▼
┌────────────────────┐    ┌───────────────────────────────────┐
│ Anthropic Claude   │    │ PostgreSQL    │   Neo4j           │
│ (Sonnet 4.6)       │    │  System of    │   Graph view      │
│                    │    │  record       │                   │
│ Picks the right    │    │  • 114k       │   • Same entities │
│ tool, never writes │    │    entities   │     as nodes      │
│ raw queries        │    │  • 2.68M      │   • Edges between │
│                    │    │    relations  │     prescribers,  │
│                    │    │  • Full       │     drugs, etc.   │
│                    │    │    provenance │   • Fast network  │
│                    │    │               │     traversal     │
└────────────────────┘    └───────────────────────────────────┘
```

### The two databases play different roles

| | PostgreSQL | Neo4j |
|---|---|---|
| Role | System of record | Graph projection |
| Strengths | Typed records, validation, joins, provenance, transactions | Network traversal, multi-hop relationship queries, similarity |
| Authoritative? | Yes | No — rebuildable from Postgres |
| Used by the AI for | Precise lookups, fuzzy text search | Aggregations, relationship-heavy questions |

If Neo4j is ever lost or corrupted, it can be rebuilt from PostgreSQL in
~5 minutes by re-running a single command. No data is uniquely held there.

### How the AI assistant stays honest

The LLM is **not** allowed to write its own database queries. Instead it
chooses from a small set of pre-built, named tools — currently seven of them —
and decides which to call and what parameters to pass.

| Tool | Purpose |
|------|---------|
| `list_queries` | Discover what hand-crafted graph queries are available |
| `run_query` | Execute one of eight named graph queries (top prescribers, co-prescribing, etc.) |
| `list_metrics` | Discover what declarative metrics and dimensions are available |
| `query_metric` | Compose a metric × dimension × filters aggregation on the fly |
| `search_entities` | Fuzzy-match names → exact identifiers |
| `get_entity` | Fetch one entity with full provenance and neighborhood |
| `describe_schema` | Show what entity types and relationships exist |

This is the **safety boundary**. The LLM cannot do anything the platform
hasn't pre-authorized. There is no SQL injection vector and no risk of an
expensive runaway query. Every answer is grounded in actual database output,
which the conversation surfaces so the user can verify it.

---

## The semantic layer: metrics and dimensions

Behind the tool surface sits a single YAML file — [metrics.yaml](metrics.yaml) —
that declares the platform's vocabulary of **metrics** (what to measure) and
**dimensions** (how to slice). A small compiler in the application assembles
these definitions into Cypher at query time.

### Why this layer matters

Before the semantic layer, every new aggregation question (*"top 10 specialties
by senior spend"*, *"average cost per claim by city"*) required a developer to
write a new `.cypher` file. Now those questions are composed at runtime from a
shared vocabulary.

Three concrete consequences:

- **Adding a metric is editing one block of YAML.** No new Go, no new Cypher,
  no rebuild. Restart the server and the LLM sees it.
- **Pivot-table thinking.** 13 metrics × 7 dimensions = 91 distinct aggregation
  shapes the LLM can already compose. Each new metric multiplies into every
  dimension and vice versa.
- **One source of truth for definitions.** When compliance, finance, and
  clinical ops all say *"total spend"*, they are summing the same column the
  same way — the definition lives in `metrics.yaml` and is reviewable.

### How a request compiles to Cypher

When the LLM calls
`query_metric(metric="total_cost", group_by="specialty", filters={drug: "Eliquis"})`,
the compiler:

1. Starts from the **base pattern**:
   `MATCH (p:Prescriber)-[r:prescribed]->(d:Drug)`
2. Adds **dimension MATCH clauses** for any referenced dimension that isn't
   already in the base — e.g. `MATCH (p)-[:has_specialty]->(s:Specialty)`
3. Adds **filter WHERE clauses** — e.g. `WHERE d.canonical_label = $f_drug`
4. Emits the **RETURN clause** with the metric expression, the group-by
   value, and a deterministic ordering

The compiled Cypher is returned to the caller alongside the data, so an analyst
or auditor can see exactly what ran. There is no hidden translation step.

### What's currently declared

**13 metrics** — `total_cost`, `total_claims`, `total_beneficiaries`,
`total_30day_fills`, `total_day_supply`, `prescription_count`,
`unique_prescribers`, `unique_drugs`, `avg_cost_per_claim`, `senior_cost`,
`senior_claims`, `avg_cost_per_beneficiary`, `avg_day_supply_per_claim`.

**7 dimensions** — `drug`, `generic`, `specialty`, `city`, `prescriber`,
`name_length` (demo of derived CASE expressions), `brand_vs_generic` (compares
Brnd_Name to Gnrc_Name with punctuation normalization).

A new metric is typically 3 lines of YAML; a new dimension is 4–6 lines.
Anyone with read access to the repo can propose one — no programming required.

---

## The strategic shift: agents as the primary consumer

The chatbot in front of this system is **one consumer**. The actual product is
the tool surface over the data layer. The same five tools that power the
chatbot — `list_queries`, `run_query`, `search_entities`, `get_entity`,
`describe_schema` — can be exposed to any AI agent, internal or third-party.

We are not building a chatbot with a database behind it. We are building a
**data layer designed to be consumed by AI agents**, with one chatbot as the
first reference consumer.

### What this gives us

Each property we designed into the platform pays off across many agents, not
just one:

| Property | Why agents need it |
|---|---|
| **Named tools, not raw queries** | A safety boundary that scales — every agent uses the same vetted interface. |
| **Self-describing schema** (`describe_schema`) | A new agent can learn the data model at runtime instead of being hard-coded. |
| **Provenance per fact** | Agents can cite sources, which is what makes their answers trustworthy. |
| **Auditable tool traces** | Every action an agent takes is logged; you can replay what it did and why. |

### Natural next step: expose the tool surface to the agent ecosystem

The dominant industry standard for letting AI agents call external tools is
the **Model Context Protocol (MCP)**, supported by Claude Desktop, Claude Code,
and a growing list of agent frameworks. Packaging this tool surface as an MCP
server is roughly a day of work and immediately unlocks:

- **Claude Desktop users** can ask questions about California prescribers from inside their normal Claude UI, no separate app needed
- **Internal agents** (for compliance review, market analysis, fraud triage) can use the same tools without each team rebuilding access to the data
- **Multi-agent workflows** — one agent can call another that calls this data layer, with the audit trail preserved end-to-end
- **Future agent frameworks** — whatever comes next likely speaks MCP

Strategically, this turns the platform from "an app with a chatbot" into "a
data layer that exposes itself to the agent ecosystem." The chatbot remains a
useful reference consumer; the layer itself outlives whichever frontends come
and go.

---

## What's in the data today

- **Source:** CMS Medicare Part D Prescribers by Provider and Drug — 2023 release
- **Scope:** California only (~2.5 million prescriber-drug aggregate rows)
- **Entities:** 114,815 — broken down as 110,430 prescribers, 2,412 drug brands, 897 generic substances, 130 specialties, 946 cities
- **Relationships:** 2,679,499 — prescribed (with claim counts and costs), generic-of, has-specialty, practices-in
- **Integrity:** All 13 cross-store consistency checks pass

Adding additional years, additional states, or different datasets (NPPES
provider registry, Open Payments pharma payments) is mechanical — the
schema already carries a provenance tag on every fact, so datasets co-exist
without colliding.

---

## Cost and operational profile

| Dimension | Today |
|---|---|
| Infrastructure | Two databases running locally via Docker (PostgreSQL 16, Neo4j 5). Self-hostable on any cloud. |
| Storage | ~5 GB total (raw CSV cached, plus database files) |
| Cold start to working system | ~30 minutes (one-time data load + projection) |
| AI cost per question | Approximately $0.01–$0.05 in API calls, depending on how many tools the LLM chains |
| Latency per question | 3–15 seconds end-to-end |
| Scale ceiling at this design point | Tens of millions of rows per dataset, hundreds of concurrent users |

The platform is built with off-the-shelf, open-source software (PostgreSQL, Neo4j,
Go, Python) and Anthropic's Claude API. No proprietary lock-in.

---

## Already shipped

- **Declarative semantic layer** — 13 metrics × 7 dimensions in `metrics.yaml`, no code required to add more.
- **MCP server** — the same five-plus-two tool surface is now reachable by any Model Context Protocol client (Claude Desktop, Claude Code, internal agents) by running `prescriber-bot.exe -mcp`.

## Decisions on the table

1. **Expand data coverage** — Move from California-only to nationwide, add prior years for trend analysis, or add the NPPES registry for organizational affiliations.
2. **Add payments data** — Layer in CMS Open Payments (pharma-to-prescriber payments) for conflict-of-interest analysis.
3. **Productionize the chat interface** — Authentication, audit logging, multi-user session isolation, deployment to managed cloud.
4. **Semantic entity resolution** — Add `pgvector` embeddings on entity labels so the LLM can find *"the cardiologist who prescribes blood thinners"* without an exact name.
5. **Domain extension** — Apply the same pattern to a different vertical (claims, supply chain, research). The infrastructure is reusable.

---

## Appendix: trust posture

Each fact in the database carries a `source_id` pointing back to the exact
dataset it came from. When the assistant cites a number, that number can be
traced to a specific row in a specific CMS file. The conversation trace shows
every tool the LLM called and what it received back, so an auditor can verify
both *what* was answered and *how*.

The LLM is constrained to nine pre-built tools and cannot execute arbitrary
queries against the databases. Database credentials are not exposed to the
model.
