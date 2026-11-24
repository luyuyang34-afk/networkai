package outbox_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"networkai/internal/outbox"
	"networkai/internal/testutil"

	"github.com/jackc/pgx/v5/pgxpool"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func insertEvents(t *testing.T, pool *pgxpool.Pool, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if _, err := pool.Exec(context.Background(),
			`INSERT INTO outbox (aggregate_type, aggregate_id, event_type) VALUES ('post', $1, 'post.changed')`, id); err != nil {
			t.Fatal(err)
		}
	}
}

func noBackoff(int) time.Duration { return 0 }

func TestEventIsMarkedProcessedOnSuccess(t *testing.T) {
	pool := testutil.NewDB(t)
	insertEvents(t, pool, "a", "b")
	var handled []string
	w := &outbox.Worker{Pool: pool, Log: quiet, Handle: func(_ context.Context, ev outbox.Event) error {
		handled = append(handled, ev.AggregateID)
		return nil
	}}
	n, err := w.ProcessBatch(context.Background())
	if err != nil || n != 2 {
		t.Fatalf("ProcessBatch = %d, %v", n, err)
	}
	if fmt.Sprint(handled) != "[a b]" {
		t.Fatalf("handled %v, want [a b] in insertion order", handled)
	}
	stats, _ := outbox.ReadStats(context.Background(), pool)
	if stats.Pending != 0 {
		t.Fatalf("pending = %d after success", stats.Pending)
	}
	if n, _ := w.ProcessBatch(context.Background()); n != 0 {
		t.Fatalf("processed events were claimed again (%d)", n)
	}
}

