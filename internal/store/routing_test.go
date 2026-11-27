package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"networkai/internal/store"
	"networkai/internal/testutil"

	"github.com/google/uuid"
)

type routes struct {
	mu   sync.Mutex
	seen []string
}

func (r *routes) add(t string) { r.mu.Lock(); r.seen = append(r.seen, t); r.mu.Unlock() }
func (r *routes) last() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.seen) == 0 {
		return ""
	}
	return r.seen[len(r.seen)-1]
}

// The replica applies WAL with a deliberate delay (TEST_REPLICA_URL points
// at a replica with recovery_min_apply_delay), so the classic bug is
// reproducible: a user writes, reads back from the replica, and their own
// write is missing. A consistency token fixes it.
func TestReadYourWritesAcrossALaggingReplica(t *testing.T) {
	primary := testutil.NewDB(t)
	replica := testutil.Replica(t, primary)
	var r routes
	st := store.New(primary).WithReplica(replica, r.add)
	ctx := context.Background()

	u, err := st.CreateUser(ctx, "alice", "h")
	if err != nil {
		t.Fatal(err)
	}
	p, err := st.CreatePost(ctx, store.NewPost{ID: uuid.New(), AuthorID: u.ID, Message: "hi", MediaType: "none"})
	if err != nil {
		t.Fatal(err)
	}
	token, err := st.WriteToken(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// 1. The bug: an untokened read right after the write hits the lagging replica.
	if _, err := st.GetPost(ctx, p.ID); !errors.Is(err, store.ErrNotFound) || r.last() != "replica" {
		t.Fatalf("expected the lag bug to reproduce (not found on replica); got err=%v route=%s", err, r.last())
	}
	// 2. The fix: with the token, the read falls back to the primary.
	tctx := store.WithConsistencyToken(ctx, token)
	if got, err := st.GetPost(tctx, p.ID); err != nil || got.ID != p.ID || r.last() != "primary_token" {
		t.Fatalf("token read: err=%v route=%s", err, r.last())
	}
	// 3. Once the replica has caught up, token reads go back to the replica.
	deadline := time.Now().Add(15 * time.Second)
	for {
		got, err := st.GetPost(tctx, p.ID)
		if err == nil && got.ID == p.ID && r.last() == "replica" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("replica never caught up: err=%v route=%s", err, r.last())
		}
		time.Sleep(100 * time.Millisecond)
	}
	// 4. Cache fills can force the primary.
	if _, err := st.GetPost(store.ForcePrimary(ctx), p.ID); err != nil || r.last() != "primary_forced" {
		t.Fatalf("forced: err=%v route=%s", err, r.last())
	}
}

func TestReplicaLagIsReported(t *testing.T) {
	primary := testutil.NewDB(t)
	replica := testutil.Replica(t, primary)
	st := store.New(primary).WithReplica(replica, nil)
	ctx := context.Background()
	u, _ := st.CreateUser(ctx, "bob", "h")
	for i := 0; i < 20; i++ {
		st.CreatePost(ctx, store.NewPost{ID: uuid.New(), AuthorID: u.ID, Message: "x", MediaType: "none"}) //nolint:errcheck
	}
	bytes, _, ok, err := st.ReplicaLag(ctx)
	if err != nil || !ok || bytes <= 0 {
		t.Fatalf("lag bytes=%v ok=%v err=%v; want > 0 right after writes on a delayed replica", bytes, ok, err)
	}
}
