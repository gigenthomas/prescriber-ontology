package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	goldmarkhtml "github.com/yuin/goldmark/renderer/html"
)

//go:embed templates/*.html
var templatesFS embed.FS

var (
	anthropicClient anthropic.Client
	pgPool          *pgxpool.Pool
	neoDriver       neo4j.DriverWithContext
	tpl             *template.Template

	queriesDir = getenv("ONTOLOGY_QUERIES_DIR", "./queries")
	model      = getenv("ANTHROPIC_MODEL", "claude-sonnet-4-6")

	queryCatalog  []queryInfo
	tools         []anthropic.ToolUnionParam
	systemPrompt  string
	sessions      sync.Map
	maxToolRounds = 12
)

type queryInfo struct {
	Name        string
	Description string
	Params      []string
}

func main() {
	mcpMode := flag.Bool("mcp", false, "Run as a Model Context Protocol stdio server instead of the HTTP chatbot")
	flag.Parse()

	_ = godotenv.Load(".env")
	_ = godotenv.Load("../.env")

	if *mcpMode {
		runMCP()
		return
	}
	runHTTP()
}

func runHTTP() {
	ctx := context.Background()
	if err := initDeps(ctx); err != nil {
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
	buildSystemPrompt()
	buildTools()

	// Events tier: start the in-process consumer goroutine. It owns its own
	// pgx connection for LISTEN; the rest of the app uses the pool.
	startConsumers(ctx)

	var err error
	tpl, err = template.ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		log.Fatalf("parse templates: %v", err)
	}

	http.HandleFunc("/", indexHandler)
	http.HandleFunc("/chat", chatHandler)
	http.HandleFunc("/actions", actionsHandler)
	http.HandleFunc("/telemetry", telemetryHandler)
	http.HandleFunc("/lineage", lineageHandler)
	http.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })

	addr := getenv("ADDR", ":8080")
	log.Printf("prescriber bot listening on %s (model=%s, queries=%s, %d queries loaded)",
		addr, model, queriesDir, len(queryCatalog))
	log.Fatal(http.ListenAndServe(addr, nil))
}

func initDeps(ctx context.Context) error {
	if err := initAnthropic(); err != nil {
		return err
	}
	if err := initPostgres(ctx); err != nil {
		return err
	}
	return initNeo4j(ctx)
}

func initAnthropic() error {
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		return errors.New("ANTHROPIC_API_KEY is not set")
	}
	anthropicClient = anthropic.NewClient(option.WithAPIKey(os.Getenv("ANTHROPIC_API_KEY")))
	return nil
}

func initPostgres(ctx context.Context) error {
	pool, err := pgxpool.New(ctx, pgDSNFromEnv())
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("postgres ping: %w", err)
	}
	pgPool = pool
	return nil
}

func initNeo4j(ctx context.Context) error {
	driver, err := neo4j.NewDriverWithContext(
		getenv("NEO4J_URI", "bolt://localhost:7687"),
		neo4j.BasicAuth(
			getenv("NEO4J_USER", "neo4j"),
			getenv("NEO4J_PASSWORD", "ontology-dev"),
			"",
		),
	)
	if err != nil {
		return fmt.Errorf("neo4j: %w", err)
	}
	if err := driver.VerifyConnectivity(ctx); err != nil {
		return fmt.Errorf("neo4j verify: %w", err)
	}
	neoDriver = driver
	return nil
}

func pgDSNFromEnv() string {
	return fmt.Sprintf("postgresql://%s:%s@%s:%s/%s",
		getenv("POSTGRES_USER", "ontology"),
		getenv("POSTGRES_PASSWORD", "ontology"),
		getenv("POSTGRES_HOST", "localhost"),
		getenv("POSTGRES_PORT", "5432"),
		getenv("POSTGRES_DB", "ontology"),
	)
}

func loadQueryCatalog() {
	entries, err := os.ReadDir(queriesDir)
	if err != nil {
		log.Printf("warn: queries dir %s: %v", queriesDir, err)
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".cypher") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".cypher")
		desc, params := parseCypherHeader(filepath.Join(queriesDir, e.Name()))
		queryCatalog = append(queryCatalog, queryInfo{
			Name:        name,
			Description: desc,
			Params:      params,
		})
	}
	sort.Slice(queryCatalog, func(i, j int) bool { return queryCatalog[i].Name < queryCatalog[j].Name })
}

