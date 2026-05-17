package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// ── OpenAI embedding client ─────────────────────────────────────────────────

const (
	openaiEmbeddingURL    = "https://api.openai.com/v1/embeddings"
	defaultEmbeddingModel = "text-embedding-3-small"
)

type openaiEmbeddingRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type openaiEmbeddingResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// embedQuery sends a single text to OpenAI and returns the 1536-dim
// embedding ready to be used in a pgvector cosine-distance query.
func embedQuery(ctx context.Context, text string) ([]float32, error) {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		return nil, errors.New("OPENAI_API_KEY not set; cannot run semantic search")
	}
	model := os.Getenv("OPENAI_EMBEDDING_MODEL")
	if model == "" {
		model = defaultEmbeddingModel
	}

	body, _ := json.Marshal(openaiEmbeddingRequest{
		Model: model,
		Input: []string{text},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, openaiEmbeddingURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai embed: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("openai embed status %d: %s", resp.StatusCode, string(respBody))
	}
	var out openaiEmbeddingResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("openai embed decode: %w", err)
	}
	if len(out.Data) == 0 {
		return nil, errors.New("openai embed: empty data")
	}
	return out.Data[0].Embedding, nil
}

// vectorLiteral formats a float32 slice as the textual form pgvector
// accepts on input: '[v1,v2,v3,...]'.
func vectorLiteral(v []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, x := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%.6f", x)
	}
	b.WriteByte(']')
	return b.String()
}

// ── Semantic search ─────────────────────────────────────────────────────────

// doSemanticSearch embeds the query and finds entities whose embedding is
// nearest by cosine distance. Optional `entityType` filter, default limit 10.
func doSemanticSearch(ctx context.Context, text, entityType string, limit int) (string, error) {
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

	v, err := embedQuery(ctx, text)
	if err != nil {
		return "", err
	}
	vec := vectorLiteral(v)

	args := []any{vec}
	where := "embedding IS NOT NULL"
	if entityType != "" {
		where += " AND type = $2"
		args = append(args, entityType)
	}
	args = append(args, limit)
	limParam := fmt.Sprintf("$%d", len(args))

	q := fmt.Sprintf(`
        SELECT external_id, type, canonical_label,
               embedding <=> $1::vector AS cosine_distance,
               embedding_text
        FROM entity
        WHERE %s
        ORDER BY embedding <=> $1::vector
        LIMIT %s`, where, limParam)

	rows, err := pgPool.Query(ctx, q, args...)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	type hit struct {
		ExternalID     string  `json:"external_id"`
		Type           string  `json:"type"`
		CanonicalLabel string  `json:"canonical_label"`
		Distance       float64 `json:"cosine_distance"`
		Similarity     float64 `json:"similarity"`
		EmbeddedText   string  `json:"embedded_text,omitempty"`
	}
	var hits []hit
	for rows.Next() {
		var h hit
		var embText string
		if err := rows.Scan(&h.ExternalID, &h.Type, &h.CanonicalLabel, &h.Distance, &embText); err != nil {
			return "", err
		}
		h.Similarity = 1.0 - h.Distance
		h.EmbeddedText = embText
		hits = append(hits, h)
	}
	return marshal(map[string]any{
		"query":   text,
		"mode":    "semantic",
		"results": hits,
	})
}
