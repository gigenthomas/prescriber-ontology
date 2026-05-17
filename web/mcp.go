package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// runMCP starts a stdio MCP server exposing the same tools the web chatbot uses.
// Logs go to stderr; stdout is reserved for JSON-RPC traffic.
func runMCP() {
	log.SetOutput(os.Stderr)

	ctx := context.Background()
	if err := initMCPDeps(ctx); err != nil {
		log.Fatalf("init: %v", err)
	}
	defer pgPool.Close()
	defer neoDriver.Close(ctx)

	loadQueryCatalog()
	if err := loadMetrics(); err != nil {
		log.Fatalf("metrics: %v", err)
	}
	if err := loadActions(); err != nil {
		log.Fatalf("actions: %v", err)
	}

	// Events tier: start consumers in MCP mode too. Multiple processes can
	// each run their own neo4j_reprojector — the cursor ensures they don't
	// duplicate work (one wins each event via the WHERE last_id < clause).
	startConsumers(ctx)

	s := server.NewMCPServer(
		"prescriber-ontology",
		"0.1.0",
		server.WithToolCapabilities(true),
		server.WithInstructions(mcpInstructions()),
	)

	readOnly := []mcp.ToolOption{
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(true),
	}

	// All read-side tools route through the shared dispatcher so telemetry
	// captures every call uniformly.
	s.AddTool(
		mcp.NewTool("list_queries",
			append(readOnly,
				mcp.WithDescription("List all available named Cypher queries with their descriptions and required parameters."),
			)...,
		),
		mcpDispatch("list_queries"),
	)

	s.AddTool(
		mcp.NewTool("describe_schema",
			append(readOnly,
				mcp.WithDescription("Return the controlled vocabulary (entity types, predicates, attributes) and live entity/relation counts."),
			)...,
		),
		mcpDispatch("describe_schema"),
	)

	s.AddTool(
		mcp.NewTool("run_query",
			append(readOnly,
				mcp.WithDescription("Run a named Cypher query from the catalog against Neo4j. Returns up to 100 rows as a JSON array. Use list_queries first to discover names and required parameters."),
				mcp.WithString("name",
					mcp.Required(),
					mcp.Description("Query name from the catalog (e.g. 'top_prescribers_by_claims', 'drug_top_prescribers').")),
				mcp.WithObject("params",
					mcp.Description("Parameter map. Keys depend on the query — see list_queries for each query's required parameters."),
				),
			)...,
		),
		mcpDispatch("run_query"),
	)

	s.AddTool(
		mcp.NewTool("search_entities",
			append(readOnly,
				mcp.WithDescription("Fuzzy-search entities by canonical_label using Postgres trigram similarity. Returns up to 'limit' entries with id, type, external_id, canonical_label, and similarity score. Use this to resolve names to exact identifiers before calling run_query or get_entity."),
				mcp.WithString("text",
					mcp.Required(),
					mcp.Description("Search text (case-insensitive).")),
				mcp.WithString("type",
					mcp.Description("Optional entity-type filter: Prescriber, Drug, GenericDrug, Specialty, Location.")),
				mcp.WithNumber("limit",
					mcp.Description("Max rows (default 10, max 50).")),
			)...,
		),
		mcpDispatch("search_entities"),
	)

	s.AddTool(
		mcp.NewTool("get_entity",
			append(readOnly,
				mcp.WithDescription("Fetch full details for one entity by external_id + type, including attrs JSON and direct neighborhood (counts of incoming/outgoing relations per predicate)."),
				mcp.WithString("external_id",
					mcp.Required(),
					mcp.Description("External identifier (NPI for Prescriber, brand name for Drug, etc.).")),
				mcp.WithString("type",
					mcp.Required(),
					mcp.Description("Entity type: Prescriber, Drug, GenericDrug, Specialty, or Location.")),
			)...,
		),
		mcpDispatch("get_entity"),
	)

	s.AddTool(
		mcp.NewTool("list_metrics",
			append(readOnly,
				mcp.WithDescription("List available metrics and dimensions for query_metric."),
			)...,
		),
		mcpDispatch("list_metrics"),
	)

	s.AddTool(
		mcp.NewTool("query_metric",
			append(readOnly,
				mcp.WithDescription("Compute a metric, optionally grouped by a dimension and/or filtered by dimension values. "+
					"Use list_metrics to discover available metrics and dimensions. "+
					"Returns rows ordered by metric value descending, plus the compiled Cypher for transparency."),
				mcp.WithString("metric",
					mcp.Required(),
					mcp.Description("Metric name (e.g. 'total_cost', 'total_claims', 'unique_prescribers').")),
				mcp.WithString("group_by",
					mcp.Description("Optional dimension name (e.g. 'specialty', 'drug', 'generic', 'city'). Omit for a single scalar.")),
				mcp.WithObject("filters",
					mcp.Description("Optional filter map: dimension_name -> exact value to match. Values are case-sensitive."),
				),
				mcp.WithNumber("limit",
					mcp.Description("Max rows to return (default 25, max 250).")),
			)...,
		),
		mcpDispatch("query_metric"),
	)

	// ── Actions (write-back) ──
	writeAction := []mcp.ToolOption{
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(true),
	}

	s.AddTool(
		mcp.NewTool("list_actions",
			append(readOnly,
				mcp.WithDescription("List available actions (write-back operations). Each action is also a tool named 'action_<name>'."),
			)...,
		),
		mcpDispatch("list_actions"),
	)

	s.AddTool(
		mcp.NewTool("entity_actions",
			append(readOnly,
				mcp.WithDescription("Return the recent action-invocation history for one entity plus its current entity_state."),
				mcp.WithString("external_id", mcp.Required(),
					mcp.Description("External identifier (NPI, brand name, etc.).")),
				mcp.WithString("type", mcp.Required(),
					mcp.Description("Entity type.")),
				mcp.WithNumber("limit",
					mcp.Description("Max invocations to return (default 25, max 100).")),
			)...,
		),
		mcpDispatch("entity_actions"),
	)

	s.AddTool(
		mcp.NewTool("entity_lineage",
			append(readOnly,
				mcp.WithDescription("Full lineage for one entity: identity, source dataset, pipeline runs that touched it, actions applied to it, current entity_state, and recent change_events. Use when asked 'where did this come from' or 'what's the history of this entity'."),
				mcp.WithString("external_id", mcp.Required(),
					mcp.Description("External identifier (NPI, brand name, etc.).")),
				mcp.WithString("type", mcp.Required(),
					mcp.Description("Entity type.")),
				mcp.WithNumber("event_limit",
					mcp.Description("Max recent change_events to return (default 25, max 200).")),
			)...,
		),
		mcpDispatch("entity_lineage"),
	)

	for _, name := range actionNames() {
		def := actionCfg.Actions[name]
		s.AddTool(buildMCPActionTool(name, def, writeAction), mcpDispatch("action_"+name))
	}

	log.Printf("prescriber-ontology MCP server starting (queries=%s, %d queries loaded)",
		queriesDir, len(queryCatalog))
	if err := server.ServeStdio(s); err != nil {
		log.Fatalf("stdio server: %v", err)
	}
}

