package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"gopkg.in/yaml.v3"
)

// ── Config ──────────────────────────────────────────────────────────────────

type actionsConfig struct {
	Actions map[string]actionDef `yaml:"actions"`
}

type actionDef struct {
	Description  string              `yaml:"description"`
	TargetType   string              `yaml:"target_type"`
	Params       map[string]paramDef `yaml:"params,omitempty"`
	StateUpdates map[string]any      `yaml:"state_updates,omitempty"`
}

type paramDef struct {
	Type        string   `yaml:"type" json:"type"`
	Values      []string `yaml:"values,omitempty" json:"values,omitempty"`
	Required    bool     `yaml:"required,omitempty" json:"required,omitempty"`
	Default     any      `yaml:"default,omitempty" json:"default,omitempty"`
	Description string   `yaml:"description,omitempty" json:"description,omitempty"`
}

var (
	actionCfg   actionsConfig
	actionsFile = getenv("ONTOLOGY_ACTIONS_FILE", "./actions.yaml")
)

func loadActions() error {
	b, err := os.ReadFile(actionsFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			log.Printf("no %s found — actions disabled", actionsFile)
			actionCfg = actionsConfig{Actions: map[string]actionDef{}}
			return nil
		}
		return fmt.Errorf("read %s: %w", actionsFile, err)
	}
	return yaml.Unmarshal(b, &actionCfg)
}