var paramRefRe = regexp.MustCompile(`\$([A-Za-z_][A-Za-z0-9_]*)`)

// parseCypherHeader returns (description, parameter-names).
// description is the joined leading `//` comment block. parameters are the distinct
// `$identifier` references found anywhere in the file body.
func parseCypherHeader(path string) (string, []string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", nil
	}
	body := string(b)

	var descLines []string
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			descLines = append(descLines, strings.TrimSpace(strings.TrimPrefix(trimmed, "//")))
			continue
		}
		if trimmed == "" && len(descLines) == 0 {
			continue
		}
		break
	}

	seen := map[string]struct{}{}
	var params []string
	for _, m := range paramRefRe.FindAllStringSubmatch(body, -1) {
		if _, dup := seen[m[1]]; dup {
			continue
		}
		seen[m[1]] = struct{}{}
		params = append(params, m[1])
	}
	return strings.Join(descLines, " "), params
}

func buildSystemPrompt() {
	var b strings.Builder
	b.WriteString(`You are Prescriber Bot. You answer questions about California's 2023 CMS Medicare Part D prescriber-by-drug data using a hybrid PostgreSQL + Neo4j ontology.

The graph contains:
- Entity types: Prescriber (NPI), Drug (brand name), GenericDrug, Specialty, Location (city)
- Relations:
  - prescribed: Prescriber -> Drug, with attrs tot_clms, tot_30day_fills, tot_day_suply, tot_drug_cst, tot_benes (and ge65_* variants for 65+ beneficiaries)
  - generic_of: Drug -> GenericDrug
  - has_specialty: Prescriber -> Specialty
  - practices_in: Prescriber -> Location

Rules:
1. NEVER make up numbers. Use the tools to fetch data, then quote those numbers exactly.
2. Drug and Specialty names in CMS data are case-sensitive (e.g. "Eliquis", "Cardiology"). When unsure of an exact spelling, use search_entities first.
3. Prefer a named query from the catalog below over ad-hoc reasoning. Use run_query when one fits.
4. Be concise. Use markdown tables for comparisons. Cite NPIs and dollar amounts directly from tool output.
5. If a question cannot be answered with available tools, say so plainly.

Available queries (use with run_query). Each query's required parameters are listed in [brackets].
If a parameter is shown, you MUST pass it in the params map, e.g. run_query(name="drug_top_prescribers", params={"brand": "Eliquis"}).

`)
	for _, q := range queryCatalog {
		params := "no params"
		if len(q.Params) > 0 {
			params = "params: " + strings.Join(q.Params, ", ")
		}
		fmt.Fprintf(&b, "- %s [%s]: %s\n", q.Name, params, q.Description)
	}
	b.WriteString(`
If a tool call returns an error like "Expected parameter(s): X", retry the same query with that parameter included.
For drug brand names, specialty names, or city names you're unsure of, use search_entities first to find the exact spelling — CMS data is case-sensitive.

For aggregation questions ("top N by X", "total Y by Z", "average cost per claim grouped by..."), prefer query_metric over run_query. It composes a metric, an optional group_by dimension, and optional filters into a single query.

Available metrics:
`)
	for _, n := range metricNames() {
		fmt.Fprintf(&b, "- %s: %s\n", n, metricCfg.Metrics[n].Description)
	}
	b.WriteString("\nAvailable dimensions (use as group_by or filter keys):\n")
	for _, n := range dimensionNames() {
		fmt.Fprintf(&b, "- %s: %s\n", n, metricCfg.Dimensions[n].Description)
	}
	b.WriteString(`
Examples:
- query_metric(metric="total_cost", group_by="specialty", limit=10) -> top 10 specialties by drug cost
- query_metric(metric="total_claims", group_by="prescriber", filters={"drug":"Eliquis"}, limit=5) -> top 5 prescribers of Eliquis by claims
- query_metric(metric="unique_drugs", group_by="specialty") -> drug variety per specialty
- query_metric(metric="total_cost", filters={"city":"SAN FRANCISCO"}) -> total spend in San Francisco (single scalar)

`)
	if len(actionCfg.Actions) > 0 {
		b.WriteString("Write-back actions available (each is its own tool named action_<name>):\n")
		for _, n := range actionNames() {
			d := actionCfg.Actions[n]
			b.WriteString(fmt.Sprintf("- action_%s (target=%s): %s\n", n, d.TargetType, strings.SplitN(d.Description, "\n", 2)[0]))
		}
		b.WriteString(`
When applying a consequential action (flag, watchlist, etc.):
1. Resolve the target with search_entities to confirm the exact external_id.
2. State your reasoning in the response BEFORE invoking the action.
3. After the action returns, summarize what was applied including the invocation_id.
4. Use entity_actions(external_id, type) to inspect prior history if relevant.
Use list_actions for the full parameter schema of any action.
`)
	}
	systemPrompt = b.String()
}