// initMCPDeps initialises only what the MCP server needs (Postgres + Neo4j).
// Skips the Anthropic client and templates — the LLM is the consumer, not us.
func initMCPDeps(ctx context.Context) error {
	if err := initPostgres(ctx); err != nil {
		return err
	}
	if err := initNeo4j(ctx); err != nil {
		return err
	}
	return nil
}

func mcpInstructions() string {
	return `Read-only ontology of CMS Medicare Part D prescriber data (California, 2023).
Entities: Prescriber (NPI), Drug (brand), GenericDrug, Specialty, Location.
Relations: prescribed (with claim/cost/fill aggregates), generic_of, has_specialty, practices_in.

Workflow:
1. Call list_queries or describe_schema to see what's available.
2. For specific drugs / specialties / cities, call search_entities first to resolve the exact canonical_label (CMS data is case-sensitive).
3. Call run_query with the right query name and params, or get_entity for a single record.
Never invent numbers — only quote values returned by the tools.`
}

// ── MCP tool builders ───────────────────────────────────────────────────────
// Per-tool handler functions used to live here; they were replaced by
// mcpDispatch (in telemetry.go) which routes all calls through the shared
// dispatcher so telemetry only has to be wrapped once.

// buildMCPActionTool generates a per-action MCP tool from the action definition.
func buildMCPActionTool(name string, def actionDef, baseOpts []mcp.ToolOption) mcp.Tool {
	opts := append([]mcp.ToolOption{}, baseOpts...)

	desc := def.Description
	if def.TargetType != "any" {
		desc = fmt.Sprintf("%s Target type: %s.", desc, def.TargetType)
	}
	opts = append(opts, mcp.WithDescription(desc))

	opts = append(opts, mcp.WithString("external_id",
		mcp.Required(),
		mcp.Description(fmt.Sprintf(
			"External identifier of the target entity (NPI for Prescriber, brand name for Drug, etc.). Type=%s.",
			def.TargetType))))

	if def.TargetType == "any" {
		opts = append(opts, mcp.WithString("target_type",
			mcp.Required(),
			mcp.Description("Entity type: Prescriber, Drug, GenericDrug, Specialty, or Location.")))
	}

	for paramName, spec := range def.Params {
		desc := spec.Description
		switch spec.Type {
		case "enum":
			enumVals := make([]any, len(spec.Values))
			for i, v := range spec.Values {
				enumVals[i] = v
			}
			stringOpts := []mcp.PropertyOption{mcp.Description(desc), mcp.Enum(spec.Values...)}
			if spec.Required {
				stringOpts = append(stringOpts, mcp.Required())
			}
			opts = append(opts, mcp.WithString(paramName, stringOpts...))
		case "string":
			stringOpts := []mcp.PropertyOption{mcp.Description(desc)}
			if spec.Required {
				stringOpts = append(stringOpts, mcp.Required())
			}
			opts = append(opts, mcp.WithString(paramName, stringOpts...))
		case "integer", "number":
			numOpts := []mcp.PropertyOption{mcp.Description(desc)}
			if spec.Required {
				numOpts = append(numOpts, mcp.Required())
			}
			opts = append(opts, mcp.WithNumber(paramName, numOpts...))
		case "boolean":
			boolOpts := []mcp.PropertyOption{mcp.Description(desc)}
			if spec.Required {
				boolOpts = append(boolOpts, mcp.Required())
			}
			opts = append(opts, mcp.WithBoolean(paramName, boolOpts...))
		}
	}

	return mcp.NewTool("action_"+name, opts...)
}

// (mcpRunAction and toolResult were removed when handlers were unified
// through mcpDispatch in telemetry.go.)
