package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// doListQueries returns the catalog as JSON.
func doListQueries() (string, error) {
	return marshal(queryCatalog)
}

// doDescribeSchema returns entity types, predicates, and live counts.
func doDescribeSchema(ctx context.Context) (string, error) {
	rows, err := pgPool.Query(ctx, `
        SELECT kind, name, description
        FROM schema_term
        ORDER BY kind, name`)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	type term struct{ Kind, Name, Description string }
	var terms []term
	for rows.Next() {
		var t term
		if err := rows.Scan(&t.Kind, &t.Name, &t.Description); err != nil {
			return "", err
		}
		terms = append(terms, t)
	}

	counts, err := pgPool.Query(ctx, `
        SELECT type, count(*) AS n FROM entity GROUP BY type
        UNION ALL
        SELECT 'rel:' || predicate, count(*) FROM relation GROUP BY predicate
        ORDER BY 1`)
	if err != nil {
		return "", err
	}
	defer counts.Close()

	cs := map[string]int64{}
	for counts.Next() {
		var k string
		var n int64
		if err := counts.Scan(&k, &n); err != nil {
			return "", err
		}
		cs[k] = n
	}

	return marshal(map[string]any{"terms": terms, "counts": cs})
}

// doRunQuery loads the named .cypher file and runs it with params.
func doRunQuery(ctx context.Context, name string, params map[string]any) (string, error) {
	if name == "" {
		return "", fmt.Errorf("name is required")
	}
	if strings.ContainsAny(name, "/\\.") {
		return "", fmt.Errorf("invalid query name")
	}
	path := filepath.Join(queriesDir, name+".cypher")
	body, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("query %q not found", name)
	}

	if params == nil {
		params = map[string]any{}
	}

	session := neoDriver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	defer session.Close(ctx)

	result, err := session.ExecuteRead(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		res, err := tx.Run(ctx, string(body), params)
		if err != nil {
			return nil, err
		}
		var out []map[string]any
		for res.Next(ctx) {
			rec := res.Record()
			row := map[string]any{}
			for _, k := range rec.Keys {
				v, _ := rec.Get(k)
				row[k] = neoValue(v)
			}
			out = append(out, row)
			if len(out) >= 100 {
				break
			}
		}
		return out, res.Err()
	})
	if err != nil {
		return "", err
	}

	rows, _ := result.([]map[string]any)
	return marshal(map[string]any{
		"query":      name,
		"row_count":  len(rows),
		"truncated":  len(rows) == 100,
		"rows":       rows,
	})
}

func neoValue(v any) any {
	switch x := v.(type) {
	case neo4j.Node:
		return map[string]any{"labels": x.Labels, "props": x.Props}
	case neo4j.Relationship:
		return map[string]any{"type": x.Type, "props": x.Props}
	case neo4j.Path:
		labels := make([]string, 0, len(x.Nodes))
		for _, n := range x.Nodes {
			if l, ok := n.Props["canonical_label"]; ok {
				labels = append(labels, fmt.Sprint(l))
			}
		}
		return map[string]any{"length": len(x.Relationships), "labels": labels}
	default:
		return v
	}
}

// doSearchEntities runs a Postgres trigram-similarity search on canonical_label.
func doSearchEntities(ctx context.Context, text, entityType string, limit int) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("text is required")
	}
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}

	args := []any{text}
	// % is the pg_trgm similarity operator. NOT a format directive — fmt.Sprintf
	// doesn't reinterpret the substituted value, so a single % is correct here.
	where := "canonical_label % $1"
	if entityType != "" {
		where += " AND type = $2"
		args = append(args, entityType)
	}
	args = append(args, limit)
	limitParam := fmt.Sprintf("$%d", len(args))

	q := fmt.Sprintf(`
        SELECT external_id, type, canonical_label,
               similarity(canonical_label, $1) AS sim,
               attrs
        FROM entity
        WHERE %s
        ORDER BY sim DESC
        LIMIT %s`, where, limitParam)

	rows, err := pgPool.Query(ctx, q, args...)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	type hit struct {
		ExternalID     string         `json:"external_id"`
		Type           string         `json:"type"`
		CanonicalLabel string         `json:"canonical_label"`
		Similarity     float64        `json:"similarity"`
		Attrs          map[string]any `json:"attrs,omitempty"`
	}
	var hits []hit
	for rows.Next() {
		var h hit
		var attrsBytes []byte
		if err := rows.Scan(&h.ExternalID, &h.Type, &h.CanonicalLabel, &h.Similarity, &attrsBytes); err != nil {
			return "", err
		}
		if len(attrsBytes) > 0 {
			_ = json.Unmarshal(attrsBytes, &h.Attrs)
		}
		hits = append(hits, h)
	}
	return marshal(hits)
}

// doGetEntity fetches one entity plus its relation-degree summary.
func doGetEntity(ctx context.Context, externalID, entityType string) (string, error) {
	if externalID == "" || entityType == "" {
		return "", fmt.Errorf("external_id and type are required")
	}

	var (
		id, label string
		attrs     []byte
	)
	err := pgPool.QueryRow(ctx, `
        SELECT id::text, canonical_label, attrs
        FROM entity
        WHERE type = $1 AND external_id = $2
        LIMIT 1`, entityType, externalID).Scan(&id, &label, &attrs)
	if err != nil {
		return "", fmt.Errorf("entity not found: %s %s", entityType, externalID)
	}

	out := map[string]any{
		"id":              id,
		"type":            entityType,
		"external_id":     externalID,
		"canonical_label": label,
	}
	if len(attrs) > 0 {
		var a any
		_ = json.Unmarshal(attrs, &a)
		out["attrs"] = a
	}

	// outgoing degree
	outRows, err := pgPool.Query(ctx, `
        SELECT predicate, count(*) FROM relation WHERE src_entity_id = $1 GROUP BY predicate`, id)
	if err != nil {
		return "", err
	}
	defer outRows.Close()
	outDeg := map[string]int64{}
	for outRows.Next() {
		var p string
		var n int64
		if err := outRows.Scan(&p, &n); err != nil {
			return "", err
		}
		outDeg[p] = n
	}

	// incoming degree
	inRows, err := pgPool.Query(ctx, `
        SELECT predicate, count(*) FROM relation WHERE dst_entity_id = $1 GROUP BY predicate`, id)
	if err != nil {
		return "", err
	}
	defer inRows.Close()
	inDeg := map[string]int64{}
	for inRows.Next() {
		var p string
		var n int64
		if err := inRows.Scan(&p, &n); err != nil {
			return "", err
		}
		inDeg[p] = n
	}

	out["out_degree"] = outDeg
	out["in_degree"] = inDeg
	return marshal(out)
}

func marshal(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
