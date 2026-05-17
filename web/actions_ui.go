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

// ── Page data ───────────────────────────────────────────────────────────────

type actionsPageData struct {
	Summary     actionsSummary
	Filter      actionsFilter
	Rows        []invocationRow
	ActionNames []string
}

type actionsSummary struct {
	Total     int
	Applied   int
	Rejected  int
	WithState int
}

type actionsFilter struct {
	Action     string
	Status     string
	TargetType string
	TargetID   string
}

type invocationRow struct {
	ID          string
	When        string
	Action      string
	TargetType  string
	TargetID    string
	TargetLabel string
	ParamsJSON  string
	StateJSON   string
	Actor       string
	Status      string
	Error       string
}

// ── Handler ─────────────────────────────────────────────────────────────────

func actionsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()
	filter := actionsFilter{
		Action:     strings.TrimSpace(r.URL.Query().Get("action")),
		Status:     strings.TrimSpace(r.URL.Query().Get("status")),
		TargetType: strings.TrimSpace(r.URL.Query().Get("target_type")),
		TargetID:   strings.TrimSpace(r.URL.Query().Get("target")),
	}

	summary, err := loadActionsSummary(ctx)
	if err != nil {
		log.Printf("actions summary: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	rows, err := loadInvocationRows(ctx, filter, 100)
	if err != nil {
		log.Printf("actions rows: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	data := actionsPageData{
		Summary:     summary,
		Filter:      filter,
		Rows:        rows,
		ActionNames: actionNames(),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tpl.ExecuteTemplate(w, "actions.html", data); err != nil {
		log.Printf("render actions.html: %v", err)
	}
}

// ── Queries ─────────────────────────────────────────────────────────────────

func loadActionsSummary(ctx context.Context) (actionsSummary, error) {
	var s actionsSummary
	err := pgPool.QueryRow(ctx, `
        SELECT
            count(*) FILTER (WHERE TRUE),
            count(*) FILTER (WHERE status='applied'),
            count(*) FILTER (WHERE status='rejected')
        FROM action_invocation`).Scan(&s.Total, &s.Applied, &s.Rejected)
	if err != nil {
		return s, err
	}
	err = pgPool.QueryRow(ctx, `
        SELECT count(*) FROM entity_state WHERE state <> '{}'::jsonb`).Scan(&s.WithState)
	if err != nil {
		return s, err
	}
	return s, nil
}

func loadInvocationRows(ctx context.Context, f actionsFilter, limit int) ([]invocationRow, error) {
	q := strings.Builder{}
	q.WriteString(`
        SELECT
            ai.id::text,
            ai.invoked_at,
            ai.action_name,
            ai.target_type,
            ai.target_external_id,
            COALESCE(e.canonical_label, '(target not in graph)') AS label,
            ai.params::text,
            COALESCE(es.state::text, '{}'),
            ai.actor,
            ai.status,
            COALESCE(ai.error_msg, '')
        FROM action_invocation ai
        LEFT JOIN entity e ON e.id = ai.target_entity_id
        LEFT JOIN entity_state es ON es.entity_id = ai.target_entity_id
        WHERE 1=1`)

	var args []any
	add := func(s string, v any) {
		args = append(args, v)
		q.WriteString(s)
	}
	if f.Action != "" {
		add(" AND ai.action_name = $%d", f.Action)
	}
	if f.Status != "" {
		add(" AND ai.status = $%d", f.Status)
	}
	if f.TargetType != "" {
		add(" AND ai.target_type = $%d", f.TargetType)
	}
	if f.TargetID != "" {
		add(" AND ai.target_external_id = $%d", f.TargetID)
	}
	// Substitute placeholder index numbers.
	finalQuery := q.String()
	for i := range args {
		finalQuery = strings.Replace(finalQuery, "$%d", "$"+itoa(i+1), 1)
	}
	finalQuery += " ORDER BY ai.invoked_at DESC LIMIT $" + itoa(len(args)+1)
	args = append(args, limit)

	rows, err := pgPool.Query(ctx, finalQuery, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []invocationRow
	for rows.Next() {
		var (
			r       invocationRow
			invoked time.Time
			errMsg  string
		)
		var stateJSON sql.NullString
		var stateStr string
		if err := rows.Scan(
			&r.ID, &invoked, &r.Action, &r.TargetType, &r.TargetID, &r.TargetLabel,
			&r.ParamsJSON, &stateStr, &r.Actor, &r.Status, &errMsg,
		); err != nil {
			return nil, err
		}
		_ = stateJSON
		r.When = invoked.Format("2006-01-02 15:04:05")
		r.ParamsJSON = prettyJSON(r.ParamsJSON)
		r.StateJSON = prettyJSON(stateStr)
		if errMsg != "" {
			r.Error = errMsg
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func prettyJSON(s string) string {
	if s == "" || s == "{}" || s == "null" {
		return ""
	}
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return s
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return s
	}
	return string(b)
}

// itoa avoids fmt.Sprintf in a tight builder loop.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	buf := [20]byte{}
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
