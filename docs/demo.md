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

# 3a. (Act 6 only) start the chatbot in auth mode and confirm Keycloak + OPA
#     are healthy. Skip this if you're only demoing acts 1–5.
docker compose ps                       # also expect keycloak + opa healthy
$env:AUTH_PROVIDER="keycloak"; $env:MCP_SERVICE_ROLES="compliance"
.\prescriber-bot.exe                    # logs should show "auth=keycloak" + "opa=enabled"

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

Open these windows side by side:

- **Left:** the chatbot at http://localhost:8081 (open in a private window so
  the conversation starts fresh)
- **Right (Act 4):** the Actions log at http://localhost:8081/actions
  — refresh after each action and the new row appears at the top
- **Optional terminal:** a `psql` shell into the Postgres container — useful
  if someone in the audience wants to see the raw rows behind the UI

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

**Setup the scene:** open http://localhost:8081/actions in the right window.
Empty (or near-empty) table — point out the summary cards at the top:
*Total invocations*, *Applied*, *Rejected*, *Entities with state*.

**Frame the scenario:** *"Suppose compliance has asked us to flag any
cardiologist whose prescribing pattern looks unusual for review."*

| Step | Prompt | What the audience sees |
|------|--------|------------------------|
| 1 | *"List the actions you can perform."* | LLM calls `list_actions` — surfaces `flag_for_review`, `unflag`, `add_to_watchlist`, `add_note`. |
| 2 | *"Find a cardiologist in San Francisco who heavily prescribes Eliquis."* | LLM chains `search_entities` and `query_metric(filters={drug:Eliquis, city:'SAN FRANCISCO', specialty:Cardiology})`. Returns a specific NPI + name. |
| 3 | *"Flag NPI <copied-from-step-2> for review. Reason: 'top-5 Eliquis prescriber in San Francisco — routine compliance check'. Severity: low."* | Chat trace: `action_flag_for_review(...)` → `invocation_id` + `state_updates: {flagged: true, ...}`. |
| 4 | **Refresh the Actions log** in the right window | Top row is the action that just happened — params, target name, the resulting state JSON, status badge `applied`. The "Total invocations" counter ticked up. |
| 5 | **In chat** | *"Show me the action history for that NPI."* — LLM calls `entity_actions`, returns the invocation list + current state. The chat trace and the UI agree. |
| 6 | *"Clear the flag — turns out the pattern is normal."* | `action_unflag` applied. Refresh the Actions log: a second row appears for the same target, state column now `{flagged: false, flag_reason: null, ...}`. The earlier flag row stays — append-only audit. |
| 7 | **In the Actions UI**, click the `target_type` dropdown → `Prescriber` and Apply | Filtered view of only Prescriber-targeting actions. Show the filter chain works (also try `status=rejected` once Step 8 fires). |

> **Point to make:** "Every action is parameterized, type-checked, transactional
> (audit row and state update commit together), visible in the chat trace, AND
> visible in the audit UI. The LLM can't write SQL; it can only call these
> named tools. Every change — and every rejected attempt — is permanently
> recorded."

### Step 8: Failure-mode demo (~30 seconds)

To prove the safety boundary:

> *"Flag that same prescriber for review with severity 'nuclear'."*

The chat trace shows `action_flag_for_review(severity='nuclear')` → error:
*"param severity must be one of [low medium high], got 'nuclear'"*.

**Refresh the Actions log.** A new row appears with a red `rejected` badge and
the error message inline. Filter by `status=rejected` to show only failures.
Even invalid attempts are part of the permanent record — nothing slips past.

> **For the audience that asks "where does this actually live?"** open a
> terminal and run:
> ```
> docker compose exec -T postgres psql -U ontology -d ontology -c \
>   "SELECT action_name, status, error_msg FROM action_invocation
>    ORDER BY invoked_at DESC LIMIT 3"
> ```
> Same data the UI is showing — just the underlying table.

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

## Act 6 — "Every call is gated by policy" (optional, auth mode only)

**Goal:** show that identity and authorization are first-class. The same tool
surface that powers everything above is filtered by a policy engine that
sees *who* is calling and *what they're trying to do* — and the decision is
logged the same way every other tool call is.

**Prerequisite:** Acts 1–5 ran in default (anonymous) mode. To run this act,
restart with `AUTH_PROVIDER=keycloak` (see preflight step 3a) and open the
chatbot in a fresh private window so it doesn't hold a stale cookie.

