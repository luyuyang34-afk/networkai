package cdc_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"networkai/internal/cdc"
	"networkai/internal/outbox"
	"networkai/internal/stats"
	"networkai/internal/store"
	"networkai/internal/testutil"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type env struct {
	t       *testing.T
	pool    *pgxpool.Pool
	st      *store.Store
	brokers []string
	topic   string
	slot    string
	author  store.User
}

func setup(t *testing.T) *env {
	brokers, topic := testutil.Kafka(t)
	pool := testutil.NewDB(t)
	e := &env{t: t, pool: pool, st: store.New(pool), brokers: brokers, topic: topic,
		slot: fmt.Sprintf("test_slot_%d", rand.Uint64())}
	if err := cdc.EnsureTopics(context.Background(), brokers, topic, 4); err != nil {
		t.Fatal(err)
	}
	// Delete this test's topics (and dead-letter topics) afterwards: brokers
	// reserve memory per partition, and leftovers eventually exhaust it.
	t.Cleanup(func() { deleteTopics(brokers, topic) })
	// Replication slots are cluster-wide and block DROP DATABASE: remove ours
	// first (cleanups run in reverse order, so this runs before the DB drop).
	t.Cleanup(func() {
		ctx := context.Background()
		for i := 0; i < 50; i++ {
			_, err := pool.Exec(ctx, `SELECT pg_drop_replication_slot($1) FROM pg_replication_slots WHERE slot_name = $1`, e.slot)
			if err == nil {
				return
			}
			time.Sleep(100 * time.Millisecond) // slot still active while the relay disconnects
		}
	})
	u, err := e.st.CreateUser(context.Background(), "author", "h")
	if err != nil {
		t.Fatal(err)
	}
	e.author = u
	return e
}

