# Demo script

A 20–30 minute walkthrough showing what the platform does. Builds up from
simple Q&A to write-back via Actions. Skip any section you don't need.

---

## Preflight (do this before the audience arrives)

```powershell
# 1. Containers up + healthy
docker compose ps                       # expect both postgres and neo4j healthy

# 2. Data + cross-store consistency check
ontology verify                         # expect "13/13 checks passed"

# 3. Chatbot reachable
curl http://localhost:8081/healthz      # expect "ok"

# 4. Fresh slate for the action demos (optional — wipes prior demo state)
docker compose exec -T postgres psql -U ontology -d ontology -c "
  TRUNCATE entity_state;
  DELETE FROM action_invocation;"

# 5. Pin a known-good CA NPI as a backup target for the Actions act
docker compose exec -T postgres psql -U ontology -d ontology -t -A -c "
  SELECT external_id, canonical_label, attrs->>'city' AS city, attrs->>'specialty' AS spec
  FROM entity WHERE type='Prescriber' AND attrs->>'specialty'='Cardiology'
  ORDER BY canonical_label LIMIT 3"
```

Open two side-by-side windows:

- **Left:** the chatbot at http://localhost:8081 (open in a private window so
  the conversation starts fresh)
- **Right:** a `psql` shell into the Postgres container — useful for showing
  the audit log lands in real time

---

## Act 1 — "It answers natural-language questions about real data"

**Goal:** show that the LLM is grounded — no hallucinations, real numbers.

| Prompt | What the audience sees |
|--------|------------------------|
| *"How many prescribers do we have in California, and which specialties are most common?"* | Concrete numbers (110k prescribers, top specialties by count). The yellow trace shows `describe_schema` + `query_metric(unique_prescribers, group_by=specialty)`. |
| *"Top 5 specialties by total drug cost"* | Internal Medicine $4.31B, Family Practice $2.60B, etc. Trace shows `query_metric(metric=total_cost, group_by=specialty, limit=5)`. |
| *"Who prescribes the most Eliquis?"* | Top 5 prescribers with names + claim counts. Trace shows `search_entities("Eliquis")` then `query_metric(...filters={drug: "Eliquis"})`. |

> **Point to make:** "Notice the yellow trace lines — the LLM tells us *exactly*
> which tool it called and what it got back. No hidden translation, no guessing
> at numbers. If you want to verify, the answer is auditable."

---

## Act 2 — "It composes metrics on the fly"

**Goal:** show that the semantic layer means new aggregation questions don't
need new code.

| Prompt | What the audience sees |
|--------|------------------------|
| *"What's the senior spend share by specialty? Top 5."* | Uses `senior_cost` metric, group by specialty. Cardiology / Hem-Onc / Endocrinology dominate — clinically sensible. |
| *"Average day-supply per claim by specialty — which specialties are most chronic?"* | Peripheral Vascular Disease, Interventional Cardiology, etc. at ~70+ days. Surfaces chronic vs acute pattern. |
| *"How does branded vs generic spend break down in California?"* | Single split: branded ~$16.2B vs generic_only ~$0.2B. Uses the `brand_vs_generic` derived dimension. |

> **Point to make:** "These three questions used three different metrics
> grouped by three different dimensions — and nobody wrote any Cypher.
> Open [metrics.yaml](../metrics.yaml) and the audience sees the entire
> vocabulary: 13 metrics × 7 dimensions = 91 question shapes."

---

## Act 3 — "It's a real graph — multi-hop reasoning"

**Goal:** show the network-shaped questions that classical SQL would
struggle with.

| Prompt | What the audience sees |
|--------|------------------------|
| *"Which drugs are most commonly co-prescribed with Eliquis?"* | Uses the `co_prescribed` Cypher query. Top results: Atorvastatin, Metformin, Lisinopril — exactly the comorbidity profile cardiologists target. |
| *"Which generic substances have the most distinct brand names mapped to them?"* | Multi-brand generics like Dextroamphetamine/Amphetamine. The trace shows `run_query(brands_per_generic)`. |
| *"What city has the most prescribers, and what do they prescribe most?"* | Two-step chain: `query_metric(unique_prescribers, group_by=city, limit=1)` → `query_metric(total_cost, group_by=drug, filters={city:"..."})`. |

> **Open the Neo4j browser** at http://localhost:7474 if there's time — paste
> any `.cypher` file from [queries/](../queries/) and let the audience see the
> raw network visualization.

---

## Act 4 — "It's not read-only — it operates on the data"

**This is the climactic demo.** Up to here we've only read. Now we write.

**Setup the scenario:** *"Suppose compliance has asked us to flag any cardiologist whose prescribing pattern looks unusual for review."*

