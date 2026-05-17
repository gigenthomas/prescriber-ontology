package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// newNeo4jReprojector subscribes to ontology.entity, ontology.relation,
// and ontology.entity_state and incrementally projects each change to
// Neo4j. Idempotent — uses MERGE everywhere, so replays are safe.
func newNeo4jReprojector() *EventConsumer {
	return &EventConsumer{
		Name:   "neo4j_reprojector",
		Topics: []string{"ontology.entity", "ontology.relation", "ontology.entity_state"},
		Handler: func(ctx context.Context, ev ChangeEvent) error {
			switch ev.Topic {
			case "ontology.entity":
				return reprojectEntity(ctx, ev)
			case "ontology.relation":
				return reprojectRelation(ctx, ev)
			case "ontology.entity_state":
				return reprojectEntityState(ctx, ev)
			}
			return nil
		},
	}
}

// reprojectEntity upserts a single entity into Neo4j. Handles DELETE by
// detach-deleting the node (and any edges it touches).
func reprojectEntity(ctx context.Context, ev ChangeEvent) error {
	if ev.Op == "DELETE" {
		return runWrite(ctx, "MATCH (e:Entity {id: $id}) DETACH DELETE e", map[string]any{"id": ev.RecordID})
	}

	var row struct {
		ID             string  `json:"id"`
		ExternalID     *string `json:"external_id"`
		Type           string  `json:"type"`
		CanonicalLabel string  `json:"canonical_label"`
		Attrs          map[string]any
	}
	if err := json.Unmarshal(ev.Payload, &row); err != nil {
		return fmt.Errorf("unmarshal entity payload: %w", err)
	}
	// payload's attrs is a separate field; unmarshal it specifically since
	// it's not a field in our struct above
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(ev.Payload, &raw); err != nil {
		return err
	}
	if attrsRaw, ok := raw["attrs"]; ok && len(attrsRaw) > 0 && string(attrsRaw) != "null" {
		_ = json.Unmarshal(attrsRaw, &row.Attrs)
	}

	props := flattenProps(row.Attrs)
	extID := ""
	if row.ExternalID != nil {
		extID = *row.ExternalID
	}

	cypher := `
        MERGE (e:Entity {id: $id})
        SET e.external_id     = $external_id,
            e.type            = $type,
            e.canonical_label = $canonical_label
        SET e += $props
        WITH e
        CALL apoc.create.addLabels(e, [$type]) YIELD node
        RETURN count(node)`
	return runWrite(ctx, cypher, map[string]any{
		"id":              row.ID,
		"external_id":     extID,
		"type":            row.Type,
		"canonical_label": row.CanonicalLabel,
		"props":           props,
	})
}

// reprojectRelation upserts a single relation. DELETE removes the edge.
func reprojectRelation(ctx context.Context, ev ChangeEvent) error {
	if ev.Op == "DELETE" {
		return runWrite(ctx, "MATCH ()-[r {id: $id}]->() DELETE r", map[string]any{"id": ev.RecordID})
	}

	var row struct {
		ID         string `json:"id"`
		Src        string `json:"src_entity_id"`
		Dst        string `json:"dst_entity_id"`
		Predicate  string `json:"predicate"`
		Attrs      map[string]any
	}
	if err := json.Unmarshal(ev.Payload, &row); err != nil {
		return fmt.Errorf("unmarshal relation payload: %w", err)
	}
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(ev.Payload, &raw)
	if attrsRaw, ok := raw["attrs"]; ok && len(attrsRaw) > 0 && string(attrsRaw) != "null" {
		_ = json.Unmarshal(attrsRaw, &row.Attrs)
	}

	props := flattenProps(row.Attrs)

	cypher := `
        MATCH (a:Entity {id: $src})
        MATCH (b:Entity {id: $dst})
        CALL apoc.merge.relationship(a, $predicate, {id: $id}, $props, b, $props)
        YIELD rel
        RETURN count(rel)`
	return runWrite(ctx, cypher, map[string]any{
		"id":        row.ID,
		"src":       row.Src,
		"dst":       row.Dst,
		"predicate": row.Predicate,
		"props":     props,
	})
}

// reprojectEntityState merges action-driven state onto the corresponding
// Neo4j node with a `state_` prefix on each key, to keep it visually
// separate from source-derived attrs.
func reprojectEntityState(ctx context.Context, ev ChangeEvent) error {
	if ev.Op == "DELETE" {
		// We don't currently clear state_* keys on DELETE since they have
		// arbitrary names. v2 could track them in a separate label or
		// store the key list in entity_state.
		log.Printf("[reproject] entity_state DELETE for %s — Neo4j state_* properties left in place", ev.RecordID)
		return nil
	}

	var row struct {
		EntityID string         `json:"entity_id"`
		State    map[string]any `json:"state"`
	}
	if err := json.Unmarshal(ev.Payload, &row); err != nil {
		return fmt.Errorf("unmarshal entity_state payload: %w", err)
	}

	prefixed := make(map[string]any, len(row.State))
	for k, v := range row.State {
		prefixed["state_"+k] = v
	}

	cypher := `
        MATCH (e:Entity {id: $id})
        SET e += $props`
	return runWrite(ctx, cypher, map[string]any{
		"id":    row.EntityID,
		"props": prefixed,
	})
}

// flattenProps mirrors the Python projector: only keep scalars and
// list-of-scalars as Neo4j accepts those as node/edge properties.
func flattenProps(attrs map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range attrs {
		if v == nil {
			continue
		}
		switch t := v.(type) {
		case string, float64, bool, int, int64:
			out[k] = v
		case []any:
			ok := true
			for _, item := range t {
				switch item.(type) {
				case string, float64, bool, int, int64:
				default:
					ok = false
				}
				if !ok {
					break
				}
			}
			if ok {
				out[k] = v
			}
		}
	}
	return out
}

// runWrite executes a write Cypher in a managed transaction.
func runWrite(ctx context.Context, cypher string, params map[string]any) error {
	session := neoDriver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	defer session.Close(ctx)
	_, err := session.ExecuteWrite(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		res, err := tx.Run(ctx, cypher, params)
		if err != nil {
			return nil, err
		}
		_, err = res.Consume(ctx)
		return nil, err
	})
	return err
}
