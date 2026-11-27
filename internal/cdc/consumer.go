package cdc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"networkai/internal/outbox"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
)

// Consumer runs one consumer group over the post-events topic.
//
// Each group keeps its own committed offsets, so groups never wait for each
// other: Elasticsearch being down stalls only "search-indexer"; a celebrity
// fan-out occupies only "timeline-fanout". Within a group, partitions are
// processed in parallel and records of one partition (= one key set) in
// order, which preserves per-post ordering.
type Consumer struct {
	Brokers []string
	Group   string
	Topic   string // default "post-events"
	Handle  outbox.Handler
	// IsUnavailable classifies "dependency down" errors: the record is
	// retried with backoff until it succeeds, without spending its retry
	// budget (same rule as ADR 0003).
	IsUnavailable func(error) bool
	MaxAttempts   int
	// MaxPause caps the backoff while a dependency is down (default 30s).
	MaxPause time.Duration
	// FromStart is kept for readability at call sites (the stats group
	// "joins later and replays"); every durable group starts at the
	// beginning of the log on partitions it has no committed offset for.
	FromStart bool
	Log       *slog.Logger
	// OnEvent is optional observability: result and end-to-end lag
	// (time from the outbox insert to the end of handling).
	OnEvent func(group, result string, lag time.Duration)
}

func (c *Consumer) Run(ctx context.Context) error {
	if c.Topic == "" {
		c.Topic = Topic
	}
	// Partitions without a committed offset start at the BEGINNING. With
	// "at the end" (Kafka's default auto.offset.reset=latest), a group that
	// restarts skips every record produced while it was down on partitions
	// it had not yet committed: silent data loss, found when a probe post
	// was never indexed. Replaying instead is safe because handlers are
	// idempotent.
	reset := kgo.NewOffset().AtStart()
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(c.Brokers...),
		kgo.ConsumerGroup(c.Group),
		kgo.ConsumeTopics(c.Topic),
		kgo.ConsumeResetOffset(reset),
		kgo.DisableAutoCommit(), // commit only after handling: at-least-once
		kgo.BlockRebalanceOnPoll(),
		kgo.FetchMaxWait(250*time.Millisecond),
	)
	if err != nil {
		return err
	}
	defer func() {
		// Leave the group explicitly on shutdown. Without this the
		// coordinator waits for the departed member's session to expire
		// before reassigning its partitions: a 60 s stall on every restart
		// or deploy (measured). AllowRebalance first, because
		// BlockRebalanceOnPoll would otherwise hold the leave back.
		cl.AllowRebalance()
		lctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := cl.LeaveGroupContext(lctx); err != nil {
			c.Log.Warn("leave group failed", "group", c.Group, "err", err)
		}
		cl.Close()
	}()
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 5
	}
	if c.MaxPause <= 0 {
		c.MaxPause = 30 * time.Second
	}
	// The dead-letter topic is created explicitly rather than relying on
	// broker auto-creation, which production clusters usually disable.
	if err := EnsureTopics(ctx, c.Brokers, c.dlqTopic(), 1); err != nil {
		return fmt.Errorf("create dead-letter topic: %w", err)
	}
	if c.IsUnavailable == nil {
		c.IsUnavailable = func(error) bool { return false }
	}
	c.Log.Info("consumer started", "group", c.Group)

	for {
		fetches := cl.PollRecords(ctx, 500)
		if ctx.Err() != nil {
			return nil
		}
		fetches.EachError(func(t string, p int32, err error) {
			c.Log.Warn("fetch error", "group", c.Group, "partition", p, "err", err)
		})
		var wg sync.WaitGroup
		fetches.EachPartition(func(p kgo.FetchTopicPartition) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for _, rec := range p.Records {
					if ctx.Err() != nil {
						return
					}
					c.process(ctx, cl, rec)
				}
			}()
		})
		wg.Wait()
		if ctx.Err() != nil {
			return nil
		}
		if err := cl.CommitUncommittedOffsets(ctx); err != nil {
			c.Log.Warn("commit failed; records may be redelivered", "group", c.Group, "err", err)
		}
		cl.AllowRebalance()
	}
}

func (c *Consumer) process(ctx context.Context, cl *kgo.Client, rec *kgo.Record) {
	var ev Event
	if err := json.Unmarshal(rec.Value, &ev); err != nil {
		c.deadLetter(ctx, cl, rec, fmt.Errorf("decode: %w", err))
		return
	}
	oe := outbox.Event{ID: ev.ID, AggregateType: ev.AggregateType, AggregateID: ev.AggregateID, EventType: ev.EventType}
	pause := time.Second
	for attempt := 1; ; {
		err := c.Handle(ctx, oe)
		if err == nil {
			c.observe("ok", ev)
			return
		}
		if ctx.Err() != nil {
			return
		}
		if c.IsUnavailable(err) {
			// Dependency down: wait and retry this record; the partition
			// (and only this group) stalls until it recovers.
			c.observe("paused", ev)
			c.Log.Warn("dependency unavailable; retrying", "group", c.Group, "err", err, "in", pause)
			sleep(ctx, pause)
			pause = min(pause*2, c.MaxPause)
			continue
		}
		if attempt >= c.MaxAttempts || errors.Is(err, outbox.ErrPermanent) {
			c.deadLetter(ctx, cl, rec, err)
			return
		}
		c.observe("retry", ev)
		sleep(ctx, time.Duration(attempt)*200*time.Millisecond)
		attempt++
	}
}

// deadLetter parks a record that keeps failing on "<topic>.dlq.<group>" with
// the error attached, so one bad event cannot block its partition forever.
func (c *Consumer) deadLetter(ctx context.Context, cl *kgo.Client, rec *kgo.Record, cause error) {
	c.Log.Error("dead-lettering event", "group", c.Group, "key", string(rec.Key), "err", cause)
	dlq := &kgo.Record{Topic: c.dlqTopic(), Key: rec.Key, Value: rec.Value,
		Headers: []kgo.RecordHeader{{Key: "error", Value: []byte(cause.Error())},
			{Key: "offset", Value: []byte(strconv.FormatInt(rec.Offset, 10))}}}
	if err := cl.ProduceSync(ctx, dlq).FirstErr(); err != nil {
		c.Log.Error("dead-letter produce failed", "err", err)
	}
	c.observe("dead", Event{})
}

func (c *Consumer) dlqTopic() string { return c.Topic + ".dlq." + c.Group }

func (c *Consumer) observe(result string, ev Event) {
	if c.OnEvent == nil {
		return
	}
	var lag time.Duration
	if !ev.CreatedAt.IsZero() {
		lag = time.Since(ev.CreatedAt)
	}
	c.OnEvent(c.Group, result, lag)
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// EnsureTopics creates the event topic (partitioned by post id) if missing.
// 7-day retention is what makes replay possible for consumers added later.
func EnsureTopics(ctx context.Context, brokers []string, topic string, partitions int32) error {
	cl, err := kgo.NewClient(kgo.SeedBrokers(brokers...))
	if err != nil {
		return err
	}
	defer cl.Close()
	adm := kadm.NewClient(cl)
	retention := strconv.FormatInt((7 * 24 * time.Hour).Milliseconds(), 10)
	resp, err := adm.CreateTopics(ctx, partitions, 1, map[string]*string{"retention.ms": &retention}, topic)
	if err != nil {
		return err
	}
	for _, r := range resp {
		if r.Err != nil && !errors.Is(r.Err, kerrTopicExists) {
			return r.Err
		}
	}
	return nil
}

var kerrTopicExists = kerr.TopicAlreadyExists
