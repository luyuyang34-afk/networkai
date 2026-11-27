package postcache_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"networkai/internal/postcache"
	"networkai/internal/store"
	"networkai/internal/testutil"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// fakeStore is the source of truth; it counts loads and can block one.
type fakeStore struct {
	mu    sync.Mutex
	posts map[uuid.UUID]store.Post
	loads atomic.Int64
	delay time.Duration
	// hook runs inside a load, after reading the row and before returning.
	hook func()
}

func (f *fakeStore) GetPost(_ context.Context, id uuid.UUID) (store.Post, error) {
	f.loads.Add(1)
	f.mu.Lock()
	p, ok := f.posts[id]
	hook := f.hook
	f.mu.Unlock()
	time.Sleep(f.delay)
	if hook != nil {
		hook()
	}
	if !ok {
		return store.Post{}, store.ErrNotFound
	}
	return p, nil
}

func (f *fakeStore) set(p store.Post) {
	f.mu.Lock()
	f.posts[p.ID] = p
	f.mu.Unlock()
}

func setup(t *testing.T, opt postcache.Options) (*fakeStore, func() *postcache.Cache, store.Post) {
	rdb, prefix := testutil.Redis(t)
	p := store.Post{ID: uuid.New(), Author: "alice", Message: "v1", Version: 1}
	fs := &fakeStore{posts: map[uuid.UUID]store.Post{p.ID: p}}
	// Each call is a separate "API instance": own singleflight, shared Redis.
	newCache := func() *postcache.Cache {
		return &postcache.Cache{Redis: rdb, Store: fs, Opt: opt, Prefix: prefix}
	}
	return fs, newCache, p
}

func TestHitAfterMissAndInvalidate(t *testing.T) {
	ctx := context.Background()
	fs, newCache, p := setup(t, postcache.Protected)
	c := newCache()
	for i := 0; i < 5; i++ {
		got, err := c.GetPost(ctx, p.ID)
		if err != nil || got.Message != "v1" {
			t.Fatalf("got %+v, %v", got, err)
		}
	}
	if n := fs.loads.Load(); n != 1 {
		t.Fatalf("5 reads made %d loads, want 1", n)
	}
	fs.set(store.Post{ID: p.ID, Message: "v2", Version: 2})
	if err := c.Invalidate(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.GetPost(ctx, p.ID); got.Message != "v2" {
		t.Fatalf("after invalidate got %q, want v2", got.Message)
	}
}

func TestNotFoundIsCached(t *testing.T) {
	ctx := context.Background()
	fs, newCache, _ := setup(t, postcache.Protected)
	c := newCache()
	missing := uuid.New()
	for i := 0; i < 3; i++ {
		if _, err := c.GetPost(ctx, missing); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	}
	if n := fs.loads.Load(); n != 1 {
		t.Fatalf("3 reads of a missing post made %d loads, want 1", n)
	}
}

// stampede: 4 instances x 50 concurrent readers hit a cold hot key.
func stampede(t *testing.T, opt postcache.Options) int64 {
	ctx := context.Background()
	fs, newCache, p := setup(t, opt)
	fs.delay = 50 * time.Millisecond
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		c := newCache()
		for j := 0; j < 50; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if got, err := c.GetPost(ctx, p.ID); err != nil || got.Message != "v1" {
					t.Errorf("got %+v, %v", got, err)
				}
			}()
		}
	}
	wg.Wait()
	return fs.loads.Load()
}

func TestStampedeProtection(t *testing.T) {
	naive := stampede(t, postcache.Options{})
	sf := stampede(t, postcache.Options{Singleflight: true})
	full := stampede(t, postcache.Protected)
	t.Logf("database loads for 200 concurrent cold reads: naive=%d singleflight=%d singleflight+leases=%d", naive, sf, full)
	if naive < 100 {
		t.Errorf("naive made only %d loads; the test is not producing a stampede", naive)
	}
	if sf > 4 {
		t.Errorf("singleflight made %d loads, want <= 4 (one per instance)", sf)
	}
	if full != 1 {
		t.Errorf("leases made %d loads, want 1", full)
	}
}

// staleSetRace: a reader loads v1, a writer commits v2 and invalidates, and
// then the reader's fill lands. Returns what the cache serves afterwards.
func staleSetRace(t *testing.T, opt postcache.Options) string {
	ctx := context.Background()
	fs, newCache, p := setup(t, opt)
	reader, writer := newCache(), newCache()
	var once sync.Once
	fs.hook = func() {
		once.Do(func() { // the first load has read v1; now the write happens
			fs.set(store.Post{ID: p.ID, Message: "v2", Version: 2})
			if err := writer.Invalidate(ctx, p.ID); err != nil {
				t.Error(err)
			}
		})
	}
	if _, err := reader.GetPost(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	fs.hook = nil
	got, err := reader.GetPost(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got.Message
}

func TestLeasesPreventStaleSet(t *testing.T) {
	if got := staleSetRace(t, postcache.Options{Singleflight: true}); got != "v1" {
		t.Logf("without leases the race did not reproduce (got %q)", got)
	} else {
		t.Log("without leases: stale v1 stays cached after the write (expected)")
	}
	if got := staleSetRace(t, postcache.Protected); got != "v2" {
		t.Fatalf("with leases the cache serves %q after the write, want v2", got)
	}
}

// expiryMisses runs a steady reader across ~5 TTLs and counts misses.
func expiryMisses(t *testing.T, opt postcache.Options) int64 {
	ctx := context.Background()
	fs, newCache, p := setup(t, opt)
	fs.delay = 20 * time.Millisecond
	c := newCache()
	c.TTL = 300 * time.Millisecond
	var misses atomic.Int64
	c.OnResult = func(r string) {
		if r == "miss" {
			misses.Add(1)
		}
	}
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := c.GetPost(ctx, p.ID); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	return misses.Load()
}

func TestEarlyRefreshAvoidsExpiryMiss(t *testing.T) {
	without := expiryMisses(t, postcache.Options{Singleflight: true, Leases: true})
	with := expiryMisses(t, postcache.Protected)
	t.Logf("misses across 5 TTLs: without XFetch=%d, with XFetch=%d", without, with)
	if without < 4 {
		t.Fatalf("without XFetch only %d misses; the test is not producing expiries", without)
	}
	// XFetch is probabilistic: a refresh triggered in the last load-time
	// before expiry can still finish late, so allow one such miss.
	if with > 2 {
		t.Fatalf("with XFetch %d misses, want <= 2 (cold start + at most one late refresh)", with)
	}
}

func TestRedisDownFallsBackToStore(t *testing.T) {
	fs := &fakeStore{posts: map[uuid.UUID]store.Post{}}
	p := store.Post{ID: uuid.New(), Message: "v1"}
	fs.set(p)
	c := &postcache.Cache{Redis: redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1}),
		Store: fs, Opt: postcache.Protected}
	got, err := c.GetPost(context.Background(), p.ID)
	if err != nil || got.Message != "v1" {
		t.Fatalf("got %+v, %v; want the post from the store", got, err)
	}
}