// startRelay runs a relay until the returned stop function is called.
func (e *env) startRelay() (stop func()) {
	e.t.Helper()
	prod, err := kgo.NewClient(kgo.SeedBrokers(e.brokers...))
	if err != nil {
		e.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &cdc.Relay{ReplicationURL: e.pool.Config().ConnString(), Slot: e.slot, Topic: e.topic,
		Pool: e.pool, Producer: prod, Log: quiet}
	if err := r.EnsureSlot(ctx); err != nil { // capture starts when the slot exists
		e.t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	return func() {
		cancel()
		if err := <-done; err != nil {
			e.t.Errorf("relay: %v", err)
		}
		prod.Close()
	}
}

func (e *env) post(msg string) uuid.UUID {
	e.t.Helper()
	p, err := e.st.CreatePost(context.Background(), store.NewPost{ID: uuid.New(), AuthorID: e.author.ID, Message: msg, MediaType: "none"})
	if err != nil {
		e.t.Fatal(err)
	}
	return p.ID
}

// readAll consumes the topic from the start (no group) until n records.
func (e *env) readAll(n int) []*kgo.Record {
	e.t.Helper()
	cl, err := kgo.NewClient(kgo.SeedBrokers(e.brokers...), kgo.ConsumeTopics(e.topic),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	if err != nil {
		e.t.Fatal(err)
	}
	defer cl.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var out []*kgo.Record
	for len(out) < n {
		f := cl.PollFetches(ctx)
		if ctx.Err() != nil {
			e.t.Fatalf("got %d of %d records before timeout", len(out), n)
		}
		out = append(out, f.Records()...)
	}
	return out
}

func decode(t *testing.T, r *kgo.Record) cdc.Event {
	var ev cdc.Event
	if err := json.Unmarshal(r.Value, &ev); err != nil {
		t.Fatal(err)
	}
	return ev
}

func TestRelayPublishesCommittedChangesInPerPostOrder(t *testing.T) {
	e := setup(t)
	stop := e.startRelay()
	defer stop()
	ctx := context.Background()

	a := e.post("a")
	b := e.post("b")
	if _, err := e.st.UpdatePostMessage(ctx, a, e.author.ID, "a edited"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.DeletePost(ctx, a, e.author.ID); err != nil {
		t.Fatal(err)
	}
	// A rolled-back transaction must never reach the log.
	if _, err := e.st.CreatePost(ctx, store.NewPost{ID: uuid.New(), AuthorID: e.author.ID,
		Message: strings.Repeat("x", 5001), MediaType: "none"}); err == nil {
		t.Fatal("expected CHECK violation")
	}
	recs := e.readAll(4)

	partitionOf := map[string]int32{}
	var aIDs []int64
	for _, r := range recs {
		ev := decode(t, r)
		if p, ok := partitionOf[ev.AggregateID]; ok && p != r.Partition {
			t.Fatalf("events for %s split across partitions %d and %d", ev.AggregateID, p, r.Partition)
		}
		partitionOf[ev.AggregateID] = r.Partition
		if ev.AggregateID == a.String() {
			aIDs = append(aIDs, ev.ID)
		}
	}
	if len(partitionOf) != 2 || partitionOf[b.String()] == 0 && partitionOf[a.String()] == 0 && len(recs) != 4 {
		t.Fatalf("unexpected records: %d for %v", len(recs), partitionOf)
	}
	if len(aIDs) != 3 || !(aIDs[0] < aIDs[1] && aIDs[1] < aIDs[2]) {
		t.Fatalf("post a's events out of order: %v", aIDs)
	}
	// Relayed rows are removed from the outbox table.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var n int
		e.pool.QueryRow(ctx, `SELECT count(*) FROM outbox`).Scan(&n) //nolint:errcheck
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("outbox still has %d rows", n)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Changes committed while the relay is down are delivered when it restarts:
// the replication slot keeps the WAL from the last confirmed position.
func TestRelayResumesAfterRestartWithoutLoss(t *testing.T) {
	e := setup(t)
	stop := e.startRelay()
	e.post("before")
	e.readAll(1)
	stop()

	for i := 0; i < 5; i++ {
		e.post(fmt.Sprintf("while down %d", i))
	}
	stop = e.startRelay()
	defer stop()
	recs := e.readAll(6)
	if len(recs) < 6 {
		t.Fatalf("got %d records, want 6", len(recs))
	}
}

// Group names are cluster-wide, so every test uses its own (prefixed with
// its unique topic); reusing names across runs made joins wait for stale
// members and made this suite flaky.
//
// One group's dependency being down must not slow any other group.
func TestConsumerGroupsAreIndependent(t *testing.T) {
	e := setup(t)
	stop := e.startRelay()
	defer stop()

	var down atomic.Bool
	down.Store(true)
	errDown := errors.New("elasticsearch unavailable")
	var searchDone, timelineDone atomic.Int32

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run := func(group string, h outbox.Handler) {
		c := &cdc.Consumer{Brokers: e.brokers, Topic: e.topic, Group: e.topic + "-" + group, FromStart: true, Log: quiet,
			MaxPause: 500 * time.Millisecond,
			Handle:   h, IsUnavailable: func(err error) bool { return errors.Is(err, errDown) }}
		go c.Run(ctx) //nolint:errcheck
	}
	run("search", func(context.Context, outbox.Event) error {
		if down.Load() {
			return errDown
		}
		searchDone.Add(1)
		return nil
	})
	run("timeline", func(context.Context, outbox.Event) error {
		timelineDone.Add(1)
		return nil
	})

	for i := 0; i < 10; i++ {
		e.post(fmt.Sprintf("p%d", i))
	}
	waitFor(t, func() bool { return timelineDone.Load() == 10 }, "timeline group to process all 10 while search is down")
	if searchDone.Load() != 0 {
		t.Fatalf("search processed %d while its dependency was down", searchDone.Load())
	}
	down.Store(false)
	waitFor(t, func() bool { return searchDone.Load() == 10 }, "search group to catch up after recovery")
}

// A consumer added later replays history from the log, and duplicate
// deliveries do not double-count.
func TestNewGroupReplaysHistoryIdempotently(t *testing.T) {
	e := setup(t)
	stop := e.startRelay()
	defer stop()
	ctx := context.Background()
	ids := []uuid.UUID{e.post("one"), e.post("two"), e.post("three")}
	if _, err := e.st.UpdatePostMessage(ctx, ids[0], e.author.ID, "one edited"); err != nil {
		t.Fatal(err)
	}
	e.readAll(4) // all events are in the log before any stats consumer exists

	sc := &stats.Consumer{Pool: e.pool}
	var handled atomic.Int32
	h := func(ctx context.Context, ev outbox.Event) error { handled.Add(1); return sc.Handle(ctx, ev) }
	// Two independent groups both replay everything into the same table:
	// 8 deliveries, but each post must count once.
	for _, g := range []string{"stats-v1", "stats-v2"} {
		gctx, cancel := context.WithCancel(ctx)
		c := &cdc.Consumer{Brokers: e.brokers, Topic: e.topic, Group: e.topic + "-" + g, FromStart: true, Log: quiet, Handle: h}
		go c.Run(gctx) //nolint:errcheck
		defer cancel()
	}
	waitFor(t, func() bool { return handled.Load() == 8 }, "both groups to replay 4 events each")
	counts, err := stats.Daily(ctx, e.pool)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, n := range counts {
		total += n
	}
	if total != 3 {
		t.Fatalf("counted %d posts, want 3 (8 deliveries deduplicated by post id)", total)
	}
}

// A record that keeps failing goes to the dead-letter topic and the
// partition moves on.
func TestPoisonRecordIsDeadLetteredAndPartitionContinues(t *testing.T) {
	e := setup(t)
	stop := e.startRelay()
	defer stop()
	bad := e.post("poison")
	good := e.post("fine")

	var mu sync.Mutex
	handledGood := false
	c := &cdc.Consumer{Brokers: e.brokers, Topic: e.topic, Group: e.topic + "-g", FromStart: true, Log: quiet, MaxAttempts: 2,
		Handle: func(_ context.Context, ev outbox.Event) error {
			if ev.AggregateID == bad.String() {
				return errors.New("cannot index this document")
			}
			if ev.AggregateID == good.String() {
				mu.Lock()
				handledGood = true
				mu.Unlock()
			}
			return nil
		}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx) //nolint:errcheck
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return handledGood }, "good record to be handled")

	dl, err := kgo.NewClient(kgo.SeedBrokers(e.brokers...), kgo.ConsumeTopics(e.topic+".dlq."+e.topic+"-g"),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	if err != nil {
		t.Fatal(err)
	}
	defer dl.Close()
	fctx, fcancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer fcancel()
	recs := dl.PollFetches(fctx).Records()
	if len(recs) != 1 || string(recs[0].Key) != bad.String() || len(recs[0].Headers) == 0 {
		t.Fatalf("dead-letter records = %d", len(recs))
	}
}

func deleteTopics(brokers []string, prefix string) {
	cl, err := kgo.NewClient(kgo.SeedBrokers(brokers...))
	if err != nil {
		return
	}
	defer cl.Close()
	adm := kadm.NewClient(cl)
	ctx := context.Background()
	topics, err := adm.ListTopics(ctx)
	if err != nil {
		return
	}
	var del []string
	for _, name := range topics.Names() {
		if strings.HasPrefix(name, prefix) {
			del = append(del, name)
		}
	}
	adm.DeleteTopics(ctx, del...) //nolint:errcheck
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A group restarting must not skip records produced while it was down, even
// on partitions where it has never committed an offset. With the reset
// policy "start at the end" (Kafka's default auto.offset.reset=latest) such
// partitions jump past those records: found when a probe post was never
// indexed after the consumers restarted.
func TestRestartedGroupDoesNotSkipRecordsOnUncommittedPartitions(t *testing.T) {
	e := setup(t)
	stop := e.startRelay()
	defer stop()
	group := e.topic + "-restart"
	var mu sync.Mutex
	seen := map[string]bool{}
	run := func() context.CancelFunc {
		ctx, cancel := context.WithCancel(context.Background())
		c := &cdc.Consumer{Brokers: e.brokers, Topic: e.topic, Group: group, Log: quiet,
			Handle: func(_ context.Context, ev outbox.Event) error {
				mu.Lock()
				seen[ev.AggregateID] = true
				mu.Unlock()
				return nil
			}}
		go c.Run(ctx) //nolint:errcheck
		return cancel
	}
	count := func() int { mu.Lock(); defer mu.Unlock(); return len(seen) }

	cancel := run()
	e.post("first") // commits an offset on one partition only
	waitFor(t, func() bool { return count() == 1 }, "first record")
	time.Sleep(500 * time.Millisecond) // let the commit land
	cancel()

	for i := 0; i < 20; i++ { // spread across all 4 partitions while the group is down
		e.post(fmt.Sprintf("while down %d", i))
	}
	cancel = run()
	defer cancel()
	start := time.Now()
	deadline := start.Add(70 * time.Second)
	for count() < 21 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("after restart: %d/21 records, %.1fs", count(), time.Since(start).Seconds())
	if count() != 21 {
		t.Fatal("records produced while the group was down were skipped")
	}
}
