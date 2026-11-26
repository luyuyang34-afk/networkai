// Command hotpost measures cache stampedes on one viral post.
//
//	go run ./tools/experiments/hotpost -instances 8 -readers 50 -duration 10s
//
// Several "API instances" (each its own cache object, so its own
// singleflight group, sharing one Redis) read the same post as fast as they
// can, with a short TTL so the key expires several times. A writer edits the
// post every -edit-every and invalidates the cache, like the API does.
//
// Reported per mode: database loads, read latency, and stale reads: a read
// that returned a version older than one whose write AND invalidation had
// already finished before the read started.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"networkai/internal/db"
	"networkai/internal/postcache"
	"networkai/internal/store"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// countingStore counts database loads and can add a fixed cost per load, to
// stand in for a heavier page build (joins, hydration).
type countingStore struct {
	st    *store.Store
	pool  *pgxpool.Pool
	extra time.Duration
	loads atomic.Int64
}

func (c *countingStore) GetPost(ctx context.Context, id uuid.UUID) (store.Post, error) {
	c.loads.Add(1)
	if c.extra > 0 {
		if _, err := c.pool.Exec(ctx, "SELECT pg_sleep($1)", c.extra.Seconds()); err != nil {
			return store.Post{}, err
		}
	}
	return c.st.GetPost(ctx, id)
}

func main() {
	instances := flag.Int("instances", 8, "API instances (separate caches, shared Redis)")
	readers := flag.Int("readers", 50, "concurrent readers per instance")
	duration := flag.Duration("duration", 10*time.Second, "run time per mode")
	ttl := flag.Duration("ttl", time.Second, "cache TTL (short, to force expiries)")
	editEvery := flag.Duration("edit-every", 250*time.Millisecond, "edit + invalidate interval (0 = no writes)")
	extra := flag.Duration("load-cost", 0, "extra database time per load (simulated heavier page)")
	conns := flag.Int("conns", 32, "database pool size (shared by all instances)")
	adminURL := flag.String("db", env("DATABASE_URL", "postgres://networkai:networkai@localhost:5432/networkai?sslmode=disable"), "admin URL")
	redisURL := flag.String("redis", env("REDIS_URL", "redis://localhost:6379/0"), "Redis URL")
	flag.Parse()
	ctx := context.Background()

	pool := freshDB(ctx, *adminURL, *conns)
	st := store.New(pool)
	author, err := st.CreateUser(ctx, "author", "x")
	must(err)
	post, err := st.CreatePost(ctx, store.NewPost{ID: uuid.New(), AuthorID: author.ID, Message: "viral v1", MediaType: "none"})
	must(err)
	ro, err := redis.ParseURL(*redisURL)
	must(err)
	rdb := redis.NewClient(ro)

	fmt.Printf("# Hot post: %d instances x %d readers, %s per mode, TTL %s, edit every %s, extra load cost %s, %d DB conns\n\n",
		*instances, *readers, *duration, *ttl, *editEvery, *extra, *conns)
	fmt.Println("| Mode | Reads/s | DB loads | DB loads/s | Read P50 | Read P99 | Reads that waited on a miss | Stale reads |")
	fmt.Println("|---|---|---|---|---|---|---|---|")
	modes := []struct {
		name string
		opt  *postcache.Options
	}{
		{"No cache (every read hits PostgreSQL)", nil},
		{"Cache-aside, no protection", &postcache.Options{}},
		{"+ singleflight", &postcache.Options{Singleflight: true}},
		{"+ singleflight + leases", &postcache.Options{Singleflight: true, Leases: true}},
		{"+ singleflight + leases + XFetch", &postcache.Protected},
	}
	for mi, mode := range modes {
		cs := &countingStore{st: st, pool: pool, extra: *extra}
		prefix := fmt.Sprintf("hotpost:%d:%d:", time.Now().UnixNano(), mi)
		caches := make([]*postcache.Cache, *instances)
		var misses atomic.Int64
		for i := range caches {
			if mode.opt != nil {
				caches[i] = &postcache.Cache{Redis: rdb, Store: cs, Opt: *mode.opt, TTL: *ttl, Prefix: prefix,
					OnResult: func(r string) {
						if r == "miss" {
							misses.Add(1)
						}
					}}
			}
		}
		get := func(i int, ctx context.Context) (store.Post, error) {
			if caches[i] == nil {
				return cs.GetPost(ctx, post.ID)
			}
			return caches[i].GetPost(ctx, post.ID)
		}

		// fresh = the newest version whose write and invalidation are done.
		var fresh atomic.Int64
		fresh.Store(post.Version)
		runCtx, cancel := context.WithTimeout(ctx, *duration)
		var wg sync.WaitGroup
		if *editEvery > 0 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				tick := time.NewTicker(*editEvery)
				defer tick.Stop()
				for n := 2; ; n++ {
					select {
					case <-runCtx.Done():
						return
					case <-tick.C:
					}
					p, err := st.UpdatePostMessage(ctx, post.ID, author.ID, fmt.Sprintf("viral v%d", n))
					must(err)
					if caches[0] != nil {
						must(caches[0].Invalidate(ctx, post.ID))
					}
					fresh.Store(p.Version)
				}
			}()
		}
		lat := make([][]time.Duration, *instances**readers)
		var stale atomic.Int64
		for i := 0; i < *instances; i++ {
			for r := 0; r < *readers; r++ {
				wg.Add(1)
				slot := i**readers + r
				go func() {
					defer wg.Done()
					for runCtx.Err() == nil {
						want := fresh.Load()
						t0 := time.Now()
						p, err := get(i, ctx)
						must(err)
						lat[slot] = append(lat[slot], time.Since(t0))
						if p.Version < want {
							stale.Add(1)
						}
					}
				}()
			}
		}
		wg.Wait()
		cancel()
		var all []time.Duration
		for _, l := range lat {
			all = append(all, l...)
		}
		sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
		secs := duration.Seconds()
		missed := "-"
		if mode.opt != nil {
			missed = fmt.Sprint(misses.Load())
		}
		fmt.Printf("| %s | %.0f | %d | %.0f | %s | %s | %s | %d (%.3f%%) |\n", mode.name, float64(len(all))/secs,
			cs.loads.Load(), float64(cs.loads.Load())/secs, ms(all[len(all)/2]), ms(all[len(all)*99/100]), missed,
			stale.Load(), 100*float64(stale.Load())/float64(len(all)))
	}
	pool.Close()
}

func freshDB(ctx context.Context, adminURL string, conns int) *pgxpool.Pool {
	admin, err := pgx.Connect(ctx, adminURL)
	must(err)
	defer admin.Close(ctx)
	name := "networkai_hotpost_exp"
	admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)") //nolint:errcheck
	_, err = admin.Exec(ctx, "CREATE DATABASE "+name)
	must(err)
	u, err := url.Parse(adminURL)
	must(err)
	u.Path = "/" + name
	q := u.Query()
	q.Set("pool_max_conns", fmt.Sprint(conns))
	u.RawQuery = q.Encode()
	pool, err := db.Connect(ctx, u.String())
	must(err)
	must(db.Migrate(ctx, pool))
	return pool
}

func ms(d time.Duration) string { return fmt.Sprintf("%.2f ms", float64(d.Microseconds())/1000) }

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
