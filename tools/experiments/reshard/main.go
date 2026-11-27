// Command reshard grows a sharded posts cluster from 2 to 4 PostgreSQL
// nodes while it serves reads and writes, and checks that nothing was lost.
//
//	docker compose --profile shards up -d
//	go run ./tools/experiments/reshard                 # fenced moves
//	go run ./tools/experiments/reshard -naive          # copy + flip, no fence
//
// Timeline: load starts; after -warmup the 32 logical shards whose target
// is a new node move one by one; load continues for -cooldown after.
// Reported per phase: write and read latency, errors and ErrMoved retries,
// scatter-gather latency, per-move fence time, and a final check that every
// acknowledged write is on its shard's owner with at least the acked
// version.
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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"networkai/internal/db"
	"networkai/internal/shard"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type sample struct {
	at  time.Duration
	d   time.Duration
	err bool
}

type recorder struct {
	mu sync.Mutex
	s  map[string][]sample
}

func (r *recorder) add(kind string, at, d time.Duration, err bool) {
	r.mu.Lock()
	r.s[kind] = append(r.s[kind], sample{at, d, err})
	r.mu.Unlock()
}

func main() {
	nodesFlag := flag.String("nodes", "n1=localhost:5441,n2=localhost:5442,n3=localhost:5443,n4=localhost:5444", "name=host:port, first two start with data")
	dirURL := flag.String("db", env("DATABASE_URL", "postgres://networkai:networkai@localhost:5432/networkai?sslmode=disable"), "admin URL for the directory database")
	seed := flag.Int("seed", 200000, "posts loaded before the experiment")
	authors := flag.Int("authors", 20000, "distinct authors")
	writers := flag.Int("writers", 16, "concurrent writers (each its own router = API instance)")
	readers := flag.Int("readers", 16, "concurrent readers")
	warmup := flag.Duration("warmup", 10*time.Second, "load before resharding starts")
	cooldown := flag.Duration("cooldown", 10*time.Second, "load after resharding ends")
	naive := flag.Bool("naive", false, "move without the fence (copy, then flip the map)")
	flag.Parse()
	ctx := context.Background()

	nodes := map[string]*pgxpool.Pool{}
	var names []string
	for _, kv := range strings.Split(*nodesFlag, ",") {
		name, hp, _ := strings.Cut(kv, "=")
		p, err := db.Connect(ctx, fmt.Sprintf("postgres://networkai:networkai@%s/shard?sslmode=disable&pool_max_conns=64", hp))
		must(err)
		_, err = p.Exec(ctx, `DROP TABLE IF EXISTS shard_posts, shard_owner`)
		must(err)
		nodes[name] = p
		names = append(names, name)
	}
	dir := freshDB(ctx, *dirURL)
	old := names[:2]
	must(shard.Bootstrap(ctx, dir, map[string]*pgxpool.Pool{old[0]: nodes[old[0]], old[1]: nodes[old[1]]},
		func(l int) string { return old[l%2] }))
	for _, n := range names[2:] {
		must(shard.AddNode(ctx, nodes[n]))
	}
	log.Printf("seeding %d posts", *seed)
	seedPosts(ctx, nodes, old, *seed, *authors)

	rec := &recorder{s: map[string][]sample{}}
	var retries atomic.Int64
	newRouter := func() *shard.Router {
		return &shard.Router{Dir: dir, Nodes: nodes, OnRetry: func() { retries.Add(1) }}
	}
	var ackMu sync.Mutex
	acked := map[uuid.UUID]shard.Post{}
	recent := make([]uuid.UUID, 0, 1<<16)

	t0 := time.Now()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < *writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := newRouter()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				var p shard.Post
				var err error
				kind := "create"
				start := time.Now()
				ackMu.Lock()
				var edit shard.Post
				if i%2 == 1 && len(recent) > 0 {
					edit = acked[recent[rand.IntN(len(recent))]]
				}
				ackMu.Unlock()
				if edit.ID != uuid.Nil {
					kind = "edit"
					p, err = r.UpdatePost(ctx, edit.ID, edit.AuthorID, fmt.Sprint("edit ", i))
				} else {
					p, err = r.CreatePost(ctx, int64(rand.IntN(*authors)+1), fmt.Sprint("new ", i))
				}
				rec.add(kind, time.Since(t0), time.Since(start), err != nil)
				if err != nil {
					log.Printf("%s error: %v", kind, err)
					continue
				}
				ackMu.Lock()
				if prev, ok := acked[p.ID]; !ok || p.Version > prev.Version {
					if !ok {
						recent = append(recent, p.ID)
					}
					acked[p.ID] = p
				}
				ackMu.Unlock()
			}
		}()
	}
	for rd := 0; rd < *readers; rd++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := newRouter()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				start := time.Now()
				var err error
				kind := ""
				switch i % 4 {
				case 0, 1:
					kind = "get"
					ackMu.Lock()
					var id uuid.UUID
					if len(recent) > 0 {
						id = recent[rand.IntN(len(recent))]
					}
					ackMu.Unlock()
					if id == uuid.Nil {
						continue
					}
					_, err = r.GetPost(ctx, id)
				case 2:
					kind = "by_author"
					_, err = r.ListByAuthor(ctx, int64(rand.IntN(*authors)+1), uuid.Nil, 20)
				case 3:
					kind = "recent"
					_, err = r.ListRecent(ctx, uuid.Nil, 20)
				}
				rec.add(kind, time.Since(t0), time.Since(start), err != nil)
				if err != nil {
					log.Printf("%s error: %v", kind, err)
				}
			}
		}()
	}

	time.Sleep(*warmup)
	reshardStart := time.Since(t0)
	log.Printf("resharding %d -> %d nodes", len(old), len(names))
	var moves []shard.MoveStats
	mover := &shard.Mover{R: newRouter(), Naive: *naive}
	err := mover.Rebalance(ctx, func(l int) string { return names[l%len(names)] }, func(st shard.MoveStats) {
		moves = append(moves, st)
	})
	if err != nil {
		log.Printf("rebalance error: %v", err)
	}
	reshardEnd := time.Since(t0)
	time.Sleep(*cooldown)
	close(stop)
	wg.Wait()

	mode := "fenced moves"
	if *naive {
		mode = "NAIVE moves (copy + flip, no fence)"
	}
	fmt.Printf("# Online resharding %d -> %d nodes, %s\n\n", len(old), len(names), mode)
	fmt.Printf("%d seeded posts, %d writers, %d readers. Resharding ran from %.1fs to %.1fs (%d moves).\n\n",
		*seed, *writers, *readers, reshardStart.Seconds(), reshardEnd.Seconds(), len(moves))
	fmt.Println("| Operation | Phase | Ops/s | P50 | P99 | Max | Errors |")
	fmt.Println("|---|---|---|---|---|---|---|")
	for _, kind := range []string{"create", "edit", "get", "by_author", "recent"} {
		for _, ph := range []struct {
			name     string
			from, to time.Duration
		}{{"before (2 nodes)", 0, reshardStart}, {"during", reshardStart, reshardEnd}, {"after (4 nodes)", reshardEnd, 1 << 62}} {
			var ds []time.Duration
			errs := 0
			for _, s := range rec.s[kind] {
				if s.at >= ph.from && s.at < ph.to {
					ds = append(ds, s.d)
					if s.err {
						errs++
					}
				}
			}
			if len(ds) == 0 {
				continue
			}
			sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
			span := min(ph.to, reshardEnd+*cooldown) - ph.from
			fmt.Printf("| %s | %s | %.0f | %s | %s | %s | %d |\n", kind, ph.name, float64(len(ds))/span.Seconds(),
				ms(ds[len(ds)/2]), ms(ds[len(ds)*99/100]), ms(ds[len(ds)-1]), errs)
		}
	}
	fmt.Printf("\nErrMoved retries (stale map or cutover in progress): %d\n\n", retries.Load())

	if len(moves) > 0 {
		var fences, totals []time.Duration
		rows, fenceRows := 0, 0
		for _, m := range moves {
			fences = append(fences, m.Fence)
			totals = append(totals, m.Total)
			rows += m.Rows
			fenceRows += m.FenceRows
		}
		sort.Slice(fences, func(i, j int) bool { return fences[i] < fences[j] })
		sort.Slice(totals, func(i, j int) bool { return totals[i] < totals[j] })
		fmt.Printf("Per move: rows copied %d avg (%d during fences in total), move time P50 %s max %s, "+
			"write-blocking window P50 %s P99 %s max %s\n\n", rows/len(moves), fenceRows,
			ms(totals[len(totals)/2]), ms(totals[len(totals)-1]),
			ms(fences[len(fences)/2]), ms(fences[len(fences)*99/100]), ms(fences[len(fences)-1]))
	}

	// Verify.
	check := newRouter()
	must(check.Refresh(ctx))
	lost, stale := 0, 0
	for id, want := range acked {
		p, err := check.GetPost(ctx, id)
		switch {
		case err != nil:
			lost++
		case p.Version < want.Version:
			stale++
		}
	}
	total := 0
	perNode := map[string]int{}
	for _, n := range names {
		var c int
		must(nodes[n].QueryRow(ctx, `SELECT count(*) FROM shard_posts`).Scan(&c))
		perNode[n] = c
		total += c
	}
	fmt.Printf("Acknowledged writes during the run: %d posts. **Lost: %d. Rolled back to an older version: %d.**\n",
		len(acked), lost, stale)
	fmt.Printf("Rows per node after cleanup: %v (total %d = %d seeded + %d new: %v)\n",
		perNode, total, *seed, countCreates(acked), total == *seed+countCreates(acked))
}