func buildTools() {
	toolParams := []anthropic.ToolParam{
		{
			Name:        "list_queries",
			Description: anthropic.String("List all available named Cypher queries with their descriptions and parameters."),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties: map[string]any{},
			},
		},
		{
			Name: "run_query",
			Description: anthropic.String(
				"Run a named Cypher query from the catalog against Neo4j. " +
					"Returns up to 100 rows as a JSON array. Use list_queries to discover names. " +
					"params is a map of parameter name to value (string or number) as documented in each query's header comment."),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties: map[string]any{
					"name": map[string]any{
						"type":        "string",
						"description": "Query name (e.g. 'top_prescribers_by_claims', 'drug_top_prescribers').",
					},
					"params": map[string]any{
						"type":        "object",
						"description": "Parameters to pass into the Cypher query. Keys depend on the query.",
						"additionalProperties": true,
					},
				},
				Required: []string{"name"},
			},
		},
		{
			Name: "search_entities",
			Description: anthropic.String(
				"Find entities by name or concept. Two modes:\n" +
					"  mode='trigram' (default) — Postgres trigram similarity on canonical_label. " +
					"Best for known spellings of drug brand names, prescriber names, specialties, or cities (e.g. 'Eliquis').\n" +
					"  mode='semantic' — OpenAI embedding cosine distance. " +
					"Best for conceptual searches (e.g. 'blood thinner', 'heart specialist in San Francisco') where the exact name isn't known.\n" +
					"Returns up to 'limit' results with type, external_id, canonical_label, and a similarity score."),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties: map[string]any{
					"text": map[string]any{
						"type":        "string",
						"description": "Search text. For semantic mode, can be a natural-language description.",
					},
					"type": map[string]any{
						"type":        "string",
						"description": "Optional entity type filter: Prescriber, Drug, GenericDrug, Specialty, Location.",
					},
					"mode": map[string]any{
						"type":        "string",
						"description": "'trigram' (default) for exact-substring fuzzy match, 'semantic' for embedding-based conceptual match.",
						"enum":        []string{"trigram", "semantic"},
					},
					"limit": map[string]any{
						"type":        "integer",
						"description": "Max rows (default 10, max 50).",
					},
				},
				Required: []string{"text"},
			},
		},
		{
			Name: "get_entity",
			Description: anthropic.String(
				"Fetch full details for one entity by its external_id and type, including its attrs JSON " +
					"and direct neighborhood (counts of incoming/outgoing relations per predicate)."),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties: map[string]any{
					"external_id": map[string]any{
						"type":        "string",
						"description": "External identifier (e.g. an NPI for Prescriber, brand name for Drug).",
					},
					"type": map[string]any{
						"type":        "string",
						"description": "Entity type.",
					},
				},
				Required: []string{"external_id", "type"},
			},
		},
		{
			Name: "describe_schema",
			Description: anthropic.String(
				"Return the controlled vocabulary: registered entity types, predicates, attribute names, and live counts per type."),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties: map[string]any{},
			},
		},
		{
			Name:        "list_metrics",
			Description: anthropic.String("List available metrics and dimensions for query_metric."),
			InputSchema: anthropic.ToolInputSchemaParam{Properties: map[string]any{}},
		},
		{
			Name: "query_metric",
			Description: anthropic.String(
				"Compute a metric, optionally grouped by a dimension and/or filtered by dimension values. " +
					"Use list_metrics to see available metrics and dimensions. " +
					"Returns rows ordered by metric value descending. " +
					"Examples: " +
					"query_metric(metric='total_cost', group_by='specialty', limit=10); " +
					"query_metric(metric='total_claims', group_by='prescriber', filters={'drug':'Eliquis'}, limit=5)."),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties: map[string]any{
					"metric": map[string]any{
						"type":        "string",
						"description": "Metric name from list_metrics (e.g. 'total_cost', 'total_claims').",
					},
					"group_by": map[string]any{
						"type":        "string",
						"description": "Optional dimension name to group by (e.g. 'specialty', 'drug', 'generic', 'city'). Omit for a single scalar.",
					},
					"filters": map[string]any{
						"type":                 "object",
						"description":          "Optional filter map: dimension_name -> exact value to match. Values are case-sensitive.",
						"additionalProperties": map[string]any{"type": "string"},
					},
					"limit": map[string]any{
						"type":        "integer",
						"description": "Max rows to return (default 25, max 250).",
					},
				},
				Required: []string{"metric"},
			},
		},
		{
			Name:        "list_actions",
			Description: anthropic.String("List available actions (write-back operations) with their parameter schemas and target types. Each action becomes its own tool named 'action_<name>'."),
			InputSchema: anthropic.ToolInputSchemaParam{Properties: map[string]any{}},
		},
		{
			Name: "entity_actions",
			Description: anthropic.String(
				"Return the recent action-invocation history for one entity, plus its current entity_state. " +
					"Use to inspect what's been done to an entity and the resulting state."),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties: map[string]any{
					"external_id": map[string]any{
						"type":        "string",
						"description": "External identifier (NPI for Prescriber, brand name for Drug, etc.).",
					},
					"type": map[string]any{
						"type":        "string",
						"description": "Entity type.",
					},
					"limit": map[string]any{
						"type":        "integer",
						"description": "Max invocations to return (default 25, max 100).",
					},
				},
				Required: []string{"external_id", "type"},
			},
		},
		{
			Name: "entity_lineage",
			Description: anthropic.String(
				"Return the full lineage for one entity: identity, source dataset, pipeline runs that " +
					"touched it, actions applied to it, current entity_state, and recent change_events. " +
					"Use when asked 'where did this come from' or 'what's the history of this entity'."),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties: map[string]any{
					"external_id": map[string]any{
						"type":        "string",
						"description": "External identifier (NPI for Prescriber, brand name for Drug, etc.).",
					},
					"type": map[string]any{
						"type":        "string",
						"description": "Entity type.",
					},
					"event_limit": map[string]any{
						"type":        "integer",
						"description": "Max recent change_events to return (default 25, max 200).",
					},
				},
				Required: []string{"external_id", "type"},
			},
		},
	}

	for _, t := range buildActionTools() {
		toolParams = append(toolParams, t)
	}

	tools = make([]anthropic.ToolUnionParam, len(toolParams))
	for i := range toolParams {
		tp := toolParams[i]
		tools[i] = anthropic.ToolUnionParam{OfTool: &tp}
	}
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// ── HTTP handlers ───────────────────────────────────────────────────────────

func indexHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	tpl.ExecuteTemplate(w, "index.html", nil)
}

func chatHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	userMsg := strings.TrimSpace(r.FormValue("message"))
	if userMsg == "" {
		http.Error(w, "empty message", http.StatusBadRequest)
		return
	}

	sid := sessionID(w, r)
	history := loadHistory(sid)
	history = append(history, anthropic.NewUserMessage(anthropic.NewTextBlock(userMsg)))

	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	ctx = WithCallContext(ctx, "agent:claude", sid, "http")

	updated, finalText, toolTrace, err := runAgent(ctx, history)
	if err != nil {
		log.Printf("agent error: %v", err)
		renderUser(w, userMsg)
		renderError(w, err.Error())
		return
	}
	saveHistory(sid, updated)

	renderUser(w, userMsg)
	for _, t := range toolTrace {
		renderTool(w, t)
	}
	renderBot(w, finalText)
}

func sessionID(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie("session"); err == nil && c.Value != "" {
		return c.Value
	}
	id := fmt.Sprintf("s%d", time.Now().UnixNano())
	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    id,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   60 * 60 * 4,
	})
	return id
}

func loadHistory(sid string) []anthropic.MessageParam {
	if v, ok := sessions.Load(sid); ok {
		return append([]anthropic.MessageParam{}, v.([]anthropic.MessageParam)...)
	}
	return nil
}

