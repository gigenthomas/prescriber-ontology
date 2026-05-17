package main

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// ── Call context ────────────────────────────────────────────────────────────
//
// Actor + session + transport flow through context. The chatbot's chatHandler
// sets these from the HTTP request; MCP handlers set them via mcpDispatch.
// executeTool reads them when recording telemetry.

type ctxKey int

const (
	ctxActor ctxKey = iota
	ctxSession
	ctxTransport
)

// WithCallContext attaches actor / session / transport tags for telemetry.
func WithCallContext(ctx context.Context, actor, session, transport string) context.Context {
	ctx = context.WithValue(ctx, ctxActor, actor)
	ctx = context.WithValue(ctx, ctxSession, session)
	ctx = context.WithValue(ctx, ctxTransport, transport)
	return ctx
}

func callContextFrom(ctx context.Context) (actor, session, transport string) {
	actor = "unknown"
	transport = "unknown"
	if v, ok := ctx.Value(ctxActor).(string); ok && v != "" {
		actor = v
	}
	if v, ok := ctx.Value(ctxSession).(string); ok {
		session = v
	}
	if v, ok := ctx.Value(ctxTransport).(string); ok && v != "" {
		transport = v
	}
	return
}

// ── Telemetry recording ────────────────────────────────────────────────────

type callRecord struct {
	id    int64
	start time.Time
	name  string
}

// startToolCall inserts a pending row. Returns a record whose finish()
// closes the loop. Best-effort: telemetry write failures are logged but do
// not block the tool call.
func startToolCall(ctx context.Context, name, paramsJSON string) *callRecord {
	actor, session, transport := callContextFrom(ctx)
	var id int64
	err := pgPool.QueryRow(ctx, `
        INSERT INTO tool_call_log
            (tool_name, params, actor, session_id, transport, status)
        VALUES ($1, NULLIF($2, '')::jsonb, $3, NULLIF($4, ''), $5, 'pending')
        RETURNING id`,
		name, paramsJSON, actor, session, transport).Scan(&id)
	if err != nil {
		log.Printf("[telemetry] start %s: %v", name, err)
		return &callRecord{name: name, start: time.Now()}
	}
	return &callRecord{id: id, name: name, start: time.Now()}
}

// finish records duration, status, error, result size.
func (r *callRecord) finish(ctx context.Context, result string, isErr bool) {
	r.finishWithPolicy(ctx, result, isErr, true, "")
}

// finishWithPolicy is finish + records the OPA decision (allow + reason)
// alongside the call. opaAllow=true,reason="" is treated as "no policy
// engine consulted" (e.g. AUTH_PROVIDER=none). Writes to the optional
// opa_allow / opa_reason columns added by migration 0008; absent until
// phase 4 lands, so failures are silently downgraded.
func (r *callRecord) finishWithPolicy(ctx context.Context, result string, isErr bool, opaAllow bool, opaReason string) {
	if r == nil || r.id == 0 {
		return
	}
	status := "ok"
	var errMsg string
	if isErr {
		status = "error"
		errMsg = result
	}
	duration := int(time.Since(r.start).Milliseconds())

	// Try the richer UPDATE first (with opa_* columns). If the migration
	// hasn't been applied, fall back to the original UPDATE — this keeps
	// auth phase 2 working even when the schema is still phase 1.
	_, err := pgPool.Exec(ctx, `
        UPDATE tool_call_log
        SET finished_at = now(),
            duration_ms = $1,
            status      = $2,
            error_msg   = NULLIF($3, ''),
            result_size = $4,
            opa_allow   = $5,
            opa_reason  = NULLIF($6, '')
        WHERE id = $7`,
		duration, status, errMsg, len(result), opaAllow, opaReason, r.id)
	if err != nil {
		// Likely "column opa_allow does not exist" — fall back.
		_, err2 := pgPool.Exec(ctx, `
            UPDATE tool_call_log
            SET finished_at = now(),
                duration_ms = $1,
                status      = $2,
                error_msg   = NULLIF($3, ''),
                result_size = $4
            WHERE id = $5`,
			duration, status, errMsg, len(result), r.id)
		if err2 != nil {
			log.Printf("[telemetry] finish %s id=%d: %v", r.name, r.id, err2)
		}
	}
}

// ── MCP dispatch unification ────────────────────────────────────────────────

// mcpDispatch returns a CallToolHandler that routes the request through the
// shared dispatchTool. This unifies the chatbot and MCP code paths through
// a single dispatcher, so telemetry only has to be wrapped once.
//
// When auth is enabled, MCP calls run as a configurable service account
// (MCP_SERVICE_SUB / MCP_SERVICE_USERNAME / MCP_SERVICE_ROLES) so the same
// OPA policies apply. When auth is disabled, fall back to the historical
// "agent:mcp" actor string.
func mcpDispatch(toolName string) func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		argsBytes, err := json.Marshal(args)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		actor := "agent:mcp"
		if authEnabled() {
			svc := mcpServiceAccountUser()
			ctx = withUser(ctx, svc)
			actor = svc.Subject
		}
		ctx = WithCallContext(ctx, actor, "", "mcp")

		out, isErr := executeTool(ctx, toolName, string(argsBytes))
		if isErr {
			return mcp.NewToolResultError(out), nil
		}
		return mcp.NewToolResultText(out), nil
	}
}

// mcpServiceAccountUser synthesises an AuthenticatedUser for MCP calls
// when auth is enabled. The sub/username/roles come from env (defaults
// chosen so the local-dev experience works without extra config).
func mcpServiceAccountUser() *AuthenticatedUser {
	roles := strings.Split(getenv("MCP_SERVICE_ROLES", "compliance,analyst,viewer"), ",")
	cleaned := make([]string, 0, len(roles))
	for _, r := range roles {
		if r = strings.TrimSpace(r); r != "" {
			cleaned = append(cleaned, r)
		}
	}
	return &AuthenticatedUser{
		Subject:   getenv("MCP_SERVICE_SUB", "00000000-0000-0000-0000-00000000mcp1"),
		Username:  getenv("MCP_SERVICE_USERNAME", "mcp-service"),
		Email:     "mcp@service.local",
		Name:      "MCP Service Account",
		Roles:     cleaned,
		ExpiresAt: time.Now().Add(24 * time.Hour),
	}
}
