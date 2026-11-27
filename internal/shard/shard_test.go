package shard_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"networkai/internal/shard"
	"networkai/internal/testutil"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type cluster struct {
	dir   *pgxpool.Pool
	nodes map[string]*pgxpool.Pool
}

// newCluster: a directory and nodes a, b (owning even / odd shards) plus
// empty nodes c, d. Each "node" is its own database.
func newCluster(t *testing.T) *cluster {
	t.Helper()
	c := &cluster{dir: testutil.NewDB(t), nodes: map[string]*pgxpool.Pool{}}
	for _, n := range []string{"a", "b", "c", "d"} {
		c.nodes[n] = testutil.NewDB(t)
	}
	err := shard.Bootstrap(context.Background(), c.dir,
		map[string]*pgxpool.Pool{"a": c.nodes["a"], "b": c.nodes["b"]},
		func(l int) string { return []string{"a", "b"}[l%2] })
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"c", "d"} {
		if err := shard.AddNode(context.Background(), c.nodes[n]); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

func (c *cluster) router() *shard.Router { return &shard.Router{Dir: c.dir, Nodes: c.nodes} }

// authorOn returns an author id whose posts land in logical shard l.
func authorOn(l int) int64 {
	for a := int64(1); ; a++ {
		if shard.LShardOfAuthor(a) == l {
			return a
		}
	}
}

func TestIDCarriesShard(t *testing.T) {
	for l := 0; l < shard.LogicalShards; l++ {
		got, ok := shard.LShardOfID(shard.NewPostID(l))
		if !ok || got != l {
			t.Fatalf("shard %d round-tripped as %d, %v", l, got, ok)
		}
	}
	if _, ok := shard.LShardOfID(uuid.New()); ok {
		t.Fatal("a random UUIDv4 must not parse as a sharded id")
	}
	counts := map[int]int{}
	for a := int64(1); a <= 64000; a++ {
		counts[shard.LShardOfAuthor(a)]++
	}
	for l, n := range counts {
		if n < 800 || n > 1200 { // expect 1000 each
			t.Fatalf("shard %d got %d of 64000 sequential authors; hash is uneven", l, n)
		}
	}
}

func TestRoutingAndScatterGather(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t)
	r := c.router()
	var ids []uuid.UUID
	for i := 0; i < 200; i++ {
		p, err := r.CreatePost(ctx, int64(i%50+1), fmt.Sprintf("post %d", i))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, p.ID)
	}
	for _, id := range ids {
		if _, err := r.GetPost(ctx, id); err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
	}
	var onA, onB int
	c.nodes["a"].QueryRow(ctx, `SELECT count(*) FROM shard_posts`).Scan(&onA) //nolint:errcheck
	c.nodes["b"].QueryRow(ctx, `SELECT count(*) FROM shard_posts`).Scan(&onB) //nolint:errcheck
	if onA == 0 || onB == 0 || onA+onB != 200 {
		t.Fatalf("rows a=%d b=%d; want both non-empty, 200 total", onA, onB)
	}
	byAuthor, err := r.ListByAuthor(ctx, 7, uuid.Nil, 100)
	if err != nil || len(byAuthor) != 4 {
		t.Fatalf("author 7 has %d posts (%v), want 4", len(byAuthor), err)
	}
	// Page through the global feed: every post exactly once, newest first.
	seen := map[uuid.UUID]bool{}
	var cursor uuid.UUID
	var prev *uuid.UUID
	for {
		page, err := r.ListRecent(ctx, cursor, 30)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		for _, p := range page {
			if seen[p.ID] {
				t.Fatalf("duplicate %s across pages", p.ID)
			}
			if prev != nil && p.ID.String() >= prev.String() {
				t.Fatal("feed not in descending id order")
			}
			id := p.ID
			prev = &id
			seen[p.ID] = true
		}
		cursor = page[len(page)-1].ID
	}
	if len(seen) != 200 {
		t.Fatalf("paged %d posts, want 200", len(seen))
	}
}

// The fence matters: a write that lands on the source after the copy and
// before the flip is lost by a naive move, and kept by a fenced one.
func TestNaiveMoveLosesWritesFencedMoveDoesNot(t *testing.T) {
	ctx := context.Background()
	for _, naive := range []bool{true, false} {
		c := newCluster(t)
		r := c.router()
		const l = 4 // owned by a
		author := authorOn(l)
		if _, err := r.CreatePost(ctx, author, "before move"); err != nil {
			t.Fatal(err)
		}
		var late shard.Post
		m := &shard.Mover{R: r, Naive: naive, AfterCopy: func() {
			var err error
			late, err = r.CreatePost(ctx, author, "written during the move")
			if err != nil {
				t.Fatal(err)
			}
		}}
		if _, err := m.Move(ctx, l, "c"); err != nil {
			t.Fatal(err)
		}
		_, err := r.GetPost(ctx, late.ID)
		if naive && !errors.Is(err, shard.ErrNotFound) {
			t.Fatalf("naive move: late write err = %v; expected it to be lost (ErrNotFound)", err)
		}
		if !naive && err != nil {
			t.Fatalf("fenced move lost an acknowledged write: %v", err)
		}
	}
}