// A failing handler (e.g. Elasticsearch down) must not lose the event: it is
// retried until it succeeds.
func TestFailedEventIsRetriedUntilSuccess(t *testing.T) {
	pool := testutil.NewDB(t)
	insertEvents(t, pool, "a")
	calls := 0
	w := &outbox.Worker{Pool: pool, Log: quiet, Backoff: noBackoff, MaxAttempts: 10,
		Handle: func(context.Context, outbox.Event) error {
			calls++
			if calls <= 3 {
				return errors.New("elasticsearch unavailable")
			}
			return nil
		}}
	for i := 0; i < 4; i++ {
		if _, err := w.ProcessBatch(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	var attempts int
	var processed bool
	pool.QueryRow(context.Background(),
		`SELECT attempts, processed_at IS NOT NULL FROM outbox`).Scan(&attempts, &processed) //nolint:errcheck
	if calls != 4 || attempts != 3 || !processed {
		t.Fatalf("calls=%d attempts=%d processed=%v; want 4, 3, true", calls, attempts, processed)
	}
}

func TestBackoffDelaysRetry(t *testing.T) {
	pool := testutil.NewDB(t)
	insertEvents(t, pool, "a")
	w := &outbox.Worker{Pool: pool, Log: quiet,
		Backoff: func(int) time.Duration { return time.Hour },
		Handle:  func(context.Context, outbox.Event) error { return errors.New("boom") }}
	w.ProcessBatch(context.Background()) //nolint:errcheck
	if n, _ := w.ProcessBatch(context.Background()); n != 0 {
		t.Fatal("event retried before its backoff elapsed")
	}
}

func TestEventIsDeadLetteredAfterMaxAttempts(t *testing.T) {
	pool := testutil.NewDB(t)
	insertEvents(t, pool, "a")
	w := &outbox.Worker{Pool: pool, Log: quiet, Backoff: noBackoff, MaxAttempts: 3,
		Handle: func(context.Context, outbox.Event) error { return errors.New("always fails") }}
	for i := 0; i < 5; i++ {
		w.ProcessBatch(context.Background()) //nolint:errcheck
	}
	stats, _ := outbox.ReadStats(context.Background(), pool)
	if stats.Dead != 1 || stats.Pending != 0 {
		t.Fatalf("stats = %+v; want 1 dead, 0 pending", stats)
	}
}

func TestPermanentErrorSkipsRetries(t *testing.T) {
	pool := testutil.NewDB(t)
	insertEvents(t, pool, "a")
	w := &outbox.Worker{Pool: pool, Log: quiet, Backoff: noBackoff, MaxAttempts: 10,
		Handle: func(context.Context, outbox.Event) error {
			return fmt.Errorf("%w: malformed", outbox.ErrPermanent)
		}}
	w.ProcessBatch(context.Background()) //nolint:errcheck
	if stats, _ := outbox.ReadStats(context.Background(), pool); stats.Dead != 1 {
		t.Fatalf("permanent error should dead-letter immediately; stats = %+v", stats)
	}
}

func TestHandlerPanicIsRetriedNotFatal(t *testing.T) {
	pool := testutil.NewDB(t)
	insertEvents(t, pool, "a")
	w := &outbox.Worker{Pool: pool, Log: quiet, Backoff: noBackoff,
		Handle: func(context.Context, outbox.Event) error { panic("bug") }}
	if _, err := w.ProcessBatch(context.Background()); err != nil {
		t.Fatalf("panic escaped the worker: %v", err)
	}
	if stats, _ := outbox.ReadStats(context.Background(), pool); stats.Pending != 1 {
		t.Fatalf("panicking event should stay pending for retry; stats = %+v", stats)
	}
}

// Several events for the same post in one batch need only one handler call,
// because the handler re-reads current state.
func TestEventsForSameAggregateAreCoalesced(t *testing.T) {
	pool := testutil.NewDB(t)
	insertEvents(t, pool, "a", "a", "a", "b")
	calls := map[string]int{}
	w := &outbox.Worker{Pool: pool, Log: quiet, Handle: func(_ context.Context, ev outbox.Event) error {
		calls[ev.AggregateID]++
		return nil
	}}
	if n, _ := w.ProcessBatch(context.Background()); n != 4 {
		t.Fatalf("claimed %d, want 4", n)
	}
	if calls["a"] != 1 || calls["b"] != 1 {
		t.Fatalf("calls = %v; want one per aggregate", calls)
	}
	if stats, _ := outbox.ReadStats(context.Background(), pool); stats.Pending != 0 {
		t.Fatalf("all coalesced events must be marked processed; stats = %+v", stats)
	}
}

// Several workers running concurrently must handle every event exactly once
// and, thanks to FOR UPDATE SKIP LOCKED, make progress in parallel.
func TestConcurrentWorkersClaimDisjointEvents(t *testing.T) {
	pool := testutil.NewDB(t)
	const total = 300
	ids := make([]string, total)
	for i := range ids {
		ids[i] = fmt.Sprintf("post-%d", i)
	}
	insertEvents(t, pool, ids...)

	var mu sync.Mutex
	seen := map[string]int{}
	var inFlight, maxInFlight atomic.Int32
	handle := func(_ context.Context, ev outbox.Event) error {
		cur := inFlight.Add(1)
		for {
			m := maxInFlight.Load()
			if cur <= m || maxInFlight.CompareAndSwap(m, cur) {
				break
			}
		}
		time.Sleep(time.Millisecond)
		inFlight.Add(-1)
		mu.Lock()
		seen[ev.AggregateID]++
		mu.Unlock()
		return nil
	}

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := &outbox.Worker{Pool: pool, Log: quiet, Handle: handle, BatchSize: 10}
			for {
				n, err := w.ProcessBatch(context.Background())
				if err != nil {
					t.Error(err)
					return
				}
				if n == 0 {
					return
				}
			}
		}()
	}
	wg.Wait()

	if len(seen) != total {
		t.Fatalf("handled %d distinct events, want %d", len(seen), total)
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("event %s handled %d times", id, n)
		}
	}
	// Correctness alone would also hold with plain FOR UPDATE (workers would
	// just queue behind each other's locks). SKIP LOCKED is what lets them
	// work in parallel, so assert that they actually overlapped.
	if maxInFlight.Load() < 2 {
		t.Fatalf("workers never ran concurrently (max in flight %d); are they blocking on each other's locks?", maxInFlight.Load())
	}
}
