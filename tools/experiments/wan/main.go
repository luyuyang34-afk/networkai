// Command wan measures what a cross-region link does to the write path.
//
//	go run ./cmd/wanproxy -listen :15432 -target localhost:5432 -delay 35ms -admin :19100 &
//	go run ./tools/experiments/wan -n 50
//
// The store connects to PostgreSQL through wanproxy (35 ms each way = 70 ms
// RTT, roughly us-east <-> eu-west). Three versions of "create a post and
// return a consistency token" are timed:
//
//   - before: transaction (BEGIN, INSERT post, INSERT outbox, SELECT,
//     COMMIT), then the token query: the previous implementation;
//   - one statement: data-modifying CTE, then the token query;
//   - pipelined: the CTE and the token query in one pipeline.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"sort"
	"time"

	"networkai/internal/db"
	"networkai/internal/store"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	n := flag.Int("n", 50, "writes per variant")
	adminURL := flag.String("db", env("DATABASE_URL", "postgres://networkai:networkai@localhost:5432/networkai?sslmode=disable"), "admin URL (local)")
	wanAddr := flag.String("wan", "localhost:15432", "wanproxy address in front of the same server")
	rtt := flag.Duration("rtt", 70*time.Millisecond, "RTT the proxy adds (for the round-trip column)")
	flag.Parse()
	ctx := context.Background()

	name := freshDB(ctx, *adminURL)
	u, _ := url.Parse(*adminURL)
	u.Host, u.Path = *wanAddr, "/"+name
	pool, err := db.Connect(ctx, u.String())
	must(err)
	st := store.New(pool)
	alice, err := st.CreateUser(ctx, "alice", "x")
	must(err)

	fmt.Printf("# Writes over a %s RTT link (%d per variant)\n\n", *rtt, *n)
	fmt.Println("| Variant | Round trips | P50 | P95 |")
	fmt.Println("|---|---|---|---|")
	variants := []struct {
		name string
		rts  string
		fn   func() error
	}{
		{"Before: transaction + token query", "6", func() error {
			if err := oldCreate(ctx, pool, alice.ID); err != nil {
				return err
			}
			_, err := st.WriteToken(ctx)
			return err
		}},
		{"One statement (CTE) + token query", "2", func() error {
			if _, err := st.CreatePost(ctx, post(alice.ID)); err != nil {
				return err
			}
			_, err := st.WriteToken(ctx)
			return err
		}},
		{"Pipelined: CTE and token together", "1", func() error {
			wctx, sink := store.WithTokenSink(ctx)
			_, err := st.CreatePost(wctx, post(alice.ID))
			if err == nil && sink.Token == "" {
				err = fmt.Errorf("no token")
			}
			return err
		}},
	}
	for _, v := range variants {
		must(v.fn()) // warm the connection and statement cache
		var ds []time.Duration
		for i := 0; i < *n; i++ {
			t0 := time.Now()
			must(v.fn())
			ds = append(ds, time.Since(t0))
		}
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		fmt.Printf("| %s | %s | %s | %s |\n", v.name, v.rts, ms(ds[len(ds)/2]), ms(ds[len(ds)*95/100]))
	}

	// Idle connections: pgxpool pings a connection idle for over a second
	// before handing it out, which on a WAN is a full extra RTT.
	var idle []time.Duration
	for i := 0; i < 5; i++ {
		time.Sleep(1500 * time.Millisecond)
		wctx, _ := store.WithTokenSink(ctx)
		t0 := time.Now()
		_, err := st.CreatePost(wctx, post(alice.ID))
		must(err)
		idle = append(idle, time.Since(t0))
	}
	sort.Slice(idle, func(i, j int) bool { return idle[i] < idle[j] })
	fmt.Printf("\nPipelined write after 1.5 s idle (pool health-check ping first): P50 %s\n", ms(idle[len(idle)/2]))
}

func post(author int64) store.NewPost {
	return store.NewPost{ID: uuid.New(), AuthorID: author, Message: "hello from eu", MediaType: "none"}
}

// oldCreate is the pre-change CreatePost, kept here as the baseline.
func oldCreate(ctx context.Context, pool *pgxpool.Pool, author int64) error {
	id := uuid.New()
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO posts (id, author_id, message, media_type) VALUES ($1, $2, $3, 'none')`,
			id, author, "hello from eu"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO outbox (aggregate_type, aggregate_id, event_type) VALUES ('post', $1, 'post.changed')`,
			id.String()); err != nil {
			return err
		}
		var v int64
		return tx.QueryRow(ctx, `SELECT p.version FROM posts p JOIN users u ON u.id = p.author_id WHERE p.id = $1`, id).Scan(&v)
	})
}

func freshDB(ctx context.Context, adminURL string) string {
	admin, err := pgx.Connect(ctx, adminURL)
	must(err)
	defer admin.Close(ctx)
	name := "networkai_wan_exp"
	admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)") //nolint:errcheck
	_, err = admin.Exec(ctx, "CREATE DATABASE "+name)
	must(err)
	u, _ := url.Parse(adminURL)
	u.Path = "/" + name
	p, err := db.Connect(ctx, u.String())
	must(err)
	must(db.Migrate(ctx, p))
	p.Close()
	return name
}

func ms(d time.Duration) string { return fmt.Sprintf("%.0f ms", float64(d.Microseconds())/1000) }

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