**Setup the scene:** the chat URL now redirects to Keycloak. Log in once as
`alice` (password `alice-dev`) so the audience sees the OIDC flow once. The
header pill now reads **Alice (analyst) · logout**.

**Frame the scenario:** *"Same four reference actions as Act 4, but now the
platform knows who's asking. Watch what changes."*

| Step | Who | Prompt | What the audience sees |
|------|-----|--------|------------------------|
| 1 | **alice** (analyst) | *"Top 5 specialties by total drug cost"* | Same answer as Act 1. Read tools are open to analyst+. Trace shows `query_metric` with `policy: allow`. |
| 2 | **alice** | *"Flag NPI \<from-Act-4\> for review, severity low, reason 'routine check'"* | Trace shows a red **[DENIED]** line: *"denied: flag_for_review (severity=low) requires compliance; user has [analyst]"*. The action never executes. |
| 3 | **alice → logout**, **bob** login (`bob-dev`) | *"Flag NPI \<same\> for review, severity low, reason 'routine check'"* | `action_flag_for_review` succeeds. `action_invocation.actor` row now contains Bob's Keycloak subject UUID — not `agent:claude`. |
| 4 | **bob** (compliance) | *"Escalate that to severity high"* (i.e. re-flag with `severity=high`) | Trace shows **[DENIED]**: *"severity=high requires senior_compliance; user has [compliance]"*. The escalation gate works. |
| 5 | **bob → logout**, **carol** login (`carol-dev`) | *"Flag NPI \<same\> for review, severity high, reason 'pattern unusual for specialty'"* | Succeeds. Carol's UUID is now on the invocation. The high-severity gate opens for senior_compliance. |
| 6 | Anywhere | Open http://localhost:8081/telemetry | New **Denied by policy** summary card is non-zero. Filter `Policy = denied` shows steps 2 and 4 with their full Rego reasons inline. Filter `Actor = <alice-subject>` isolates her trace. |

> **Point to make:** "Three things stayed identical between Acts 4 and 6 —
> the tool surface, the actions library, the audit log. The only thing
> that changed is that the policy engine now sits between the LLM and the
> database, and it can see who's asking. The denial isn't a 500 error in a
> log file somewhere — it's a first-class chat-trace entry with the exact
> reason, and it's queryable in the telemetry UI like any other call."

### Optional: same policy, different transport

If Claude Desktop is wired in via MCP (Act 5), the **service-account roles**
in `MCP_SERVICE_ROLES` determine what *that* transport can do. With
`MCP_SERVICE_ROLES=compliance` (the dev default), the MCP agent has Bob's
permissions: it can flag low/medium but not high. Show the same severity=high
request via Claude Desktop and it gets denied with the same Rego reason as
step 4 — proving that web chat and MCP share one decision path, not two.

---

## Closing — the platform tour (~5 minutes)

Open these in order and narrate:

1. **http://localhost:8081/actions** — *"This is the audit surface for
   everything the platform writes. Built into the same chatbot binary; takes
   a filter on action, status, target type, or external_id."*

2. **[metrics.yaml](../metrics.yaml)** — *"The entire read-side question
   vocabulary. 13 metrics, 7 dimensions, declarative. Add a new metric,
   restart, the LLM sees it instantly."*

3. **[actions.yaml](../actions.yaml)** — *"The write-back vocabulary. Type
   checks, default values, $-substituted state updates. Same edit-restart
   pattern. Adding `escalate_to_legal` is a four-line YAML change."*

4. **[queries/](../queries/)** — *"Hand-crafted Cypher for the questions
   that don't fit the metric pattern. The LLM picks among these via
   `run_query`."*

5. **[docs/events-plan.md](events-plan.md)** — *"The next layer: automatic
   propagation of changes from Postgres to Neo4j and beyond, with a Kafka
   migration path documented up front."*

6. **[docs/auth-plan.md](auth-plan.md)** — *"How the identity + policy layer
   in Act 6 is wired: Keycloak issues OIDC tokens, OPA evaluates Rego on
   every tool call, the decision is cached and logged. Phased delivery
   doc — useful if you want to lift this pattern into another project."*

7. **[ARCHITECTURE.md](../ARCHITECTURE.md)** — *"The full executive
   briefing if you want to send it to someone who wasn't here."*

---

## After the demo — cleanup

Before wiping, the Actions log page is a good record to screenshot if you
want to keep evidence of what was demonstrated.

