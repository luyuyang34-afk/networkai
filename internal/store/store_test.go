package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"networkai/internal/store"
	"networkai/internal/testutil"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func outboxCount(t *testing.T, pool *pgxpool.Pool, postID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM outbox WHERE aggregate_id = $1`, postID.String()).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func mustUser(t *testing.T, s *store.Store, name string) store.User {
	t.Helper()
	u, err := s.CreateUser(context.Background(), name, "hash")
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestUsernameUniqueness(t *testing.T) {
	s := store.New(testutil.NewDB(t))
	mustUser(t, s, "alice")
	if _, err := s.CreateUser(context.Background(), "alice", "h"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate username: got %v, want ErrConflict", err)
	}
}

// Every post mutation must enqueue exactly one outbox event in the same
// transaction.
func TestPostMutationsWriteOutbox(t *testing.T) {
	pool := testutil.NewDB(t)
	s := store.New(pool)
	ctx := context.Background()
	alice := mustUser(t, s, "alice")

	p, err := s.CreatePost(ctx, store.NewPost{ID: uuid.New(), AuthorID: alice.ID, Message: "hello", MediaType: "none"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Version != 1 || p.Author != "alice" {
		t.Fatalf("created post = %+v", p)
	}
	if got := outboxCount(t, pool, p.ID); got != 1 {
		t.Fatalf("outbox rows after create = %d, want 1", got)
	}

	p, err = s.UpdatePostMessage(ctx, p.ID, alice.ID, "edited")
	if err != nil {
		t.Fatal(err)
	}
	if p.Version != 2 || p.Message != "edited" {
		t.Fatalf("updated post = %+v", p)
	}

	p, err = s.DeletePost(ctx, p.ID, alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p.Version != 3 || p.DeletedAt == nil {
		t.Fatalf("deleted post = %+v", p)
	}
	if got := outboxCount(t, pool, p.ID); got != 3 {
		t.Fatalf("outbox rows after create+update+delete = %d, want 3", got)
	}

	if _, err := s.GetPost(ctx, p.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetPost on deleted post: %v", err)
	}
	if _, err := s.GetPostForIndex(ctx, p.ID); err != nil {
		t.Fatalf("indexer must still see the soft-deleted post: %v", err)
	}
}

// The core outbox guarantee: if the post write fails, no event is left
// behind (and vice versa). We force a failure with the CHECK constraint.
func TestFailedPostWriteLeavesNoOutboxEvent(t *testing.T) {
	pool := testutil.NewDB(t)
	s := store.New(pool)
	alice := mustUser(t, s, "alice")
	id := uuid.New()

	_, err := s.CreatePost(context.Background(), store.NewPost{
		ID: id, AuthorID: alice.ID, Message: strings.Repeat("x", 5001), MediaType: "none",
	})
	if err == nil {
		t.Fatal("expected CHECK constraint violation")
	}
	if got := outboxCount(t, pool, id); got != 0 {
		t.Fatalf("outbox rows after failed insert = %d, want 0 (transaction must roll back both)", got)
	}
}

func TestOwnership(t *testing.T) {
	pool := testutil.NewDB(t)
	s := store.New(pool)
	ctx := context.Background()
	alice, bob := mustUser(t, s, "alice"), mustUser(t, s, "bob")
	p, _ := s.CreatePost(ctx, store.NewPost{ID: uuid.New(), AuthorID: alice.ID, Message: "mine", MediaType: "none"})

	if _, err := s.UpdatePostMessage(ctx, p.ID, bob.ID, "hijack"); !errors.Is(err, store.ErrForbidden) {
		t.Errorf("bob editing alice's post: %v, want ErrForbidden", err)
	}
	if _, err := s.DeletePost(ctx, p.ID, bob.ID); !errors.Is(err, store.ErrForbidden) {
		t.Errorf("bob deleting alice's post: %v, want ErrForbidden", err)
	}
	if _, err := s.DeletePost(ctx, uuid.New(), alice.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("deleting missing post: %v, want ErrNotFound", err)
	}
	if got := outboxCount(t, pool, p.ID); got != 1 {
		t.Errorf("rejected mutations must not enqueue events; outbox rows = %d", got)
	}
	// Deleting twice: the second call sees a deleted post.
	if _, err := s.DeletePost(ctx, p.ID, alice.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeletePost(ctx, p.ID, alice.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second delete: %v, want ErrNotFound", err)
	}
}

// Keyset pagination must return every post exactly once, newest first, even
// when many posts share the same created_at timestamp.
func TestListPostsKeysetPagination(t *testing.T) {
	pool := testutil.NewDB(t)
	s := store.New(pool)
	ctx := context.Background()
	alice, bob := mustUser(t, s, "alice"), mustUser(t, s, "bob")

	want := map[uuid.UUID]bool{}
	for i := 0; i < 23; i++ {
		author := alice
		if i%3 == 0 {
			author = bob
		}
		p, err := s.CreatePost(ctx, store.NewPost{ID: uuid.New(), AuthorID: author.ID, Message: "m", MediaType: "none"})
		if err != nil {
			t.Fatal(err)
		}
		want[p.ID] = true
	}
	// Force timestamp ties so the id tie-breaker is exercised.
	if _, err := pool.Exec(ctx, `UPDATE posts SET created_at = date_trunc('minute', created_at)`); err != nil {
		t.Fatal(err)
	}

	seen := map[uuid.UUID]bool{}
	var after *store.PageCursor
	var prev *store.Post
	pages := 0
	for {
		page, next, err := s.ListPosts(ctx, store.ListParams{After: after, Limit: 5})
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, p := range page {
			if seen[p.ID] {
				t.Fatalf("post %s returned twice", p.ID)
			}
			seen[p.ID] = true
			if prev != nil && (p.CreatedAt.After(prev.CreatedAt) ||
				(p.CreatedAt.Equal(prev.CreatedAt) && p.ID.String() > prev.ID.String())) {
				t.Fatalf("ordering violated: %s after %s", p.ID, prev.ID)
			}
			pp := p
			prev = &pp
		}
		if next == nil {
			break
		}
		after = next
	}
	if len(seen) != len(want) || pages != 5 {
		t.Fatalf("saw %d posts in %d pages, want %d in 5", len(seen), pages, len(want))
	}

	bobs, _, err := s.ListPosts(ctx, store.ListParams{Author: "bob", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(bobs) != 8 {
		t.Fatalf("bob has %d posts, want 8", len(bobs))
	}
}
