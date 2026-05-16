package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
	"gopkg.in/yaml.v3"
)

// ── Config ──────────────────────────────────────────────────────────────────

type metricsConfig struct {
	Base       metricsBase                 `yaml:"base"`
	Metrics    map[string]metricDef        `yaml:"metrics"`
	Dimensions map[string]dimensionDef     `yaml:"dimensions"`
}

type metricsBase struct {
	Match string `yaml:"match"`
}

type metricDef struct {
	Description string `yaml:"description"`
	Cypher      string `yaml:"cypher"`
}

type dimensionDef struct {
	Description string   `yaml:"description"`
	Match       string   `yaml:"match,omitempty"`
	Expression  string   `yaml:"expression"`
	Extra       []string `yaml:"extra,omitempty"`
}

var (
	metricCfg    metricsConfig
	metricsFile  = getenv("ONTOLOGY_METRICS_FILE", "./metrics.yaml")
)

func loadMetrics() error {
	b, err := os.ReadFile(metricsFile)
	if err != nil {
		return fmt.Errorf("read %s: %w", metricsFile, err)
	}
	if err := yaml.Unmarshal(b, &metricCfg); err != nil {
		return fmt.Errorf("parse %s: %w", metricsFile, err)
	}
	if metricCfg.Base.Match == "" {
		return fmt.Errorf("metrics config missing base.match")
	}
	if len(metricCfg.Metrics) == 0 || len(metricCfg.Dimensions) == 0 {
		return fmt.Errorf("metrics config must define at least one metric and one dimension")
	}
	return nil
}

func metricNames() []string {
	names := make([]string, 0, len(metricCfg.Metrics))
	for k := range metricCfg.Metrics {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

func dimensionNames() []string {
	names := make([]string, 0, len(metricCfg.Dimensions))
	for k := range metricCfg.Dimensions {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// ── Compiler ────────────────────────────────────────────────────────────────

// compileMetricQuery builds a Cypher statement plus a parameter map from the
// declarative request. Returns ErrUnknown* if the metric or any referenced
// dimension is missing.
func compileMetricQuery(
	metric string,
	groupBy string,
	filters map[string]string,
	limit int,
) (cypher string, params map[string]any, err error) {
	m, ok := metricCfg.Metrics[metric]
	if !ok {
		return "", nil, fmt.Errorf("unknown metric %q; available: %v", metric, metricNames())
	}

	var gbDim *dimensionDef
	if groupBy != "" {
		d, ok := metricCfg.Dimensions[groupBy]
		if !ok {
			return "", nil, fmt.Errorf("unknown dimension %q; available: %v", groupBy, dimensionNames())
		}
		gbDim = &d
	}

	for fName := range filters {
		if _, ok := metricCfg.Dimensions[fName]; !ok {
			return "", nil, fmt.Errorf("unknown filter dimension %q; available: %v", fName, dimensionNames())
		}
	}

	var lines []string
	lines = append(lines, "MATCH "+metricCfg.Base.Match)

	seenMatch := map[string]bool{}
	var addMatches []string
	if gbDim != nil && gbDim.Match != "" {
		seenMatch[gbDim.Match] = true
		addMatches = append(addMatches, gbDim.Match)
	}
	fNames := sortedKeys(filters)
	for _, fName := range fNames {
		d := metricCfg.Dimensions[fName]
		if d.Match != "" && !seenMatch[d.Match] {
			seenMatch[d.Match] = true
			addMatches = append(addMatches, d.Match)
		}
	}
	for _, m := range addMatches {
		lines = append(lines, "MATCH "+m)
	}

	params = map[string]any{}
	var wheres []string
	for _, fName := range fNames {
		d := metricCfg.Dimensions[fName]
		paramName := "f_" + fName
		wheres = append(wheres, fmt.Sprintf("%s = $%s", d.Expression, paramName))
		params[paramName] = filters[fName]
	}
	if len(wheres) > 0 {
		lines = append(lines, "WHERE "+strings.Join(wheres, " AND "))
	}

	if gbDim != nil {
		retParts := []string{
			fmt.Sprintf("%s AS %s", gbDim.Expression, groupBy),
			fmt.Sprintf("%s AS %s", m.Cypher, metric),
		}
		retParts = append(retParts, gbDim.Extra...)
		lines = append(lines, "RETURN "+strings.Join(retParts, ", "))
		lines = append(lines, "ORDER BY "+metric+" DESC")
	} else {
		lines = append(lines, fmt.Sprintf("RETURN %s AS %s", m.Cypher, metric))
	}

	if limit > 0 {
		lines = append(lines, fmt.Sprintf("LIMIT %d", limit))
	}

	return strings.Join(lines, "\n"), params, nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ── Tool handlers ──────────────────────────────────────────────────────────

func doListMetrics() (string, error) {
	type entry struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	var metrics, dims []entry
	for _, n := range metricNames() {
		metrics = append(metrics, entry{n, metricCfg.Metrics[n].Description})
	}
	for _, n := range dimensionNames() {
		dims = append(dims, entry{n, metricCfg.Dimensions[n].Description})
	}
	return marshal(map[string]any{"metrics": metrics, "dimensions": dims})
}

func doQueryMetric(
	ctx context.Context,
	metric, groupBy string,
	filters map[string]string,
	limit int,
) (string, error) {
	if limit <= 0 {
		limit = 25
	} else if limit > 250 {
		limit = 250
	}

	cypher, params, err := compileMetricQuery(metric, groupBy, filters, limit)
	if err != nil {
		return "", err
	}

	session := neoDriver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	defer session.Close(ctx)

	rowsAny, err := session.ExecuteRead(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		res, err := tx.Run(ctx, cypher, params)
		if err != nil {
			return nil, err
		}
		var rows []map[string]any
		for res.Next(ctx) {
			rec := res.Record()
			row := map[string]any{}
			for _, k := range rec.Keys {
				v, _ := rec.Get(k)
				row[k] = neoValue(v)
			}
			rows = append(rows, row)
		}
		return rows, res.Err()
	})
	if err != nil {
		return "", fmt.Errorf("neo4j: %w", err)
	}

	rows, _ := rowsAny.([]map[string]any)
	return marshal(map[string]any{
		"metric":    metric,
		"group_by":  groupBy,
		"filters":   filters,
		"row_count": len(rows),
		"cypher":    cypher,
		"rows":      rows,
	})
}
