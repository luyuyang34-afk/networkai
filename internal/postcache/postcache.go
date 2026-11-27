// Package postcache is a Redis read-through cache for single posts
// (GET /posts/{id}), built to survive a hot key: one viral post read by
// thousands of clients at once.
//
// Plain cache-aside has three known failure modes, and each has a known fix.
// Every fix can be switched off, so the experiment can show what it buys:
//
//  1. Stampede inside one process: N concurrent misses on the same key issue
//     N identical database queries. Fix: singleflight (one load per key per
//     process; the other callers wait for it).
//  2. Stampede across processes, and stale sets: with many API instances,
//     singleflight still lets every instance load. Worse, a reader that
//     loaded the old row can write it into the cache AFTER a writer
//     invalidated it, leaving the cache wrong until the TTL. Fix: leases, as
//     in Facebook's memcache paper (NSDI 2013). A miss takes a short lease
//     (SET NX); only the holder loads and fills, and its fill succeeds only
//     if the lease still exists. Invalidation deletes the lease, so a fill
//     racing with a write is dropped. Callers without the lease wait briefly
//     for the holder's fill.
//  3. Synchronized expiry: when a hot key expires, every reader misses at
//     the same instant. Fix: probabilistic early recomputation ("XFetch",
//     Vattani et al., VLDB 2015). Each hit refreshes early with a
//     probability that rises as expiry approaches, scaled by how long the
//     load takes, so one reader refreshes in the background before the key
//     expires and nobody sees a miss.
//
// The database stays the source of truth. If Redis is down, reads go
// straight to the store.
package postcache

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	mrand "math/rand/v2"
	"time"

	"networkai/internal/store"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
)

// Store is what the cache loads from (and passes writes through to).
type Store interface {
	GetPost(ctx context.Context, id uuid.UUID) (store.Post, error)
}

// Options selects which protections are on. The zero value is plain
// cache-aside.
type Options struct {
	Singleflight bool
	Leases       bool
	// Beta > 0 enables XFetch early refresh; 1 is the paper's default.
	Beta float64
}

// Protected is the production setting: all three protections.
var Protected = Options{Singleflight: true, Leases: true, Beta: 1}

type Cache struct {
	Redis *redis.Client
	Store Store
	Opt   Options
	// TTL of a cached post (default 60s). Invalidation on edit and delete
	// makes it a backstop, not the freshness bound.
	TTL time.Duration
	// NegativeTTL caches "not found" so a deleted hot post does not
	// stampede the database (default 5s).
	NegativeTTL time.Duration
	// LeaseTTL bounds how long a crashed lease holder blocks fills
	// (default 2s). LeaseWait is how long a caller without the lease waits
	// for the holder before loading itself, without filling (default 300ms).
	LeaseTTL, LeaseWait time.Duration
	Prefix              string
	// OnResult counts outcomes: hit, miss, early_refresh, lease_wait_hit,
	// lease_wait_timeout, fill_dropped, redis_error.
	OnResult func(result string)

	sf singleflight.Group
}

// entry is the cached value. Delta and Expiry drive XFetch.
type entry struct {
	Post   *store.Post `json:"p"`
	Delta  int64       `json:"d"` // load time, microseconds
	Expiry int64       `json:"e"` // unix microseconds
}

func (c *Cache) key(id uuid.UUID) string   { return c.Prefix + "post:" + id.String() }
func (c *Cache) lease(id uuid.UUID) string { return c.Prefix + "post-lease:" + id.String() }

func (c *Cache) count(r string) {
	if c.OnResult != nil {
		c.OnResult(r)
	}
}

func (c *Cache) ttl(e entry) time.Duration {
	if e.Post == nil {
		return or(c.NegativeTTL, 5*time.Second)
	}
	return or(c.TTL, 60*time.Second)
}

// GetPost returns the post from the cache, loading it on a miss.
func (c *Cache) GetPost(ctx context.Context, id uuid.UUID) (store.Post, error) {
	raw, err := c.Redis.Get(ctx, c.key(id)).Bytes()
	switch {
	case err == nil:
		var e entry
		if json.Unmarshal(raw, &e) == nil {
			c.count("hit")
			if c.Opt.Beta > 0 && c.shouldRefreshEarly(e) {
				c.count("early_refresh")
				go c.refresh(context.WithoutCancel(ctx), id) //nolint:errcheck
			}
			return e.result()
		}
	case !errors.Is(err, redis.Nil):
		c.count("redis_error")
		return c.Store.GetPost(ctx, id)
	}
	c.count("miss")
	if !c.Opt.Singleflight {
		e, err := c.load(ctx, id)
		if err != nil {
			return store.Post{}, err
		}
		return e.result()
	}
	v, err, _ := c.sf.Do(id.String(), func() (any, error) {
		return c.load(context.WithoutCancel(ctx), id)
	})
	if err != nil {
		return store.Post{}, err
	}
	return v.(entry).result()
}