// A router with an old map writes to the old owner, gets ErrMoved, reloads
// and succeeds on the new owner.
func TestStaleRouterIsRedirected(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t)
	const l = 9 // owned by b
	author := authorOn(l)
	stale := c.router()
	var retries atomic.Int64
	stale.OnRetry = func() { retries.Add(1) }
	p, err := stale.CreatePost(ctx, author, "v1") // loads the map now
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&shard.Mover{R: c.router()}).Move(ctx, l, "d"); err != nil {
		t.Fatal(err)
	}
	if stale.Placement()[l] != "b" {
		t.Fatal("test setup: the stale router should still map the shard to b")
	}
	up, err := stale.UpdatePost(ctx, p.ID, author, "v2")
	if err != nil || up.Version != 2 {
		t.Fatalf("update through stale router: %+v, %v", up, err)
	}
	if retries.Load() == 0 {
		t.Fatal("expected at least one ErrMoved retry")
	}
	var onB int
	c.nodes["b"].QueryRow(ctx, `SELECT count(*) FROM shard_posts WHERE lshard = $1`, l).Scan(&onB) //nolint:errcheck
	if onB != 0 {
		t.Fatalf("source still has %d rows of the moved shard after cleanup", onB)
	}
}

// Rebalance 2 -> 4 nodes while writers create and edit posts: no errors,
// and every acknowledged write is on its shard's owner afterwards.
func TestRebalanceUnderLoadKeepsEveryWrite(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t)
	r := c.router()
	for i := 0; i < 2000; i++ { // existing data to copy
		if _, err := r.CreatePost(ctx, int64(i%500+1), "seed"); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	acked := map[uuid.UUID]int64{} // id -> highest acknowledged version
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var writeErrs atomic.Int64
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wr := c.router() // each writer is its own "API instance"
			var mine []shard.Post
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				var p shard.Post
				var err error
				if i%3 == 2 && len(mine) > 0 {
					old := mine[i%len(mine)]
					p, err = wr.UpdatePost(ctx, old.ID, old.AuthorID, fmt.Sprint("edit ", i))
				} else {
					p, err = wr.CreatePost(ctx, int64(w*1000+i%97+1), fmt.Sprint("new ", i))
					mine = append(mine, p)
				}
				if err != nil {
					writeErrs.Add(1)
					t.Errorf("write failed during rebalance: %v", err)
					return
				}
				mu.Lock()
				if p.Version > acked[p.ID] {
					acked[p.ID] = p.Version
				}
				mu.Unlock()
			}
		}()
	}
	time.Sleep(200 * time.Millisecond)
	var moves int
	var maxFence time.Duration
	err := (&shard.Mover{R: r}).Rebalance(ctx, func(l int) string { return []string{"a", "b", "c", "d"}[l%4] },
		func(st shard.MoveStats) {
			moves++
			maxFence = max(maxFence, st.Fence)
		})
	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if moves != 32 {
		t.Fatalf("moved %d shards, want 32 (half of 64)", moves)
	}
	t.Logf("32 moves under load, %d acknowledged posts, max fence %s", len(acked), maxFence)

	check := c.router()
	lost := 0
	for id, v := range acked {
		p, err := check.GetPost(ctx, id)
		if err != nil || p.Version < v {
			lost++
		}
	}
	if lost > 0 {
		t.Fatalf("%d of %d acknowledged writes lost or rolled back", lost, len(acked))
	}
	// Exactly one owner per shard, and it is the directory's node.
	place := check.Placement()
	for l := 0; l < shard.LogicalShards; l++ {
		owners := 0
		for name, p := range c.nodes {
			var owned bool
			if p.QueryRow(ctx, `SELECT owned FROM shard_owner WHERE lshard = $1`, l).Scan(&owned) == nil && owned {
				owners++
				if name != place[l] {
					t.Fatalf("shard %d owned by %s but mapped to %s", l, name, place[l])
				}
			}
		}
		if owners != 1 {
			t.Fatalf("shard %d has %d owners", l, owners)
		}
	}
}

// A write transaction that started before a copy round and commits after it
// must still be copied. This is what the xmin horizon is for: a horizon of
// "highest xid seen" (xmax) would skip it.
func TestInFlightWriteDuringCopyIsNotSkipped(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t)
	r := c.router()
	const l = 6 // owned by a
	author := authorOn(l)
	if _, err := r.CreatePost(ctx, author, "existing"); err != nil {
		t.Fatal(err)
	}
	// A writer, exactly as Router.CreatePost does it, left open.
	tx, err := c.nodes["a"].Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT owned FROM shard_owner WHERE lshard = $1 FOR SHARE`, l); err != nil {
		t.Fatal(err)
	}
	slow := shard.NewPostID(l)
	if _, err := tx.Exec(ctx, `INSERT INTO shard_posts (id, lshard, author_id, message) VALUES ($1, $2, $3, 'slow')`,
		slow, l, author); err != nil {
		t.Fatal(err)
	}
	// Make later transactions newer than the slow one.
	for i := 0; i < 3; i++ {
		if _, err := r.CreatePost(ctx, author, "newer"); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan error, 1)
	go func() {
		_, err := (&shard.Mover{R: c.router()}).Move(ctx, l, "c")
		done <- err
	}()
	time.Sleep(300 * time.Millisecond) // copy rounds run; cutover waits on our FOR SHARE
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := r.GetPost(ctx, slow); err != nil {
		t.Fatalf("write that committed during the move was not copied: %v", err)
	}
}