func actionNames() []string {
	names := make([]string, 0, len(actionCfg.Actions))
	for k := range actionCfg.Actions {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// ── Validation + substitution ───────────────────────────────────────────────

// validateAndSubstitute checks user-supplied params against the action's
// param spec, applies defaults, rejects unknown params, and substitutes
// "$name" placeholders inside state_updates with resolved values.
func validateAndSubstitute(def actionDef, input map[string]any) (map[string]any, map[string]any, error) {
	resolved := map[string]any{}

	// Defaults + type-check supplied values.
	for name, spec := range def.Params {
		val, present := input[name]
		if !present {
			if spec.Required {
				return nil, nil, fmt.Errorf("missing required param %q", name)
			}
			if spec.Default != nil {
				resolved[name] = spec.Default
			}
			continue
		}
		switch spec.Type {
		case "string":
			s, ok := val.(string)
			if !ok {
				return nil, nil, fmt.Errorf("param %q must be a string", name)
			}
			if spec.Required && strings.TrimSpace(s) == "" {
				return nil, nil, fmt.Errorf("param %q must be non-empty", name)
			}
			resolved[name] = s
		case "enum":
			s, ok := val.(string)
			if !ok {
				return nil, nil, fmt.Errorf("param %q must be a string (enum)", name)
			}
			valid := false
			for _, v := range spec.Values {
				if s == v {
					valid = true
					break
				}
			}
			if !valid {
				return nil, nil, fmt.Errorf("param %q must be one of %v, got %q", name, spec.Values, s)
			}
			resolved[name] = s
		case "integer":
			switch n := val.(type) {
			case float64:
				resolved[name] = int(n)
			case int:
				resolved[name] = n
			default:
				return nil, nil, fmt.Errorf("param %q must be an integer", name)
			}
		case "number":
			switch n := val.(type) {
			case float64:
				resolved[name] = n
			case int:
				resolved[name] = float64(n)
			default:
				return nil, nil, fmt.Errorf("param %q must be a number", name)
			}
		case "boolean":
			b, ok := val.(bool)
			if !ok {
				return nil, nil, fmt.Errorf("param %q must be a boolean", name)
			}
			resolved[name] = b
		default:
			return nil, nil, fmt.Errorf("param %q has unsupported type %q", name, spec.Type)
		}
	}

	// Reject unknown params.
	for name := range input {
		if name == "_target_type" {
			continue // internal hint for target_type=any
		}
		if _, ok := def.Params[name]; !ok {
			return nil, nil, fmt.Errorf("unknown param %q", name)
		}
	}

	updates := substituteUpdates(def.StateUpdates, resolved)
	return resolved, updates, nil
}

func substituteUpdates(template map[string]any, params map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range template {
		out[k] = substituteValue(v, params)
	}
	return out
}

func substituteValue(v any, params map[string]any) any {
	if s, ok := v.(string); ok && strings.HasPrefix(s, "$") {
		if val, ok := params[s[1:]]; ok {
			return val
		}
	}
	return v
}

// ── Executor ────────────────────────────────────────────────────────────────

type actionResult struct {
	InvocationID string         `json:"invocation_id"`
	Action       string         `json:"action"`
	Target       string         `json:"target"`
	Params       map[string]any `json:"params"`
	StateUpdates map[string]any `json:"state_updates,omitempty"`
	Status       string         `json:"status"`
}

func executeAction(
	ctx context.Context,
	actionName, externalID string,
	input map[string]any,
	actor, sessionID string,
) (string, error) {
	def, ok := actionCfg.Actions[actionName]
	if !ok {
		return "", fmt.Errorf("unknown action %q", actionName)
	}

	// Resolve target type — either fixed by the action, or supplied via hidden _target_type.
	targetType := def.TargetType
	if targetType == "any" {
		t, ok := input["_target_type"].(string)
		if !ok || t == "" {
			return "", fmt.Errorf("action %q targets any entity; pass target_type", actionName)
		}
		targetType = t
		delete(input, "_target_type")
	}

	// Resolve target entity.
	var entityID, canonicalLabel string
	err := pgPool.QueryRow(ctx, `
        SELECT id::text, canonical_label
        FROM entity
        WHERE external_id = $1 AND type = $2
        LIMIT 1`, externalID, targetType).Scan(&entityID, &canonicalLabel)
	if err != nil {
		// Log a rejected invocation (no FK on entity since we never resolved it).
		recordRejection(ctx, actionName, "", targetType, externalID, input, actor, sessionID,
			fmt.Sprintf("target %s/%s not found", targetType, externalID))
		return "", fmt.Errorf("target %s/%s not found", targetType, externalID)
	}

	resolvedParams, stateUpdates, err := validateAndSubstitute(def, input)
	if err != nil {
		recordRejection(ctx, actionName, entityID, targetType, externalID, input, actor, sessionID, err.Error())
		return "", err
	}

	// Apply in a single transaction: audit row + state update.
	tx, err := pgPool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)

	paramsJSON, _ := json.Marshal(resolvedParams)
	var invocationID string
	err = tx.QueryRow(ctx, `
        INSERT INTO action_invocation
            (action_name, target_entity_id, target_type, target_external_id, params, actor, session_id, status)
        VALUES ($1, $2::uuid, $3, $4, $5::jsonb, $6, NULLIF($7, ''), 'applied')
        RETURNING id::text`,
		actionName, entityID, targetType, externalID, string(paramsJSON), actor, sessionID,
	).Scan(&invocationID)
	if err != nil {
		return "", fmt.Errorf("insert invocation: %w", err)
	}

	if len(stateUpdates) > 0 {
		updatesJSON, _ := json.Marshal(stateUpdates)
		_, err = tx.Exec(ctx, `
            INSERT INTO entity_state (entity_id, state, last_action_id)
            VALUES ($1::uuid, $2::jsonb, $3::uuid)
            ON CONFLICT (entity_id) DO UPDATE
            SET state          = entity_state.state || EXCLUDED.state,
                updated_at     = now(),
                last_action_id = EXCLUDED.last_action_id`,
			entityID, string(updatesJSON), invocationID)
		if err != nil {
			return "", fmt.Errorf("apply state: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return "", err
	}

	return marshal(actionResult{
		InvocationID: invocationID,
		Action:       actionName,
		Target:       fmt.Sprintf("%s:%s (%s)", targetType, externalID, canonicalLabel),
		Params:       resolvedParams,
		StateUpdates: stateUpdates,
		Status:       "applied",
	})
}

func recordRejection(
	ctx context.Context,
	actionName, entityID, targetType, externalID string,
	input map[string]any,
	actor, sessionID, errMsg string,
) {
	inputJSON, _ := json.Marshal(input)
	var targetIDArg any
	if entityID != "" {
		targetIDArg = entityID
	}
	_, err := pgPool.Exec(ctx, `
        INSERT INTO action_invocation
            (action_name, target_entity_id, target_type, target_external_id, params, actor, session_id, status, error_msg)
        VALUES ($1, $2::uuid, $3, $4, $5::jsonb, $6, NULLIF($7, ''), 'rejected', $8)`,
		actionName, targetIDArg, targetType, externalID, string(inputJSON), actor, sessionID, errMsg)
	if err != nil {
		log.Printf("failed to log rejected invocation: %v", err)
	}
}

// ── Discovery + history ─────────────────────────────────────────────────────

func doListActions() (string, error) {
	type entry struct {
		Name         string              `json:"name"`
		Description  string              `json:"description"`
		TargetType   string              `json:"target_type"`
		Params       map[string]paramDef `json:"params,omitempty"`
		StateUpdates map[string]any      `json:"state_updates,omitempty"`
	}
	list := make([]entry, 0, len(actionCfg.Actions))
	for _, n := range actionNames() {
		d := actionCfg.Actions[n]
		list = append(list, entry{
			Name: n, Description: d.Description, TargetType: d.TargetType,
			Params: d.Params, StateUpdates: d.StateUpdates,
		})
	}
	return marshal(map[string]any{"actions": list})
}

func doEntityActions(ctx context.Context, externalID, entityType string, limit int) (string, error) {
	if limit <= 0 {
		limit = 25
	}
	if limit > 100 {
		limit = 100
	}

	rows, err := pgPool.Query(ctx, `
        SELECT id::text, action_name, params::text, actor, COALESCE(session_id, ''),
               invoked_at, status, COALESCE(error_msg, '')
        FROM action_invocation
        WHERE target_external_id = $1 AND target_type = $2
        ORDER BY invoked_at DESC
        LIMIT $3`,
		externalID, entityType, limit)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	type inv struct {
		ID        string         `json:"invocation_id"`
		Action    string         `json:"action"`
		Params    map[string]any `json:"params"`
		Actor     string         `json:"actor"`
		SessionID string         `json:"session_id,omitempty"`
		At        string         `json:"invoked_at"`
		Status    string         `json:"status"`
		Error     string         `json:"error,omitempty"`
	}
	var invs []inv
	for rows.Next() {
		var i inv
		var paramsStr string
		var at time.Time
		var errMsg string
		if err := rows.Scan(&i.ID, &i.Action, &paramsStr, &i.Actor, &i.SessionID, &at, &i.Status, &errMsg); err != nil {
			return "", err
		}
		i.At = at.Format(time.RFC3339)
		if errMsg != "" {
			i.Error = errMsg
		}
		_ = json.Unmarshal([]byte(paramsStr), &i.Params)
		invs = append(invs, i)
	}

	// Current entity_state (if any).
	var stateStr sql.NullString
	err = pgPool.QueryRow(ctx, `
        SELECT es.state::text
        FROM entity e
        LEFT JOIN entity_state es ON es.entity_id = e.id
        WHERE e.external_id = $1 AND e.type = $2`,
		externalID, entityType).Scan(&stateStr)
	var state map[string]any
	if err == nil && stateStr.Valid {
		_ = json.Unmarshal([]byte(stateStr.String), &state)
	}

	return marshal(map[string]any{
		"target":        fmt.Sprintf("%s:%s", entityType, externalID),
		"current_state": state,
		"invocations":   invs,
	})
}

// ── Anthropic tool generation ───────────────────────────────────────────────

func buildActionTools() []anthropic.ToolParam {
	tools := make([]anthropic.ToolParam, 0, len(actionCfg.Actions))
	for _, name := range actionNames() {
		tools = append(tools, buildOneActionTool(name, actionCfg.Actions[name]))
	}
	return tools
}

func buildOneActionTool(name string, def actionDef) anthropic.ToolParam {
	props := map[string]any{
		"external_id": map[string]any{
			"type": "string",
			"description": fmt.Sprintf(
				"External identifier of the target entity (NPI for Prescriber, brand name for Drug, etc.). Type=%s.",
				def.TargetType),
		},
	}
	required := []string{"external_id"}

	if def.TargetType == "any" {
		props["target_type"] = map[string]any{
			"type":        "string",
			"description": "Entity type: Prescriber, Drug, GenericDrug, Specialty, or Location.",
		}
		required = append(required, "target_type")
	}

	for paramName, spec := range def.Params {
		schema := map[string]any{}
		if spec.Description != "" {
			schema["description"] = spec.Description
		}
		switch spec.Type {
		case "enum":
			schema["type"] = "string"
			schema["enum"] = spec.Values
		case "string":
			schema["type"] = "string"
		case "integer":
			schema["type"] = "integer"
		case "number":
			schema["type"] = "number"
		case "boolean":
			schema["type"] = "boolean"
		}
		props[paramName] = schema
		if spec.Required {
			required = append(required, paramName)
		}
	}

	desc := def.Description
	if def.TargetType != "any" {
		desc = fmt.Sprintf("%s Target type: %s.", desc, def.TargetType)
	}

	return anthropic.ToolParam{
		Name:        "action_" + name,
		Description: anthropic.String(desc),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: props,
			Required:   required,
		},
	}
}