func saveHistory(sid string, h []anthropic.MessageParam) {
	if len(h) > 40 {
		h = h[len(h)-40:]
	}
	sessions.Store(sid, h)
}

// ── Rendering ───────────────────────────────────────────────────────────────

var (
	userTpl  = template.Must(template.New("u").Parse(`<div class="msg user">{{.}}</div>`))
	botTpl   = template.Must(template.New("b").Parse(`<div class="msg bot">{{.}}</div>`))
	toolTpl  = template.Must(template.New("t").Parse(`<div class="msg tool">{{.}}</div>`))
	errorTpl = template.Must(template.New("e").Parse(`<div class="msg error">{{.}}</div>`))

	mdRenderer = goldmark.New(
		goldmark.WithExtensions(extension.Table, extension.Strikethrough),
		goldmark.WithRendererOptions(goldmarkhtml.WithUnsafe()),
	)
)

func renderUser(w http.ResponseWriter, s string) { userTpl.Execute(w, s) }
func renderBot(w http.ResponseWriter, s string) {
	var buf bytes.Buffer
	if err := mdRenderer.Convert([]byte(s), &buf); err != nil {
		buf.Reset()
		buf.WriteString(template.HTMLEscapeString(s))
	}
	botTpl.Execute(w, template.HTML(buf.String()))
}
func renderTool(w http.ResponseWriter, s string)  { toolTpl.Execute(w, s) }
func renderError(w http.ResponseWriter, s string) { errorTpl.Execute(w, s) }

// ── Agent loop ──────────────────────────────────────────────────────────────

