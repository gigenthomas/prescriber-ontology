package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// doEntityLineage returns the full lineage history of one entity:
//   - identity (external_id + type + canonical_label)
//   - source (which dataset it came from)
//   - pipeline_runs that touched it (via change_event.pipeline_run_id)
//   - action_invocations targeting it
//   - current entity_state
//   - recent change_events
//
// Useful for compliance / debugging / "where did this fact come from".
func doEntityLineage(ctx context.Context, externalID, entityType string, eventLimit int) (string, error) {
	if externalID == "" || entityType == "" {
		return "", fmt.Errorf("external_id and type are required")
	}
	if eventLimit <= 0 {
		eventLimit = 25
	}
	if eventLimit > 200 {
		eventLimit = 200
	}

	// Identity + source.
	var (
		entityID       string
		canonicalLabel string
		createdAt      time.Time
		updatedAt      time.Time
		version        int
		attrsJSON      sql.NullString
		sourceName     sql.NullString
		sourceURI      sql.NullString
		sourceVersion  sql.NullString
	)
	err := pgPool.QueryRow(ctx, `
        SELECT e.id::text, e.canonical_label, e.created_at, e.updated_at, e.version,
               e.attrs::text, s.name, s.uri, s.version
        FROM entity e
        LEFT JOIN source s ON s.id = e.source_id
        WHERE e.type = $1 AND e.external_id = $2`,
		entityType, externalID,
	).Scan(&entityID, &canonicalLabel, &createdAt, &updatedAt, &version, &attrsJSON, &sourceName, &sourceURI, &sourceVersion)
	if err != nil {
		return "", fmt.Errorf("entity not found: %s/%s", entityType, externalID)
	}

	var attrs any
	if attrsJSON.Valid {
		_ = json.Unmarshal([]byte(attrsJSON.String), &attrs)
	}

	// Pipeline runs that touched this entity (via change_event.pipeline_run_id).
	plRows, err := pgPool.Query(ctx, `
        SELECT pr.id::text, pr.name, pr.started_at, pr.finished_at, pr.status,
               pr.actor, pr.inputs::text, pr.outputs::text, pr.commit_sha,
               count(ce.id) AS event_count
        FROM pipeline_run pr
        JOIN change_event ce ON ce.pipeline_run_id = pr.id
        WHERE ce.record_id = $1::uuid
        GROUP BY pr.id
        ORDER BY pr.started_at DESC`,
		entityID,
	)
	if err != nil {
		return "", err
	}
	defer plRows.Close()

	type pipelineRunRow struct {
		ID          string         `json:"id"`
		Name        string         `json:"name"`
		StartedAt   string         `json:"started_at"`
		FinishedAt  string         `json:"finished_at,omitempty"`
		Status      string         `json:"status"`
		Actor       string         `json:"actor"`
		Inputs      map[string]any `json:"inputs,omitempty"`
		Outputs     map[string]any `json:"outputs,omitempty"`
		CommitSHA   string         `json:"commit_sha,omitempty"`
		EventCount  int            `json:"event_count"`
	}
	var pipelineRuns []pipelineRunRow
	for plRows.Next() {
		var pr pipelineRunRow
		var started time.Time
		var finished sql.NullTime
		var inputs, outputs string
		var commit sql.NullString
		if err := plRows.Scan(&pr.ID, &pr.Name, &started, &finished, &pr.Status, &pr.Actor, &inputs, &outputs, &commit, &pr.EventCount); err != nil {
			return "", err
		}
		pr.StartedAt = started.Format(time.RFC3339)
		if finished.Valid {
			pr.FinishedAt = finished.Time.Format(time.RFC3339)
		}
		if commit.Valid {
			pr.CommitSHA = commit.String
		}
		_ = json.Unmarshal([]byte(inputs), &pr.Inputs)
		_ = json.Unmarshal([]byte(outputs), &pr.Outputs)
		pipelineRuns = append(pipelineRuns, pr)
	}

	// Action invocations targeting this entity.
	actRows, err := pgPool.Query(ctx, `
        SELECT id::text, action_name, params::text, actor, COALESCE(session_id, ''),
               invoked_at, status, COALESCE(error_msg, '')
        FROM action_invocation
        WHERE target_external_id = $1 AND target_type = $2
        ORDER BY invoked_at DESC
        LIMIT 100`,
		externalID, entityType)
	if err != nil {
		return "", err
	}
	defer actRows.Close()

	type actionRow struct {
		ID        string         `json:"id"`
		Action    string         `json:"action"`
		Params    map[string]any `json:"params"`
		Actor     string         `json:"actor"`
		Session   string         `json:"session_id,omitempty"`
		InvokedAt string         `json:"invoked_at"`
		Status    string         `json:"status"`
		Error     string         `json:"error,omitempty"`
	}
	var actions []actionRow
	for actRows.Next() {
		var a actionRow
		var params string
		var invoked time.Time
		var errMsg string
		if err := actRows.Scan(&a.ID, &a.Action, &params, &a.Actor, &a.Session, &invoked, &a.Status, &errMsg); err != nil {
			return "", err
		}
		a.InvokedAt = invoked.Format(time.RFC3339)
		if errMsg != "" {
			a.Error = errMsg
		}
		_ = json.Unmarshal([]byte(params), &a.Params)
		actions = append(actions, a)
	}

	// Current entity_state.
	var stateJSON sql.NullString
	var stateUpdated sql.NullTime
	_ = pgPool.QueryRow(ctx, `
        SELECT es.state::text, es.updated_at
        FROM entity_state es
        WHERE es.entity_id = $1::uuid`,
		entityID,
	).Scan(&stateJSON, &stateUpdated)
	var currentState any
	if stateJSON.Valid {
		_ = json.Unmarshal([]byte(stateJSON.String), &currentState)
	}

	// Recent change_events.
	evRows, err := pgPool.Query(ctx, `
        SELECT ce.id, ce.occurred_at, ce.topic, ce.op,
               COALESCE(ce.pipeline_run_id::text, ''),
               COALESCE(pr.name, ''),
               COALESCE(ce.action_invocation_id::text, ''),
               COALESCE(ai.action_name, '')
        FROM change_event ce
        LEFT JOIN pipeline_run     pr ON pr.id = ce.pipeline_run_id
        LEFT JOIN action_invocation ai ON ai.id = ce.action_invocation_id
        WHERE ce.record_id = $1::uuid
        ORDER BY ce.id DESC
        LIMIT $2`,
		entityID, eventLimit)
	if err != nil {
		return "", err
	}
	defer evRows.Close()

	type eventRow struct {
		ID                 int64  `json:"id"`
		OccurredAt         string `json:"occurred_at"`
		Topic              string `json:"topic"`
		Op                 string `json:"op"`
		PipelineRunID      string `json:"pipeline_run_id,omitempty"`
		PipelineRunName    string `json:"pipeline_run_name,omitempty"`
		ActionInvocationID string `json:"action_invocation_id,omitempty"`
		ActionName         string `json:"action_name,omitempty"`
	}
	var events []eventRow
	for evRows.Next() {
		var ev eventRow
		var occurred time.Time
		if err := evRows.Scan(&ev.ID, &occurred, &ev.Topic, &ev.Op, &ev.PipelineRunID, &ev.PipelineRunName, &ev.ActionInvocationID, &ev.ActionName); err != nil {
			return "", err
		}
		ev.OccurredAt = occurred.Format(time.RFC3339)
		events = append(events, ev)
	}

	out := map[string]any{
		"identity": map[string]any{
			"id":              entityID,
			"type":            entityType,
			"external_id":     externalID,
			"canonical_label": canonicalLabel,
			"created_at":      createdAt.Format(time.RFC3339),
			"updated_at":      updatedAt.Format(time.RFC3339),
			"version":         version,
			"attrs":           attrs,
		},
		"source": map[string]any{
			"name":    nullStr(sourceName),
			"uri":     nullStr(sourceURI),
			"version": nullStr(sourceVersion),
		},
		"pipeline_runs": pipelineRuns,
		"actions":       actions,
		"current_state": currentState,
		"events":        events,
	}
	if stateUpdated.Valid {
		out["state_updated_at"] = stateUpdated.Time.Format(time.RFC3339)
	}
	return marshal(out)
}

func nullStr(s sql.NullString) string {
	if s.Valid {
		return s.String
	}
	return ""
}
