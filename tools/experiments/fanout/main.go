// Command fanout measures the celebrity problem for home timelines.
//
//	go run ./tools/experiments/fanout -followers 50000
//
// It creates (in a throwaway database) one "celebrity" with N followers, all
// of whom have an active cached timeline, plus regular users, and compares:
//
//  1. pure fan-out on write (every post pushed to every follower) against
//     the hybrid (celebrity posts pulled at read time), on write cost and
//     read latency;
//  2. head-of-line blocking: with one worker processing events in order, how
//     long a regular user's post waits behind a celebrity's fan-out.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"net/url"
	"os"
	"sort"
	"time"

	"networkai/internal/db"
	"networkai/internal/store"
	"networkai/internal/timeline"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

func main() {
	followers := flag.Int("followers", 50000, "celebrity follower count")
	samples := flag.Int("samples", 300, "timeline reads to time per mode")
	pgURL := flag.String("db", env("DATABASE_URL", "postgres://networkai:networkai@localhost:5432/networkai?sslmode=disable"), "admin database URL")
	redisURL := flag.String("redis", env("REDIS_URL", "redis://localhost:6379/0"), "redis URL")
	flag.Parse()
	ctx := context.Background()

	pool := freshDB(ctx, *pgURL)
	defer pool.Close()
	opt, err := redis.ParseURL(*redisURL)
	must(err)
	rdb := redis.NewClient(opt)
	st := store.New(pool)

	log.Printf("seeding: 1 celebrity, %d followers, 2000 regular authors ...", *followers)
	star, regular := seed(ctx, pool, *followers)
	fans, err := st.FollowerIDsAfter(ctx, star, 0, *followers)
	must(err)

	fmt.Printf("# Celebrity fan-out experiment\n\nCelebrity followers: %d (all with an active cached timeline). "+
		"Each follower also follows 20 of 2,000 regular authors (~%d followers each).\n\n", len(fans), len(fans)*20/2000)
	fmt.Println("| Mode | Redis writes for 1 celebrity post | Time to fan out | Timeline read P50 | P95 | Regular post waits behind it |")
	fmt.Println("|---|---|---|---|---|---|")

	for _, mode := range []struct {
		name      string
		threshold int64
	}{{"Pure fan-out on write", 0}, {"Hybrid (celebrities pulled)", 10000}} {
		prefix := fmt.Sprintf("exp%d:", rand.Uint64())
		tl := &timeline.Service{Redis: rdb, Store: st, CelebrityThreshold: mode.threshold, Prefix: prefix}
		// Make every follower "active" (has a cached timeline), in bulk. A
		// real system would have built these as users read; doing it one
		// Read at a time here would take ~40 ms x N just to set up.
		markActive(ctx, rdb, prefix, fans)
		for _, f := range fans[:5] { // and fully build a few through the real path
			_, err := tl.Read(ctx, f, time.Time{}, 1)
			must(err)
		}

		writes, fanout := 0, time.Duration(0)
		tl.OnFanout = func(n int, d time.Duration) { writes, fanout = n, d }
		writes, fanout = 0, 0
		starPost := post(ctx, st, star, "celebrity post")
		regPost := post(ctx, st, regular[0], "regular post right after")

		// One worker, events in order: the regular post is handled only
		// after the celebrity post's fan-out finishes.
		t0 := time.Now()
		must(tl.HandlePostChanged(ctx, starPost))
		starWrites, starFanout := writes, fanout // record before the next post overwrites them
		must(tl.HandlePostChanged(ctx, regPost))
		regularWait := time.Since(t0)

		lat := make([]time.Duration, 0, *samples)
		for i := 0; i < *samples; i++ {
			f := fans[rand.IntN(len(fans))]
			s := time.Now()
			_, err := tl.Read(ctx, f, time.Time{}, 20)
			must(err)
			lat = append(lat, time.Since(s))
		}
		sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
		fmt.Printf("| %s | %d | %s | %s | %s | %s |\n", mode.name, starWrites, round(starFanout),
			round(lat[len(lat)/2]), round(lat[len(lat)*95/100]), round(regularWait))

		// Clean up this mode's keys.
		iter := rdb.Scan(ctx, 0, prefix+"*", 1000).Iterator()
		for iter.Next(ctx) {
			rdb.Del(ctx, iter.Val())
		}
	}
	fmt.Println("\nTimeline reads include the PostgreSQL pull for celebrity posts in hybrid mode.")
}

