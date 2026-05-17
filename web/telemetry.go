package main

import (
	"context"
	"encoding/json"
	"log"
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
	_, err := pgPool.Exec(ctx, `
        UPDATE tool_call_log
        SET finished_at = now(),
            duration_ms = $1,
            status      = $2,
            error_msg   = NULLIF($3, ''),
            result_size = $4
        WHERE id = $5`,
		duration, status, errMsg, len(result), r.id)
	if err != nil {
		log.Printf("[telemetry] finish %s id=%d: %v", r.name, r.id, err)
	}
}

// ── MCP dispatch unification ────────────────────────────────────────────────

// mcpDispatch returns a CallToolHandler that routes the request through the
// shared dispatchTool. This unifies the chatbot and MCP code paths through
// a single dispatcher, so telemetry only has to be wrapped once.
func mcpDispatch(toolName string) func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		argsBytes, err := json.Marshal(args)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		ctx = WithCallContext(ctx, "agent:mcp", "", "mcp")
		out, isErr := executeTool(ctx, toolName, string(argsBytes))
		if isErr {
			return mcp.NewToolResultError(out), nil
		}
		return mcp.NewToolResultText(out), nil
	}
}