| Step | Prompt | What the audience sees |
|------|--------|------------------------|
| 1 | *"List the actions you can perform."* | LLM calls `list_actions` — surfaces `flag_for_review`, `unflag`, `add_to_watchlist`, `add_note`. |
| 2 | *"Find a cardiologist in San Francisco who heavily prescribes Eliquis."* | LLM chains `search_entities` and `query_metric(filters={drug:Eliquis, city:'SAN FRANCISCO', specialty:Cardiology})`. Returns a specific NPI + name. |
| 3 | *"Flag NPI <copied-from-step-2> for review. Reason: 'top-5 Eliquis prescriber in San Francisco — routine compliance check'. Severity: low."* | Trace shows `action_flag_for_review(...)` — returns an `invocation_id` and `state_updates: {flagged: true, ...}`. |
| 4 | **Switch to psql** | `SELECT * FROM action_invocation ORDER BY invoked_at DESC LIMIT 1;` — show the row Just Got Written. |
| 5 | **Stay in psql** | `SELECT e.canonical_label, es.state FROM entity_state es JOIN entity e ON e.id = es.entity_id;` — show the entity state reflects the flag. |
| 6 | **Back in chat** | *"Show me the action history for that NPI."* — LLM calls `entity_actions`, returns the invocation list + current state. |
| 7 | *"Clear the flag — turns out the pattern is normal."* | `action_unflag` applied. Show psql again: state now `{flagged: false, ...}`. The invocation is in the audit log forever. |

> **Point to make:** "Every action is parameterized, type-checked, transactional
> (audit row and state update commit together), and visible in the trace. The
> LLM can't write SQL; it can only call these named tools. And every change
> is permanently audited."

### Failure-mode demo (~30 seconds)

To prove the safety boundary:

> *"Flag that same prescriber for review with severity 'nuclear'."*

The trace shows `action_flag_for_review(severity='nuclear')` → error:
*"param severity must be one of [low medium high], got 'nuclear'"*.

In psql:

```sql
SELECT action_name, status, error_msg FROM action_invocation
WHERE status='rejected' ORDER BY invoked_at DESC LIMIT 1;
```

Even the rejected attempt is in the audit log. Nothing slips past.

---

## Act 5 — "It's MCP-native"

**Goal:** show that the platform is not "an app with a chatbot" but "a data
layer agents consume."

If Claude Desktop is configured (see [README.md](../README.md) for the
config snippet), open it side-by-side with the web chat and ask the same
question in both:

> *"What's the senior spend share for Cardiology in CA?"*

Both produce the same answer because both are hitting the same MCP tool
surface. The web chat is one consumer; Claude Desktop is another. The
underlying ontology, metrics, and actions are shared.

> **Point to make:** "Tomorrow, when a new agent framework comes out — if it
> speaks MCP, it gets these tools for free. Today we have one chatbot. The
> platform is built to outlive whichever frontend wins."

---

## Closing — the platform tour (~5 minutes)

Open these files in order and narrate:

1. **[metrics.yaml](../metrics.yaml)** — *"The entire question vocabulary.
   13 metrics, 7 dimensions, declarative. Add a new metric, restart, the LLM
   sees it instantly."*

2. **[actions.yaml](../actions.yaml)** — *"The write-back vocabulary. Type
   checks, default values, audit-required flag. Same edit-restart pattern."*

3. **[queries/](../queries/)** — *"Hand-crafted Cypher for the questions
   that don't fit the metric pattern. The LLM picks among these via
   `run_query`."*

4. **[docs/events-plan.md](events-plan.md)** — *"The next layer: automatic
   propagation of changes, with a Kafka migration path documented up front."*

5. **[ARCHITECTURE.md](../ARCHITECTURE.md)** — *"The full executive
   briefing if you want to send it to someone who wasn't here."*

---

## After the demo — cleanup

```powershell
# Wipe the demo actions so the slate is clean for next time
docker compose exec -T postgres psql -U ontology -d ontology -c "
  TRUNCATE entity_state;
  DELETE FROM action_invocation;"

# Optionally stop the chatbot
# (find the task in your terminal session and Ctrl+C, or kill the process)
```

If you need to reset *everything* — Postgres + Neo4j data plus the schema —
the canonical command is `ontology reset --yes`, then re-run the
[Quick Start](../README.md#quick-start) from step 5.

---

## Sample prompts that always look good

Keep these in your back pocket — they consistently produce clean, interesting
answers regardless of which CA prescriber you pick.

- *"What's the most expensive specialty in California and what drives the cost?"* — two-step: top specialty by `total_cost`, then top drugs filtered to that specialty
- *"Are there prescribers in San Francisco with unusually high day-supply per claim?"* — composes `avg_day_supply_per_claim` with `city` filter and `prescriber` group-by
- *"Which oncology drugs have the highest average cost per beneficiary?"* — `avg_cost_per_beneficiary` filtered to `specialty: "Hematology-Oncology"`, grouped by `drug`
- *"Flag every prescriber in this list of NPIs for review with reason 'Q2 compliance batch'"* — demonstrates batched Actions across many entities
- *"Show me everything you can do to a Drug."* — surfaces `add_to_watchlist`, `add_note`, and any future Drug-targeted Action without you having to name them