func markActive(ctx context.Context, rdb *redis.Client, prefix string, ids []int64) {
	for i := 0; i < len(ids); i += 1000 {
		pipe := rdb.Pipeline()
		for _, id := range ids[i:min(i+1000, len(ids))] {
			k := fmt.Sprintf("%stl:%d", prefix, id)
			pipe.ZAdd(ctx, k, redis.Z{Score: 0, Member: "__built__"})
			pipe.Expire(ctx, k, time.Hour)
		}
		_, err := pipe.Exec(ctx)
		must(err)
	}
}

// freshDB creates and migrates a throwaway database next to the admin one.
func freshDB(ctx context.Context, adminURL string) *pgxpool.Pool {
	admin, err := pgx.Connect(ctx, adminURL)
	must(err)
	defer admin.Close(ctx)
	name := "networkai_fanout_exp"
	admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)") //nolint:errcheck
	_, err = admin.Exec(ctx, "CREATE DATABASE "+name)
	must(err)
	u, err := url.Parse(adminURL)
	must(err)
	u.Path = "/" + name
	pool, err := db.Connect(ctx, u.String())
	must(err)
	must(db.Migrate(ctx, pool))
	return pool
}

// seed bulk-inserts users and follows in SQL (fast) and returns the
// celebrity's id and 20 regular author ids.
func seed(ctx context.Context, pool *pgxpool.Pool, n int) (int64, []int64) {
	_, err := pool.Exec(ctx, `
		INSERT INTO users (username, password_hash) VALUES ('celebrity', 'x');
		INSERT INTO users (username, password_hash)
		  SELECT 'author_' || g, 'x' FROM generate_series(1, 2000) g;
		INSERT INTO users (username, password_hash)
		  SELECT 'fan_' || g, 'x' FROM generate_series(1, $1::int) g;`, n)
	if err != nil { // multi-statement with args is not allowed; fall back
		_, err = pool.Exec(ctx, `INSERT INTO users (username, password_hash) VALUES ('celebrity', 'x')`)
		must(err)
		_, err = pool.Exec(ctx, `INSERT INTO users (username, password_hash) SELECT 'author_' || g, 'x' FROM generate_series(1, 2000) g`)
		must(err)
		_, err = pool.Exec(ctx, `INSERT INTO users (username, password_hash) SELECT 'fan_' || g, 'x' FROM generate_series(1, $1::int) g`, n)
		must(err)
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO follows (follower_id, followee_id)
		  SELECT f.id, c.id FROM users f, users c WHERE f.username LIKE 'fan\_%' AND c.username = 'celebrity'`)
	must(err)
	// Each fan follows 20 random regular authors (lateral subquery is
	// re-evaluated per fan because it references f).
	// Authors were inserted in one statement, so their ids are contiguous:
	// pick 20 random ids from that range per fan (duplicates are skipped).
	_, err = pool.Exec(ctx, `
		WITH a AS (SELECT min(id) AS lo, count(*) AS n FROM users WHERE username LIKE 'author\_%')
		INSERT INTO follows (follower_id, followee_id)
		  SELECT f.id, a.lo + floor(random() * a.n)::bigint
		  FROM users f, a, generate_series(1, 20)
		  WHERE f.username LIKE 'fan\_%'
		  ON CONFLICT DO NOTHING`)
	must(err)
	// One aggregate pass, not a correlated subquery per user: with fresh,
	// un-analyzed tables the planner may run a full scan of follows for each
	// of the N users (that version ran for 10+ minutes).
	_, err = pool.Exec(ctx, `ANALYZE follows`)
	must(err)
	_, err = pool.Exec(ctx, `
		UPDATE users u SET follower_count = c.n
		FROM (SELECT followee_id, count(*) AS n FROM follows GROUP BY followee_id) c
		WHERE c.followee_id = u.id`)
	must(err)
	var star int64
	must(pool.QueryRow(ctx, `SELECT id FROM users WHERE username = 'celebrity'`).Scan(&star))
	rows, err := pool.Query(ctx, `SELECT id FROM users WHERE username LIKE 'author\_%' ORDER BY id`)
	must(err)
	reg, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	must(err)
	// Some history so reads have something to merge.
	for _, a := range reg[:50] {
		for i := 0; i < 2; i++ {
			post(ctx, store.New(pool), a, "history")
		}
	}
	return star, reg
}

func post(ctx context.Context, st *store.Store, author int64, msg string) uuid.UUID {
	p, err := st.CreatePost(ctx, store.NewPost{ID: uuid.New(), AuthorID: author, Message: msg, MediaType: "none"})
	must(err)
	return p.ID
}

func round(d time.Duration) string {
	switch {
	case d > time.Second:
		return fmt.Sprintf("%.2f s", d.Seconds())
	default:
		return fmt.Sprintf("%.1f ms", float64(d.Microseconds())/1000)
	}
}

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
