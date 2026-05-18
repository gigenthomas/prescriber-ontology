package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"strings"
)

// entityMeta maps entity type names to their display color and emoji.
var entityMeta = map[string]struct{ color, emoji string }{
	"Prescriber": {"#2d6cdf", "🧑‍⚕️"},
	"Drug":       {"#2a9d5c", "💊"},
	"Generic":    {"#1a8a8a", "🧪"},
	"Specialty":  {"#e07b2a", "🏥"},
	"City":       {"#6b7280", "📍"},
}

func entityChip(entityType string) template.HTML {
	meta, ok := entityMeta[entityType]
	if !ok {
		meta = struct{ color, emoji string }{"#888", "◆"}
	}
	return template.HTML(fmt.Sprintf(
		`<span class="entity-chip" style="background:%s">%s %s</span>`,
		meta.color, meta.emoji, template.HTMLEscapeString(entityType),
	))
}

// tripleStrip returns the HTML relationship path shown in each tool trace header.
func tripleStrip(toolName, inputJSON string) template.HTML {
	fwd := func(rel string) string {
		return fmt.Sprintf(`<span class="triple-arrow"> ──</span><span class="triple-rel">%s</span><span class="triple-arrow">──▶ </span>`, template.HTMLEscapeString(rel))
	}
	back := func(rel string) string {
		return fmt.Sprintf(`<span class="triple-arrow"> ◀──</span><span class="triple-rel">%s</span><span class="triple-arrow">── </span>`, template.HTMLEscapeString(rel))
	}
	label := func(s string) template.HTML {
		return template.HTML(fmt.Sprintf(`<span class="triple-label">%s</span>`, template.HTMLEscapeString(s)))
	}
	join := func(parts ...string) template.HTML {
		return template.HTML(strings.Join(parts, ""))
	}

	switch toolName {
	case "run_query":
		var input struct {
			Name string `json:"name"`
		}
		json.Unmarshal([]byte(inputJSON), &input)
		switch input.Name {
		case "co_prescribed":
			return join(string(entityChip("Prescriber")), fwd("prescribed"), string(entityChip("Drug")), back("prescribed"), string(entityChip("Prescriber")))
		case "drug_top_prescribers":
			return join(string(entityChip("Drug")), back("prescribed"), string(entityChip("Prescriber")))
		case "specialty_drug_breakdown", "costly_specialties":
			return join(string(entityChip("Prescriber")), fwd("has_specialty"), string(entityChip("Specialty")))
		case "city_prescriber_counts":
			return join(string(entityChip("Prescriber")), fwd("practices_in"), string(entityChip("City")))
		case "brands_per_generic":
			return join(string(entityChip("Drug")), fwd("generic_of"), string(entityChip("Generic")))
		default:
			return join(string(entityChip("Prescriber")), fwd("prescribed"), string(entityChip("Drug")))
		}

	case "query_metric":
		var input struct {
			GroupBy string `json:"group_by"`
		}
		json.Unmarshal([]byte(inputJSON), &input)
		switch input.GroupBy {
		case "specialty":
			return join(string(entityChip("Prescriber")), fwd("has_specialty"), string(entityChip("Specialty")))
		case "city":
			return join(string(entityChip("Prescriber")), fwd("practices_in"), string(entityChip("City")))
		case "generic":
			return join(string(entityChip("Drug")), fwd("generic_of"), string(entityChip("Generic")))
		default:
			return join(string(entityChip("Prescriber")), fwd("prescribed"), string(entityChip("Drug")))
		}

	case "get_entity":
		var input struct {
			Type string `json:"type"`
		}
		json.Unmarshal([]byte(inputJSON), &input)
		t := input.Type
		if t == "" {
			t = "Entity"
		}
		return entityChip(t)

	case "search_entities":
		var input struct {
			EntityType string `json:"entity_type"`
		}
		json.Unmarshal([]byte(inputJSON), &input)
		t := input.EntityType
		if t == "" {
			t = "Entity"
		}
		return entityChip(t)

	case "entity_lineage":
		return join(string(entityChip("Entity")), fwd("lineage"),
			`<span class="entity-chip" style="background:#7c3aed">📋 Pipeline</span>`)

	case "describe_schema":
		return label("Full schema")

	case "list_queries", "list_metrics":
		return label("Catalog")
	}

	if strings.HasPrefix(toolName, "action_") {
		action := strings.TrimPrefix(toolName, "action_")
		return join(string(entityChip("Entity")), fwd(action),
			`<span class="entity-chip" style="background:#6b7280">📝 State</span>`)
	}

	return label(toolName)
}

// resultCount extracts a row count from a JSON result string.
func resultCount(resultJSON string) int {
	var rc struct {
		RowCount int `json:"row_count"`
	}
	if json.Unmarshal([]byte(resultJSON), &rc) == nil && rc.RowCount > 0 {
		return rc.RowCount
	}
	var arr []any
	if json.Unmarshal([]byte(resultJSON), &arr) == nil {
		return len(arr)
	}
	return 0
}

