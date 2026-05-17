package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// ChangeEvent is one row from the change_event outbox table. Field names
// mirror what would appear on a Kafka record so migration is a transport
// swap, not a rewrite.
type ChangeEvent struct {
	ID         int64           `json:"id"`
	OccurredAt time.Time       `json:"occurred_at"`
	Topic      string          `json:"topic"`
	Key        string          `json:"key"`
	Op         string          `json:"op"`
	RecordID   string          `json:"record_id"`
	Payload    json.RawMessage `json:"payload"`
}

// EventConsumer is one subscriber to one or more topics. Tracks its own
// cursor per topic; multiple consumers never block each other.
type EventConsumer struct {
	Name    string
	Topics  []string
	Handler func(context.Context, ChangeEvent) error
}

const (
	heartbeatInterval = 30 * time.Second
	drainBatchSize    = 500
)

// StartConsumers launches each consumer in its own goroutine. They run
// until the supplied context is cancelled.
func StartConsumers(ctx context.Context, consumers []*EventConsumer) {
	for _, c := range consumers {
		go c.Run(ctx)
	}
}

// Run is the consumer loop. Opens a LISTEN connection, drains the outbox,
// blocks on NOTIFY (with a heartbeat timeout so missed NOTIFYs don't stall
// progress), and repeats.
func (c *EventConsumer) Run(ctx context.Context) {
	log.Printf("[events] consumer %q starting, topics=%v", c.Name, c.Topics)

	for {
		if err := c.runOnce(ctx); err != nil {
			if ctx.Err() != nil {
				log.Printf("[events] consumer %q shutting down: %v", c.Name, ctx.Err())
				return
			}
			log.Printf("[events] consumer %q error: %v (retrying in 5s)", c.Name, err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}
}

func (c *EventConsumer) runOnce(ctx context.Context) error {
	// Acquire a dedicated pgx connection from the pool for LISTEN; release
	// it on exit. pgxpool conn.Hijack() would let us own it forever, but
	// for v1 a leased conn is fine.
	conn, err := pgPool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "LISTEN ontology_events"); err != nil {
		return fmt.Errorf("LISTEN: %w", err)
	}

	for {
		if err := c.drain(ctx); err != nil {
			return err
		}

		// Wait for either a NOTIFY or the heartbeat. WaitForNotification
		// blocks; we wrap it in a context with deadline to enforce the
		// heartbeat.
		waitCtx, cancel := context.WithTimeout(ctx, heartbeatInterval)
		_, err := conn.Conn().WaitForNotification(waitCtx)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// timeout is expected; loop to drain again
			continue
		}
		// NOTIFY received; loop will drain
	}
}

// drain reads new events for this consumer's topics, applies them in order,
// and advances the cursor on success. Stops if the handler returns an
// error so the next pass retries from the same cursor.
func (c *EventConsumer) drain(ctx context.Context) error {
	cursors, err := c.loadCursors(ctx)
	if err != nil {
		return fmt.Errorf("load cursors: %w", err)
	}

	// Pull the smallest cursor across all subscribed topics as the floor —
	// then filter in Go to assign each event to the right topic cursor.
	floor := minCursor(cursors)

	rows, err := pgPool.Query(ctx, `
        SELECT id, occurred_at, topic, COALESCE("key", ''), op, COALESCE(record_id::text, ''), COALESCE(payload, 'null'::jsonb)
        FROM change_event
        WHERE topic = ANY($1) AND id > $2
        ORDER BY id
        LIMIT $3`,
		c.Topics, floor, drainBatchSize)
	if err != nil {
		return fmt.Errorf("query events: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var ev ChangeEvent
		if err := rows.Scan(&ev.ID, &ev.OccurredAt, &ev.Topic, &ev.Key, &ev.Op, &ev.RecordID, &ev.Payload); err != nil {
			return fmt.Errorf("scan event: %w", err)
		}
		// Skip already-processed events for this topic.
		if cursors[ev.Topic] >= ev.ID {
			continue
		}
		if err := c.Handler(ctx, ev); err != nil {
			log.Printf("[events] %s handler error on id=%d topic=%s: %v", c.Name, ev.ID, ev.Topic, err)
			return err
		}
		if err := c.updateCursor(ctx, ev.Topic, ev.ID); err != nil {
			return fmt.Errorf("update cursor: %w", err)
		}
		cursors[ev.Topic] = ev.ID
	}
	return rows.Err()
}

func (c *EventConsumer) loadCursors(ctx context.Context) (map[string]int64, error) {
	out := map[string]int64{}
	for _, t := range c.Topics {
		out[t] = 0
	}
	rows, err := pgPool.Query(ctx, `
        SELECT topic, last_id FROM consumer_cursor
        WHERE consumer_name = $1 AND topic = ANY($2)`,
		c.Name, c.Topics)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var t string
		var id int64
		if err := rows.Scan(&t, &id); err != nil {
			return nil, err
		}
		out[t] = id
	}
	return out, rows.Err()
}

func (c *EventConsumer) updateCursor(ctx context.Context, topic string, lastID int64) error {
	_, err := pgPool.Exec(ctx, `
        INSERT INTO consumer_cursor (consumer_name, topic, last_id)
        VALUES ($1, $2, $3)
        ON CONFLICT (consumer_name, topic) DO UPDATE
        SET last_id = EXCLUDED.last_id, updated_at = now()
        WHERE consumer_cursor.last_id < EXCLUDED.last_id`,
		c.Name, topic, lastID)
	return err
}

func minCursor(m map[string]int64) int64 {
	first := true
	var min int64
	for _, v := range m {
		if first || v < min {
			min = v
			first = false
		}
	}
	return min
}

// ensure pgx import isn't unused on early compile passes
var _ = pgx.ErrNoRows

// startedConsumers guards against double-starting in main.
var startedConsumers sync.Once

func startConsumers(ctx context.Context) {
	startedConsumers.Do(func() {
		StartConsumers(ctx, []*EventConsumer{newNeo4jReprojector()})
	})
}

// formatChange returns a short human description for the logs.
func formatChange(ev ChangeEvent) string {
	return strings.Join([]string{ev.Topic, ev.Op, ev.RecordID}, " ")
}