// shouldRefreshEarly is XFetch: refresh if now - delta*beta*ln(U) >= expiry.
// -ln(U) is exponentially distributed, so most hits far from expiry never
// refresh, and the chance rises sharply in the last few load-times.
func (c *Cache) shouldRefreshEarly(e entry) bool {
	now := time.Now().UnixMicro()
	gap := float64(e.Delta) * c.Opt.Beta * -math.Log(1-mrand.Float64())
	return float64(now)+gap >= float64(e.Expiry)
}

// refresh reloads in the background; concurrent refreshes in this process
// share one load.
func (c *Cache) refresh(ctx context.Context, id uuid.UUID) error {
	_, err, _ := c.sf.Do("refresh:"+id.String(), func() (any, error) {
		return c.load(ctx, id)
	})
	return err
}

// load reads the database and fills the cache, under a lease if enabled.
func (c *Cache) load(ctx context.Context, id uuid.UUID) (entry, error) {
	var token string
	if c.Opt.Leases {
		token = newToken()
		ok, err := c.Redis.SetNX(ctx, c.lease(id), token, or(c.LeaseTTL, 2*time.Second)).Result()
		if err == nil && !ok {
			// Someone else is loading: wait for their fill.
			if e, ok := c.waitForFill(ctx, id); ok {
				c.count("lease_wait_hit")
				return e, nil
			}
			c.count("lease_wait_timeout")
			return c.fetch(ctx, id) // serve, but do not fill without the lease
		}
	}
	e, err := c.fetch(ctx, id)
	if err != nil {
		return entry{}, err
	}
	b, _ := json.Marshal(e)
	if c.Opt.Leases {
		n, err := fillIfLeased.Run(ctx, c.Redis, []string{c.key(id), c.lease(id)},
			b, token, c.ttl(e).Milliseconds()).Int()
		if err == nil && n == 0 {
			c.count("fill_dropped") // invalidated while we were loading
		}
	} else {
		c.Redis.Set(ctx, c.key(id), b, c.ttl(e))
	}
	return e, nil
}

// fetch reads the primary: a cache filled from a lagging replica would stay
// wrong after the replica catches up (ADR 0007).
func (c *Cache) fetch(ctx context.Context, id uuid.UUID) (entry, error) {
	start := time.Now()
	p, err := c.Store.GetPost(store.ForcePrimary(ctx), id)
	var e entry
	switch {
	case err == nil:
		e.Post = &p
	case errors.Is(err, store.ErrNotFound):
	default:
		return entry{}, err
	}
	e.Delta = time.Since(start).Microseconds()
	e.Expiry = time.Now().Add(c.ttl(e)).UnixMicro()
	return e, nil
}

func (c *Cache) waitForFill(ctx context.Context, id uuid.UUID) (entry, bool) {
	deadline := time.Now().Add(or(c.LeaseWait, 300*time.Millisecond))
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return entry{}, false
		case <-time.After(10 * time.Millisecond):
		}
		raw, err := c.Redis.Get(ctx, c.key(id)).Bytes()
		if err == nil {
			var e entry
			if json.Unmarshal(raw, &e) == nil {
				return e, true
			}
		}
	}
	return entry{}, false
}

// Invalidate drops the cached post and any lease, so an in-flight fill of
// the old version is rejected. Call it after a write commits.
func (c *Cache) Invalidate(ctx context.Context, id uuid.UUID) error {
	return c.Redis.Del(ctx, c.key(id), c.lease(id)).Err()
}

var fillIfLeased = redis.NewScript(`
if redis.call('GET', KEYS[2]) == ARGV[2] then
  redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[3])
  redis.call('DEL', KEYS[2])
  return 1
end
return 0`)

func (e entry) result() (store.Post, error) {
	if e.Post == nil {
		return store.Post{}, store.ErrNotFound
	}
	return *e.Post, nil
}

func newToken() string {
	b := make([]byte, 8)
	rand.Read(b) //nolint:errcheck
	return hex.EncodeToString(b)
}

func or(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}