// toMermaid generates a Mermaid graph LR string from a tool result.
// Returns empty string if the tool/result doesn't lend itself to a diagram.
func toMermaid(toolName, inputJSON, resultJSON string) string {
	classDefs := "\n  classDef prescriber fill:#2d6cdf,color:#fff,stroke:none\n" +
		"  classDef drug fill:#2a9d5c,color:#fff,stroke:none\n" +
		"  classDef generic fill:#1a8a8a,color:#fff,stroke:none\n" +
		"  classDef specialty fill:#e07b2a,color:#fff,stroke:none\n" +
		"  classDef city fill:#6b7280,color:#fff,stroke:none"

	switch toolName {
	case "get_entity":
		return mermaidGetEntity(resultJSON, classDefs)
	case "run_query":
		return mermaidRunQuery(inputJSON, resultJSON, classDefs)
	}
	return ""
}

func mermaidGetEntity(resultJSON, classDefs string) string {
	var result struct {
		Type           string           `json:"type"`
		CanonicalLabel string           `json:"canonical_label"`
		OutDegree      map[string]int64 `json:"out_degree"`
		InDegree       map[string]int64 `json:"in_degree"`
	}
	if err := json.Unmarshal([]byte(resultJSON), &result); err != nil {
		return ""
	}

	predicateTarget := map[string]string{
		"prescribed":    "drug",
		"has_specialty": "specialty",
		"practices_in":  "city",
		"generic_of":    "generic",
	}

	var sb strings.Builder
	sb.WriteString("graph LR\n")
	sb.WriteString(fmt.Sprintf("  E([\"%s\"]):::%s\n", mermaidLabel(result.CanonicalLabel), strings.ToLower(result.Type)))

	i := 0
	for pred, count := range result.OutDegree {
		cls, ok := predicateTarget[pred]
		if !ok {
			continue
		}
		nodeID := fmt.Sprintf("N%d", i)
		sb.WriteString(fmt.Sprintf("  %s([\"%s (%d)\"]):::%s\n", nodeID, strings.Title(strings.ReplaceAll(pred, "_", " ")), count, cls))
		sb.WriteString(fmt.Sprintf("  E -->|\"%s\"| %s\n", pred, nodeID))
		i++
	}
	for pred, count := range result.InDegree {
		cls, ok := predicateTarget[pred]
		if !ok {
			continue
		}
		nodeID := fmt.Sprintf("I%d", i)
		sb.WriteString(fmt.Sprintf("  %s([\"%s (%d)\"]):::%s\n", nodeID, strings.Title(strings.ReplaceAll(pred, "_", " ")), count, cls))
		sb.WriteString(fmt.Sprintf("  %s -->|\"%s\"| E\n", nodeID, pred))
		i++
	}
	sb.WriteString(classDefs)
	return sb.String()
}

func mermaidRunQuery(inputJSON, resultJSON, classDefs string) string {
	var input struct {
		Name string `json:"name"`
	}
	json.Unmarshal([]byte(inputJSON), &input)

	var result struct {
		Rows []map[string]any `json:"rows"`
	}
	if err := json.Unmarshal([]byte(resultJSON), &result); err != nil || len(result.Rows) == 0 {
		return ""
	}
	rows := result.Rows
	if len(rows) > 8 {
		rows = rows[:8]
	}

	var sb strings.Builder
	sb.WriteString("graph LR\n")

	switch input.Name {
	case "top_prescribers_by_claims", "prescriber_drugs":
		sb.WriteString("  D([\"💊 All Drugs\"]):::drug\n")
		for i, row := range rows {
			name := fmt.Sprint(row["prescriber"])
			claims := fmt.Sprint(row["total_claims"])
			sb.WriteString(fmt.Sprintf("  P%d([\"🧑‍⚕️ %s\\n%s claims\"]):::prescriber\n", i, mermaidLabel(name), claims))
			sb.WriteString(fmt.Sprintf("  P%d -->|\"prescribed\"| D\n", i))
		}

	case "drug_top_prescribers":
		sb.WriteString("  D([\"💊 Drug\"]):::drug\n")
		for i, row := range rows {
			name := fmt.Sprint(row["prescriber"])
			sb.WriteString(fmt.Sprintf("  P%d([\"🧑‍⚕️ %s\"]):::prescriber\n", i, mermaidLabel(name)))
			sb.WriteString(fmt.Sprintf("  P%d -->|\"prescribed\"| D\n", i))
		}

	case "costly_specialties", "specialty_drug_breakdown":
		sb.WriteString("  P([\"🧑‍⚕️ Prescribers\"]):::prescriber\n")
		for i, row := range rows {
			name := fmt.Sprint(row["specialty"])
			sb.WriteString(fmt.Sprintf("  S%d([\"🏥 %s\"]):::specialty\n", i, mermaidLabel(name)))
			sb.WriteString(fmt.Sprintf("  P -->|\"has_specialty\"| S%d\n", i))
		}

	case "city_prescriber_counts":
		sb.WriteString("  P([\"🧑‍⚕️ Prescribers\"]):::prescriber\n")
		for i, row := range rows {
			name := fmt.Sprint(row["city"])
			sb.WriteString(fmt.Sprintf("  C%d([\"📍 %s\"]):::city\n", i, mermaidLabel(name)))
			sb.WriteString(fmt.Sprintf("  P -->|\"practices_in\"| C%d\n", i))
		}

	default:
		return ""
	}

	sb.WriteString(classDefs)
	return sb.String()
}

func mermaidLabel(s string) string {
	s = strings.ReplaceAll(s, `"`, `'`)
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 28 {
		s = s[:28] + "…"
	}
	return s
}