func countCreates(acked map[uuid.UUID]shard.Post) int { return len(acked) }

// seedPosts loads rows straight into their owners with COPY.
func seedPosts(ctx context.Context, nodes map[string]*pgxpool.Pool, owners []string, n, authors int) {
	rows := map[string][][]any{}
	base := time.Now().Add(-30 * 24 * time.Hour)
	for i := 0; i < n; i++ {
		a := int64(rand.IntN(authors) + 1)
		l := shard.LShardOfAuthor(a)
		node := owners[l%len(owners)]
		rows[node] = append(rows[node], []any{shard.NewPostID(l), int16(l), a, fmt.Sprint("seed ", i),
			base.Add(time.Duration(i) * time.Second)})
	}
	for node, rs := range rows {
		_, err := nodes[node].CopyFrom(ctx, pgx.Identifier{"shard_posts"},
			[]string{"id", "lshard", "author_id", "message", "created_at"}, pgx.CopyFromRows(rs))
		must(err)
		_, err = nodes[node].Exec(ctx, `ANALYZE shard_posts`)
		must(err)
	}
}

func freshDB(ctx context.Context, adminURL string) *pgxpool.Pool {
	admin, err := pgx.Connect(ctx, adminURL)
	must(err)
	defer admin.Close(ctx)
	name := "networkai_shard_directory"
	admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)") //nolint:errcheck
	_, err = admin.Exec(ctx, "CREATE DATABASE "+name)
	must(err)
	u, err := url.Parse(adminURL)
	must(err)
	u.Path = "/" + name
	p, err := db.Connect(ctx, u.String())
	must(err)
	return p
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
