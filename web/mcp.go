package main

import (
	"context"
	"errors"
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

	s := server.NewMCPServer(
		"prescriber-ontology",
		"0.1.0",
		server.WithToolCapabilities(true),
		server.WithInstructions(mcpInstructions()),
	)

	s.AddTool(
		mcp.NewTool("list_queries",
			mcp.WithDescription("List all available named Cypher queries with their descriptions and required parameters."),
		),
		mcpListQueries,
	)

	s.AddTool(
		mcp.NewTool("describe_schema",
			mcp.WithDescription("Return the controlled vocabulary (entity types, predicates, attributes) and live entity/relation counts."),
		),
		mcpDescribeSchema,
	)

	s.AddTool(
		mcp.NewTool("run_query",
			mcp.WithDescription("Run a named Cypher query from the catalog against Neo4j. Returns up to 100 rows as a JSON array. Use list_queries first to discover names and required parameters."),
			mcp.WithString("name",
				mcp.Required(),
				mcp.Description("Query name from the catalog (e.g. 'top_prescribers_by_claims', 'drug_top_prescribers').")),
			mcp.WithObject("params",
				mcp.Description("Parameter map. Keys depend on the query — see list_queries for each query's required parameters."),
			),
		),
		mcpRunQuery,
	)

	s.AddTool(
		mcp.NewTool("search_entities",
			mcp.WithDescription("Fuzzy-search entities by canonical_label using Postgres trigram similarity. Returns up to 'limit' entries with id, type, external_id, canonical_label, and similarity score. Use this to resolve names to exact identifiers before calling run_query or get_entity."),
			mcp.WithString("text",
				mcp.Required(),
				mcp.Description("Search text (case-insensitive).")),
			mcp.WithString("type",
				mcp.Description("Optional entity-type filter: Prescriber, Drug, GenericDrug, Specialty, Location.")),
			mcp.WithNumber("limit",
				mcp.Description("Max rows (default 10, max 50).")),
		),
		mcpSearchEntities,
	)

	s.AddTool(
		mcp.NewTool("get_entity",
			mcp.WithDescription("Fetch full details for one entity by external_id + type, including attrs JSON and direct neighborhood (counts of incoming/outgoing relations per predicate)."),
			mcp.WithString("external_id",
				mcp.Required(),
				mcp.Description("External identifier (NPI for Prescriber, brand name for Drug, etc.).")),
			mcp.WithString("type",
				mcp.Required(),
				mcp.Description("Entity type: Prescriber, Drug, GenericDrug, Specialty, or Location.")),
		),
		mcpGetEntity,
	)

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

// ── tool handlers ───────────────────────────────────────────────────────────

func mcpListQueries(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	out, err := doListQueries()
	return toolResult(out, err)
}

func mcpDescribeSchema(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	out, err := doDescribeSchema(ctx)
	return toolResult(out, err)
}

func mcpRunQuery(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name, err := req.RequireString("name")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	params := map[string]any{}
	if raw, ok := req.GetArguments()["params"].(map[string]any); ok {
		params = raw
	}
	out, err := doRunQuery(ctx, name, params)
	return toolResult(out, err)
}

func mcpSearchEntities(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	text, err := req.RequireString("text")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	entityType := req.GetString("type", "")
	limit := req.GetInt("limit", 10)
	out, err := doSearchEntities(ctx, text, entityType, limit)
	return toolResult(out, err)
}

func mcpGetEntity(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	externalID, err := req.RequireString("external_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	entityType, err := req.RequireString("type")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	out, err := doGetEntity(ctx, externalID, entityType)
	return toolResult(out, err)
}

func toolResult(out string, err error) (*mcp.CallToolResult, error) {
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if out == "" {
		return mcp.NewToolResultError("empty result"), errors.New("empty result")
	}
	return mcp.NewToolResultText(out), nil
}
