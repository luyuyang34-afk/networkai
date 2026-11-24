// Package outbox implements the consumer side of the transactional outbox.
//
// Delivery is at-least-once: an event is marked processed only after its
// handler succeeds, so a crash between "handled" and "marked" replays the
// event. Handlers must therefore be idempotent (see package indexer).
package outbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Event struct {
	ID            int64
	AggregateType string
	AggregateID   string
	EventType     string
	Attempts      int
}

// Handler applies one event. Returning an error schedules a retry.
type Handler func(ctx context.Context, ev Event) error

type Worker struct {
	Pool           *pgxpool.Pool
	Handle         Handler
	BatchSize      int
	PollInterval   time.Duration
	MaxAttempts    int
	HandlerTimeout time.Duration
	Log            *slog.Logger

	// Backoff returns the delay before retry number `attempt` (1-based).
	// Defaults to exponential backoff with jitter, capped at 5 minutes.
	Backoff func(attempt int) time.Duration
}

func DefaultBackoff(attempt int) time.Duration {
	base := 500 * time.Millisecond << min(attempt-1, 10)
	base = min(base, 5*time.Minute)
	// Full jitter avoids every failed event retrying at the same instant.
	return time.Duration(rand.Int64N(int64(base))) + 100*time.Millisecond
}

// Run polls until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) error {
	w.defaults()
	w.Log.Info("outbox worker started", "batch", w.BatchSize, "poll", w.PollInterval)
	lastPrune := time.Time{}
	for {
		n, err := w.ProcessBatch(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			w.Log.Error("outbox batch failed", "err", err)
		}
		if time.Since(lastPrune) > time.Hour {
			if pruned, err := w.Prune(ctx, 7*24*time.Hour); err != nil {
				w.Log.Error("outbox prune failed", "err", err)
			} else if pruned > 0 {
				w.Log.Info("outbox pruned", "rows", pruned)
			}
			lastPrune = time.Now()
		}
		if n > 0 && err == nil {
			continue // more work may be waiting; don't sleep
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(w.PollInterval):
		}
	}
}

// ProcessBatch claims up to BatchSize due events and handles them. It returns
// how many events were claimed.
//
// FOR UPDATE SKIP LOCKED lets several workers run concurrently: each claims a
// disjoint set of rows instead of blocking on (or double-processing) rows
// another worker holds.
func (w *Worker) ProcessBatch(ctx context.Context) (int, error) {
	w.defaults()
	claimed := 0
	err := pgx.BeginFunc(ctx, w.Pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, aggregate_type, aggregate_id, event_type, attempts
			FROM outbox
			WHERE processed_at IS NULL AND dead_at IS NULL AND next_attempt_at <= now()
			ORDER BY id
			LIMIT $1
			FOR UPDATE SKIP LOCKED`, w.BatchSize)
		if err != nil {
			return err
		}
		events, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Event, error) {
			var e Event
			err := r.Scan(&e.ID, &e.AggregateType, &e.AggregateID, &e.EventType, &e.Attempts)
			return e, err
		})
		if err != nil {
			return err
		}
		claimed = len(events)

		// Coalesce: handlers re-read current state, so several events for
		// the same aggregate in one batch need only one handler call.
		results := map[string]error{}
		for _, ev := range events {
			key := ev.AggregateType + "/" + ev.AggregateID
			herr, seen := results[key]
			if !seen {
				herr = w.handleOne(ctx, ev)
				results[key] = herr
			}
			if err := w.record(ctx, tx, ev, herr); err != nil {
				return err
			}
		}
		return nil
	})
	return claimed, err
}

func (w *Worker) handleOne(ctx context.Context, ev Event) (err error) {
	hctx, cancel := context.WithTimeout(ctx, w.HandlerTimeout)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("handler panic: %v", r)
		}
	}()
	return w.Handle(hctx, ev)
}

func (w *Worker) record(ctx context.Context, tx pgx.Tx, ev Event, herr error) error {
	if herr == nil {
		_, err := tx.Exec(ctx, `UPDATE outbox SET processed_at = now(), last_error = NULL WHERE id = $1`, ev.ID)
		return err
	}
	attempt := ev.Attempts + 1
	if attempt >= w.MaxAttempts || errors.Is(herr, ErrPermanent) {
		w.Log.Error("outbox event dead-lettered", "id", ev.ID, "aggregate", ev.AggregateID, "attempts", attempt, "err", herr)
		_, err := tx.Exec(ctx,
			`UPDATE outbox SET attempts = $2, last_error = $3, dead_at = now() WHERE id = $1`,
			ev.ID, attempt, herr.Error())
		return err
	}
	delay := w.Backoff(attempt)
	w.Log.Warn("outbox event failed, will retry", "id", ev.ID, "aggregate", ev.AggregateID,
		"attempt", attempt, "retry_in", delay, "err", herr)
	_, err := tx.Exec(ctx,
		`UPDATE outbox SET attempts = $2, last_error = $3, next_attempt_at = now() + $4 * interval '1 millisecond' WHERE id = $1`,
		ev.ID, attempt, herr.Error(), delay.Milliseconds())
	return err
}

// Prune deletes processed events older than `olderThan`.
func (w *Worker) Prune(ctx context.Context, olderThan time.Duration) (int64, error) {
	tag, err := w.Pool.Exec(ctx,
		`DELETE FROM outbox WHERE processed_at IS NOT NULL AND processed_at < now() - $1 * interval '1 second'`, int64(olderThan.Seconds()))
	return tag.RowsAffected(), err
}

func (w *Worker) defaults() {
	if w.BatchSize <= 0 {
		w.BatchSize = 100
	}
	if w.PollInterval <= 0 {
		w.PollInterval = 500 * time.Millisecond
	}
	if w.MaxAttempts <= 0 {
		w.MaxAttempts = 10
	}
	if w.HandlerTimeout <= 0 {
		w.HandlerTimeout = 10 * time.Second
	}
	if w.Backoff == nil {
		w.Backoff = DefaultBackoff
	}
	if w.Log == nil {
		w.Log = slog.Default()
	}
}

// Stats describes indexing lag; exposed on /readyz so you can watch the
// backlog drain after an Elasticsearch outage.
type Stats struct {
	Pending          int64   `json:"pending"`
	Dead             int64   `json:"dead"`
	OldestPendingSec float64 `json:"oldest_pending_seconds"`
}

func ReadStats(ctx context.Context, pool *pgxpool.Pool) (Stats, error) {
	var s Stats
	err := pool.QueryRow(ctx, `
		SELECT
		  count(*) FILTER (WHERE processed_at IS NULL AND dead_at IS NULL),
		  count(*) FILTER (WHERE dead_at IS NOT NULL),
		  COALESCE(EXTRACT(EPOCH FROM now() - min(created_at)
		           FILTER (WHERE processed_at IS NULL AND dead_at IS NULL)), 0)::float8
		FROM outbox`).Scan(&s.Pending, &s.Dead, &s.OldestPendingSec)
	if err != nil {
		return Stats{}, err
	}
	return s, nil
}

// ErrPermanent can wrap a handler error to dead-letter immediately instead of
// retrying (for example, malformed event data that will never succeed).
var ErrPermanent = errors.New("permanent failure")