func runAgent(ctx context.Context, messages []anthropic.MessageParam) ([]anthropic.MessageParam, string, []string, error) {
	var toolTrace []string

	for round := 0; round < maxToolRounds; round++ {
		resp, err := anthropicClient.Messages.New(ctx, anthropic.MessageNewParams{
			Model:     anthropic.Model(model),
			MaxTokens: 4096,
			System: []anthropic.TextBlockParam{
				{
					Text: systemPrompt,
					CacheControl: anthropic.CacheControlEphemeralParam{
						Type: "ephemeral",
					},
				},
			},
			Tools:    tools,
			Messages: messages,
		})
		if err != nil {
			return messages, "", toolTrace, fmt.Errorf("anthropic: %w", err)
		}

		logUsage(resp.Usage)
		messages = append(messages, resp.ToParam())

		var (
			toolResults []anthropic.ContentBlockParamUnion
			finalText   strings.Builder
		)

		for _, block := range resp.Content {
			switch v := block.AsAny().(type) {
			case anthropic.TextBlock:
				if v.Text != "" {
					finalText.WriteString(v.Text)
				}
			case anthropic.ToolUseBlock:
				inputJSON := string(v.JSON.Input.Raw())
				toolTrace = append(toolTrace, fmt.Sprintf("→ %s(%s)", v.Name, truncate(inputJSON, 240)))
				result, isErr := executeTool(ctx, v.Name, inputJSON)
				toolTrace = append(toolTrace, fmt.Sprintf("← %s", truncate(result, 320)))
				toolResults = append(toolResults, anthropic.NewToolResultBlock(v.ID, result, isErr))
			}
		}

		if len(toolResults) == 0 {
			return messages, finalText.String(), toolTrace, nil
		}
		messages = append(messages, anthropic.NewUserMessage(toolResults...))
	}

	return messages, "(stopped after max tool rounds)", toolTrace, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// logUsage records per-call token + cache stats so we can see whether the
// prompt cache is hitting. Looks for cache_creation_input_tokens (first
// request that wrote the cache) and cache_read_input_tokens (subsequent
// requests that read it).
func logUsage(u anthropic.Usage) {
	log.Printf("anthropic usage: input=%d output=%d cache_create=%d cache_read=%d",
		u.InputTokens, u.OutputTokens,
		u.CacheCreationInputTokens, u.CacheReadInputTokens)
}

// ── Tool dispatch ───────────────────────────────────────────────────────────

func executeTool(ctx context.Context, name, inputJSON string) (string, bool) {
	rec := startToolCall(ctx, name, inputJSON)
	out, err := dispatchTool(ctx, name, inputJSON)
	if err != nil {
		msg := fmt.Sprintf("error: %v", err)
		rec.finish(ctx, msg, true)
		return msg, true
	}
	rec.finish(ctx, out, false)
	return out, false
}

func dispatchTool(ctx context.Context, name, inputJSON string) (string, error) {
	switch name {
	case "list_queries":
		return doListQueries()
	case "describe_schema":
		return doDescribeSchema(ctx)
	case "run_query":
		var in struct {
			Name   string         `json:"name"`
			Params map[string]any `json:"params"`
		}
		if err := json.Unmarshal([]byte(inputJSON), &in); err != nil {
			return "", fmt.Errorf("bad input: %w", err)
		}
		return doRunQuery(ctx, in.Name, in.Params)
	case "search_entities":
		var in struct {
			Text  string `json:"text"`
			Type  string `json:"type"`
			Mode  string `json:"mode"`
			Limit int    `json:"limit"`
		}
		if err := json.Unmarshal([]byte(inputJSON), &in); err != nil {
			return "", fmt.Errorf("bad input: %w", err)
		}
		if in.Mode == "semantic" {
			return doSemanticSearch(ctx, in.Text, in.Type, in.Limit)
		}
		return doSearchEntities(ctx, in.Text, in.Type, in.Limit)
	case "get_entity":
		var in struct {
			ExternalID string `json:"external_id"`
			Type       string `json:"type"`
		}
		if err := json.Unmarshal([]byte(inputJSON), &in); err != nil {
			return "", fmt.Errorf("bad input: %w", err)
		}
		return doGetEntity(ctx, in.ExternalID, in.Type)
	case "list_metrics":
		return doListMetrics()
	case "query_metric":
		var in struct {
			Metric  string            `json:"metric"`
			GroupBy string            `json:"group_by"`
			Filters map[string]string `json:"filters"`
			Limit   int               `json:"limit"`
		}
		if err := json.Unmarshal([]byte(inputJSON), &in); err != nil {
			return "", fmt.Errorf("bad input: %w", err)
		}
		return doQueryMetric(ctx, in.Metric, in.GroupBy, in.Filters, in.Limit)
	case "list_actions":
		return doListActions()
	case "entity_actions":
		var in struct {
			ExternalID string `json:"external_id"`
			Type       string `json:"type"`
			Limit      int    `json:"limit"`
		}
		if err := json.Unmarshal([]byte(inputJSON), &in); err != nil {
			return "", fmt.Errorf("bad input: %w", err)
		}
		return doEntityActions(ctx, in.ExternalID, in.Type, in.Limit)
	case "entity_lineage":
		var in struct {
			ExternalID string `json:"external_id"`
			Type       string `json:"type"`
			EventLimit int    `json:"event_limit"`
		}
		if err := json.Unmarshal([]byte(inputJSON), &in); err != nil {
			return "", fmt.Errorf("bad input: %w", err)
		}
		return doEntityLineage(ctx, in.ExternalID, in.Type, in.EventLimit)
	}

	if strings.HasPrefix(name, "action_") {
		actionName := strings.TrimPrefix(name, "action_")
		var in map[string]any
		if err := json.Unmarshal([]byte(inputJSON), &in); err != nil {
			return "", fmt.Errorf("bad input: %w", err)
		}
		externalID, _ := in["external_id"].(string)
		delete(in, "external_id")
		if tt, ok := in["target_type"].(string); ok {
			in["_target_type"] = tt
			delete(in, "target_type")
		}
		actor, session, _ := callContextFrom(ctx)
		if actor == "unknown" {
			actor = "agent:claude"
		}
		return executeAction(ctx, actionName, externalID, in, actor, session)
	}

	return "", fmt.Errorf("unknown tool %q", name)
}