```powershell
# Wipe the demo actions so the slate is clean for next time
docker compose exec -T postgres psql -U ontology -d ontology -c "
  TRUNCATE entity_state;
  DELETE FROM action_invocation;"
# Refresh http://localhost:8081/actions — all counters should drop to zero.

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

---

## Appendix: auth v1 verification walkthrough

A self-check protocol, separate from the audience-facing Act 6. Run this
end-to-end any time you change a policy file, touch the auth code, or want
to confirm the verification checklist in [auth-plan.md](auth-plan.md) is
still green. ~10 minutes.

### Step 0 — bring up the full stack in auth mode

```powershell
docker compose up -d
docker compose ps                       # postgres, neo4j, keycloak, opa all "healthy"

# Kill any earlier bot run first, then:
$env:AUTH_PROVIDER="keycloak"
$env:MCP_SERVICE_ROLES="compliance"
.\prescriber-bot.exe
```

Expect in the startup log:
- `auth: provider=keycloak issuer=http://localhost:8180/realms/ontology-dev …`
- `opa: enabled url=http://localhost:8181/v1/data/…`

### Step 1 — unauthenticated redirect

Open `http://localhost:8081/` in a **fresh private window**. Should bounce
to Keycloak's `realms/ontology-dev/protocol/openid-connect/auth?...`.

### Step 2 — alice (analyst) — read OK, action denied

Log in: `alice` / `alice-dev`. Header pill: **alice (analyst) · logout**.

| Prompt | Expect |
|---|---|
| *"Top 5 specialties by total drug cost"* | Real numbers. Trace shows `query_metric` with no DENY badge. |
| *"Flag NPI 1427277136 for review, severity low, reason 'routine check'"* | Trace: `[DENIED] action_flag_for_review` with reason `requires compliance; user has [analyst]`. |

Logout — confirm bounce through Keycloak end-session and back to `/`.

### Step 3 — bob (compliance) — low OK, high denied

Log in: `bob` / `bob-dev`. Header pill: **bob (compliance)**.

| Prompt | Expect |
|---|---|
| *"Flag NPI 1427277136 for review, severity low, reason 'routine check'"* | Applied. `invocation_id` returned. |
| *"Re-flag the same NPI with severity high, reason 'pattern unusual'"* | `[DENIED]` with reason `severity=high requires senior_compliance; user has [compliance]`. |

Logout.

### Step 4 — carol (senior_compliance) — high OK

Log in: `carol` / `carol-dev`. Header pill: **carol (senior_compliance)**.

| Prompt | Expect |
|---|---|
| *"Flag NPI 1427277136 for review, severity high, reason 'pattern unusual for specialty'"* | Applied. |

### Step 5 — verify the audit surface

`http://localhost:8081/telemetry` (still logged in as carol):

- **Denied by policy** summary card ≥ 2 (alice's flag + bob's high-flag).
- **Actor filter dropdown** lists real names (`alice`, `bob`, `carol`) plus the synthetic labels. *(Phase 4a behavior — names not UUIDs.)*
- Filter `Policy = denied` → two rows, each with the full Rego reason inline.
- Filter `Actor = alice (…)` → only alice's calls.
- Reset filters — *Actor* column shows **names**, not UUIDs.

`http://localhost:8081/actions`:

- Three rows for NPI 1427277136 — alice's (rejected), bob's (applied low), carol's (applied high).
- Actor column shows **names**, not UUIDs.

### Step 6 — regression sanity

```powershell
ontology verify                         # expect 13/13 still passing
```

### Step 7 — restart in anonymous mode (clean up)

```powershell
# Ctrl+C to stop the bot, then:
Remove-Item Env:\AUTH_PROVIDER
Remove-Item Env:\MCP_SERVICE_ROLES
.\prescriber-bot.exe                    # back to default anonymous mode
```

`http://localhost:8081/` should now load directly into chat — no Keycloak bounce.

### What "pass" means

Every row in the table above ticked, plus the regression check at Step 6.
Anything off — a missing display name, a wrong policy reason, a row that
didn't appear in /actions — is a regression in either the policy file, the
materializer JOIN, or the dispatch wiring. Start by checking the chat
trace's reason string against the matching `reason :=` rule in
[auth/policies/actions.rego](../auth/policies/actions.rego).

---

## Neo4j Querirs 
http://localhost:7474/browser/

MATCH (p:Prescriber {external_id: "1023069424"})
OPTIONAL MATCH (p)-[r]-(other)
RETURN p, r, other