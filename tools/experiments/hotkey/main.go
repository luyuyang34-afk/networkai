// Command hotkey measures the "viral post" hot key: many concurrent likes on
// ONE post, under three like-count layouts.
//
//		go run ./tools/experiments/hotkey -users 50000 -workers 64
//
//	  - no counter:   likes rows only; readers COUNT(*) them
//	  - 1 row:        one counter row per post, updated in the like transaction
//	  - 16 shards:    the counter split over 16 rows, one picked at random
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
	"networkai/internal/store"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	users := flag.Int("users", 50000, "distinct users liking the post")
	workers := flag.Int("workers", 64, "concurrent likers")
	adminURL := flag.String("db", env("DATABASE_URL", "postgres://networkai:networkai@localhost:5432/networkai?sslmode=disable"), "admin URL")
	flag.Parse()
	ctx := context.Background()

	fmt.Printf("# Hot key: %d users like one post, %d concurrent workers\n\n", *users, *workers)
	fmt.Println("| Layout | Likes/s | Like P50 | Like P99 | Count read P50 (post with all likes) |")
	fmt.Println("|---|---|---|---|---|")
	for _, mode := range []struct {
		name   string
		shards int
	}{{"No counter (COUNT(*) on read)", 0}, {"1 counter row per post", 1}, {"16 counter shards", 16}} {
		pool := freshDB(ctx, *adminURL, *workers)
		st := store.New(pool).WithLikeShards(mode.shards)
		author, err := st.CreateUser(ctx, "author", "x")
		must(err)
		post, err := st.CreatePost(ctx, store.NewPost{ID: uuid.New(), AuthorID: author.ID, Message: "viral", MediaType: "none"})
		must(err)
		_, err = pool.Exec(ctx, `INSERT INTO users (username, password_hash) SELECT 'liker_' || g, 'x' FROM generate_series(1, $1::int) g`, *users)
		must(err)
		rows, err := pool.Query(ctx, `SELECT id FROM users WHERE username LIKE 'liker\_%'`)
		must(err)
		ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
		must(err)

		var next atomic.Int64
		lat := make([][]time.Duration, *workers)
		var wg sync.WaitGroup
		start := time.Now()
		for w := 0; w < *workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					i := next.Add(1) - 1
					if i >= int64(len(ids)) {
						return
					}
					t0 := time.Now()
					must(st.Like(ctx, ids[i], post.ID))
					lat[w] = append(lat[w], time.Since(t0))
				}
			}()
		}
		wg.Wait()
		elapsed := time.Since(start)
		var all []time.Duration
		for _, l := range lat {
			all = append(all, l...)
		}
		sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })

		var reads []time.Duration
		for i := 0; i < 200; i++ {
			t0 := time.Now()
			c, err := st.LikeCounts(ctx, []string{post.ID.String()})
			must(err)
			reads = append(reads, time.Since(t0))
			if c[post.ID.String()] != len(ids) {
				log.Fatalf("%s: count %d, want %d", mode.name, c[post.ID.String()], len(ids))
			}
		}
		sort.Slice(reads, func(i, j int) bool { return reads[i] < reads[j] })
		fmt.Printf("| %s | %.0f | %s | %s | %s |\n", mode.name, float64(len(ids))/elapsed.Seconds(),
			ms(all[len(all)/2]), ms(all[len(all)*99/100]), ms(reads[len(reads)/2]))
		pool.Close()
	}
}

func freshDB(ctx context.Context, adminURL string, conns int) *pgxpool.Pool {
	admin, err := pgx.Connect(ctx, adminURL)
	must(err)
	defer admin.Close(ctx)
	name := "networkai_hotkey_exp"
	admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)") //nolint:errcheck
	_, err = admin.Exec(ctx, "CREATE DATABASE "+name)
	must(err)
	u, err := url.Parse(adminURL)
	must(err)
	u.Path = "/" + name
	q := u.Query()
	q.Set("pool_max_conns", fmt.Sprint(conns)) // one connection per worker
	u.RawQuery = q.Encode()
	pool, err := db.Connect(ctx, u.String())
	must(err)
	must(db.Migrate(ctx, pool))
	return pool
}

func ms(d time.Duration) string { return fmt.Sprintf("%.1f ms", float64(d.Microseconds())/1000) }

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
