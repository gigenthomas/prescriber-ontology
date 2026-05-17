package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"
)

type telemetryPageData struct {
	Summary   telemetrySummary
	ToolStats []toolStatRow
	Filter    telemetryFilter
	Rows      []callRow
}

type telemetrySummary struct {
	Total            int
	Errors           int
	AvgMs            int
	P95Ms            int
	DistinctTools    int
	DistinctSessions int
	Denied           int
}

type toolStatRow struct {
	Name     string
	Calls    int
	AvgMs    int
	MaxMs    int
	Errors   int
	ErrorPct int
}

type telemetryFilter struct {
	Tool   string
	Status string
	Actor  string
	OPA    string // "" | "allowed" | "denied"
}

type callRow struct {
	When       string
	Tool       string
	Actor      string
	Transport  string
	DurationMs string
	ResultSize string
	Status     string
	Error      string
	ParamsJSON string
	OPAAllow   string // "allowed" | "denied" | ""
	OPAReason  string
}

func telemetryHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()
	filter := telemetryFilter{
		Tool:   strings.TrimSpace(r.URL.Query().Get("tool")),
		Status: strings.TrimSpace(r.URL.Query().Get("status")),
		Actor:  strings.TrimSpace(r.URL.Query().Get("actor")),
		OPA:    strings.TrimSpace(r.URL.Query().Get("opa")),
	}

	summary, err := loadTelemetrySummary(ctx)
	if err != nil {
		log.Printf("telemetry summary: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	stats, err := loadToolStats(ctx)
	if err != nil {
		log.Printf("tool stats: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	rows, err := loadCallRows(ctx, filter, 100)
	if err != nil {
		log.Printf("call rows: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	data := telemetryPageData{
		Summary:   summary,
		ToolStats: stats,
		Filter:    filter,
		Rows:      rows,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tpl.ExecuteTemplate(w, "telemetry.html", data); err != nil {
		log.Printf("render telemetry.html: %v", err)
	}
}

func loadTelemetrySummary(ctx context.Context) (telemetrySummary, error) {
	var s telemetrySummary
	err := pgPool.QueryRow(ctx, `
        SELECT
            count(*),
            count(*) FILTER (WHERE status='error'),
            COALESCE(avg(duration_ms), 0)::int,
            COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_ms), 0)::int,
            count(DISTINCT tool_name),
            count(DISTINCT session_id) FILTER (WHERE session_id IS NOT NULL),
            count(*) FILTER (WHERE opa_allow = false)
        FROM tool_call_log`).Scan(
		&s.Total, &s.Errors, &s.AvgMs, &s.P95Ms, &s.DistinctTools, &s.DistinctSessions, &s.Denied)
	return s, err
}

func loadToolStats(ctx context.Context) ([]toolStatRow, error) {
	rows, err := pgPool.Query(ctx, `
        SELECT tool_name,
               count(*) AS calls,
               COALESCE(avg(duration_ms), 0)::int AS avg_ms,
               COALESCE(max(duration_ms), 0)      AS max_ms,
               count(*) FILTER (WHERE status='error') AS errors
        FROM tool_call_log
        WHERE invoked_at >= now() - interval '7 days'
        GROUP BY tool_name
        ORDER BY calls DESC, avg_ms DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []toolStatRow
	for rows.Next() {
		var r toolStatRow
		if err := rows.Scan(&r.Name, &r.Calls, &r.AvgMs, &r.MaxMs, &r.Errors); err != nil {
			return nil, err
		}
		if r.Calls > 0 {
			r.ErrorPct = (r.Errors * 100) / r.Calls
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func loadCallRows(ctx context.Context, f telemetryFilter, limit int) ([]callRow, error) {
	var q strings.Builder
	q.WriteString(`
        SELECT id, invoked_at, tool_name, actor, transport,
               COALESCE(duration_ms, 0),
               COALESCE(result_size, 0),
               status,
               COALESCE(error_msg, ''),
               COALESCE(params::text, ''),
               opa_allow,
               COALESCE(opa_reason, '')
        FROM tool_call_log
        WHERE 1=1`)

	var args []any
	add := func(clause string, v any) {
		args = append(args, v)
		q.WriteString(strings.Replace(clause, "$?", placeholder(len(args)), 1))
	}
	if f.Tool != "" {
		add(` AND tool_name = $?`, f.Tool)
	}
	if f.Status != "" {
		add(` AND status = $?`, f.Status)
	}
	if f.Actor != "" {
		add(` AND actor = $?`, f.Actor)
	}
	switch f.OPA {
	case "allowed":
		q.WriteString(` AND opa_allow = true`)
	case "denied":
		q.WriteString(` AND opa_allow = false`)
	}
	args = append(args, limit)
	q.WriteString(` ORDER BY invoked_at DESC LIMIT ` + placeholder(len(args)))

	rows, err := pgPool.Query(ctx, q.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []callRow
	for rows.Next() {
		var (
			id       int64
			invoked  time.Time
			tool     string
			actor    string
			trans    string
			duration int
			rsize    int
			status   string
			params   string
			opaAllow sql.NullBool
			opaReason string
			cr       callRow
		)
		var errStr string
		if err := rows.Scan(&id, &invoked, &tool, &actor, &trans, &duration, &rsize, &status, &errStr, &params, &opaAllow, &opaReason); err != nil {
			return nil, err
		}
		cr.When = invoked.Format("2006-01-02 15:04:05")
		cr.Tool = tool
		cr.Actor = actor
		cr.Transport = trans
		cr.DurationMs = itoa(duration)
		cr.ResultSize = formatBytes(rsize)
		cr.Status = status
		cr.Error = errStr
		cr.ParamsJSON = prettyJSONShort(params, 240)
		if opaAllow.Valid {
			if opaAllow.Bool {
				cr.OPAAllow = "allowed"
			} else {
				cr.OPAAllow = "denied"
			}
		}
		cr.OPAReason = opaReason
		out = append(out, cr)
	}
	return out, rows.Err()
}

func placeholder(n int) string {
	return "$" + itoa(n)
}

func formatBytes(n int) string {
	switch {
	case n >= 1024*1024:
		return itoa(n/(1024*1024)) + " MB"
	case n >= 1024:
		return itoa(n/1024) + " KB"
	default:
		return itoa(n) + " B"
	}
}

func prettyJSONShort(s string, max int) string {
	if s == "" || s == "null" {
		return ""
	}
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return truncate(s, max)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return truncate(s, max)
	}
	return truncate(string(b), max)
}
