package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"networkai/internal/cdc"
	"networkai/internal/config"
	"networkai/internal/indexer"
	"networkai/internal/metrics"
	"networkai/internal/outbox"
	"networkai/internal/postcache"
	"networkai/internal/search"
	"networkai/internal/stats"
	"networkai/internal/store"
	"networkai/internal/timeline"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/twmb/franz-go/pkg/kgo"
)

// Partitions of the post-events topic: the unit of parallelism per consumer
// group (at most this many consumers of one group do useful work).
const eventPartitions = 12

// runRelay streams outbox inserts from the WAL to the post-events topic.
func runRelay(ctx context.Context, cfg config.Config, pool *pgxpool.Pool, log *slog.Logger) error {
	m := metrics.New()
	m.RegisterOutboxBacklog(pool) // includes replication slot lag
	go serveMetrics(ctx, cfg.MetricsAddr, m, log)

	if err := retry(ctx, log, "kafka", func() error {
		return cdc.EnsureTopics(ctx, cfg.KafkaBrokers, cdc.Topic, eventPartitions)
	}); err != nil {
		return err
	}
	prod, err := kgo.NewClient(kgo.SeedBrokers(cfg.KafkaBrokers...))
	if err != nil {
		return err
	}
	defer prod.Close()
	r := &cdc.Relay{ReplicationURL: cfg.DatabaseURL, Slot: "networkai_relay", Pool: pool, Producer: prod, Log: log,
		OnPublished: m.ObserveRelay}
	if err := r.EnsureSlot(ctx); err != nil {
		return err
	}
	// A dropped replication connection is retried; the slot makes it resume
	// exactly where PostgreSQL last saw a confirmation.
	for {
		err := r.Run(ctx)
		if ctx.Err() != nil {
			return nil
		}
		log.Error("relay stopped; restarting", "err", err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(2 * time.Second):
		}
	}
}

// runConsumer runs one consumer group. Each group is its own process so a
// failing dependency, a slow job or a deploy affects only that group.
func runConsumer(ctx context.Context, cfg config.Config, group string, st *store.Store, pool *pgxpool.Pool,
	es *search.Client, log *slog.Logger) error {
	m := metrics.New()
	go serveMetrics(ctx, cfg.MetricsAddr, m, log)

	var handle outbox.Handler
	var unavailable func(error) bool
	fromStart := false
	switch group {
	case "search-indexer":
		if err := retry(ctx, log, "elasticsearch", func() error { return es.EnsureIndex(ctx) }); err != nil {
			return err
		}
		ix := &indexer.Indexer{Posts: st, Index: es, Log: log, Embed: loadEmbedder(cfg, log)}
		handle = ix.Handle
		unavailable = func(err error) bool { return errors.Is(err, search.ErrUnavailable) }
	case "timeline-fanout":
		tl := openTimelines(cfg, st, m, log)
		if tl == nil {
			return errors.New("timeline-fanout needs REDIS_URL")
		}
		handle = tl.Handle
		unavailable = func(err error) bool { return errors.Is(err, timeline.ErrUnavailable) }
	case "post-stats":
		// A team that joined later: starts from the beginning of the log.
		handle = (&stats.Consumer{Pool: pool}).Handle
		fromStart = true
	case "cache-invalidation":
		// Second invalidation path for the post cache (ADR 0008): the API
		// deletes the key after a write, but if it dies between commit and
		// DEL, this group still invalidates from the log. Duplicates are
		// harmless.
		ro, err := redis.ParseURL(cfg.RedisURL)
		if err != nil {
			return fmt.Errorf("cache-invalidation needs REDIS_URL: %w", err)
		}
		pc := &postcache.Cache{Redis: redis.NewClient(ro)}
		handle = func(ctx context.Context, ev outbox.Event) error {
			id, err := uuid.Parse(ev.AggregateID)
			if err != nil || ev.AggregateType != store.AggregatePost {
				return nil // not a post event; nothing cached under it
			}
			if err := pc.Invalidate(ctx, id); err != nil {
				return fmt.Errorf("%w: %v", timeline.ErrUnavailable, err)
			}
			return nil
		}
		unavailable = func(err error) bool { return errors.Is(err, timeline.ErrUnavailable) }
	default:
		return fmt.Errorf("unknown consumer group %q (search-indexer | timeline-fanout | post-stats | cache-invalidation)", group)
	}
	c := &cdc.Consumer{Brokers: cfg.KafkaBrokers, Group: group, Handle: handle, IsUnavailable: unavailable,
		FromStart: fromStart, Log: log, OnEvent: m.ObserveConsumer}
	return c.Run(ctx)
}

func retry(ctx context.Context, log *slog.Logger, what string, fn func() error) error {
	for {
		err := fn()
		if err == nil || ctx.Err() != nil {
			return err
		}
		log.Warn(what+" not ready, retrying", "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}
