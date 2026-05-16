# Prescriber Knowledge Platform — Architecture Brief

**Prepared for:** Executive leadership
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
chooses from a small set of pre-built, named tools — currently nine of them —
and decides which to call and what parameters to pass.

| Tool | Purpose |
|------|---------|
| `list_queries` | Discover what canned questions are available |
| `run_query` | Execute one of eight named graph queries (ancestors, top prescribers, co-prescribing, etc.) |
| `search_entities` | Fuzzy-match names → exact identifiers |
| `get_entity` | Fetch one entity with full provenance and neighborhood |
| `describe_schema` | Show what entity types and relationships exist |

This is the **safety boundary**. The LLM cannot do anything the platform
hasn't pre-authorized. There is no SQL injection vector and no risk of an
expensive runaway query. Every answer is grounded in actual database output,
which the conversation surfaces so the user can verify it.

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

## Decisions on the table

1. **Expand data coverage** — Move from California-only to nationwide, add prior years for trend analysis, or add the NPPES registry for organizational affiliations.
2. **Add payments data** — Layer in CMS Open Payments (pharma-to-prescriber payments) for conflict-of-interest analysis.
3. **Productionize the chat interface** — Authentication, audit logging, multi-user session isolation, deployment to managed cloud.
4. **Domain extension** — Apply the same pattern to a different vertical (claims, supply chain, research). The infrastructure is reusable.

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
