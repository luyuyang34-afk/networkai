// Package stats is the "post-stats" consumer: posts created per day.
//
// It exists to show two things a log enables:
//   - a team can start consuming later and replay history from the log, with
//     no change to producers;
//   - at-least-once delivery means duplicates, so a consumer with side
//     effects that are not naturally idempotent (incrementing a counter)
//     must deduplicate. Here the dedupe key is the post id, not the event
//     id: a post has several events (create, edits, delete) but must be
//     counted exactly once, including on replay.
package stats

import (
	"context"
	"errors"
	"fmt"

	"networkai/internal/outbox"
	"networkai/internal/store"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Consumer struct{ Pool *pgxpool.Pool }

func (c *Consumer) Handle(ctx context.Context, ev outbox.Event) error {
	if ev.AggregateType != store.AggregatePost {
		return nil
	}
	id, err := uuid.Parse(ev.AggregateID)
	if err != nil {
		return fmt.Errorf("%w: bad post id %q", outbox.ErrPermanent, ev.AggregateID)
	}
	return pgx.BeginFunc(ctx, c.Pool, func(tx pgx.Tx) error {
		// Dedupe and count in one transaction: either both happen or neither.
		tag, err := tx.Exec(ctx, `INSERT INTO stats_counted_posts (post_id) VALUES ($1) ON CONFLICT DO NOTHING`, id)
		if err != nil || tag.RowsAffected() == 0 {
			return err // already counted (duplicate delivery or replay)
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO daily_post_counts (day, posts)
			SELECT created_at::date, 1 FROM posts WHERE id = $1
			ON CONFLICT (day) DO UPDATE SET posts = daily_post_counts.posts + 1`, id)
		return err
	})
}

// Daily returns counts per day (for tests and the experiment).
func Daily(ctx context.Context, pool *pgxpool.Pool) (map[string]int64, error) {
	rows, err := pool.Query(ctx, `SELECT day::text, posts FROM daily_post_counts`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var d string
		var n int64
		if err := rows.Scan(&d, &n); err != nil {
			return nil, err
		}
		out[d] = n
	}
	return out, errors.Join(rows.Err())
}
