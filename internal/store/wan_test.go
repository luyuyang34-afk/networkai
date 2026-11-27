package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"networkai/internal/store"
	"networkai/internal/testutil"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// The pipelined write (post + outbox + token in one round trip) must behave
// exactly like the plain one.
func TestPipelinedCreatePostWithToken(t *testing.T) {
	ctx := context.Background()
	pool := testutil.NewDB(t)
	s := store.New(pool)
	alice := mustUser(t, s, "alice")

	before, err := s.WriteToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wctx, sink := store.WithTokenSink(ctx)
	id := uuid.New()
	p, err := s.CreatePost(wctx, store.NewPost{ID: id, AuthorID: alice.ID, Message: "hi", MediaType: "none"})
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != id || p.Author != "alice" || p.Message != "hi" || p.Version != 1 || p.CreatedAt.IsZero() {
		t.Fatalf("returned post %+v", p)
	}
	if outboxCount(t, pool, id) != 1 {
		t.Fatal("outbox event missing")
	}
	// The token was taken after the commit: it is past the earlier LSN,
	// and the primary considers it reached.
	var past, reached bool
	if err := pool.QueryRow(ctx, `SELECT $1::pg_lsn > $2::pg_lsn, pg_current_wal_lsn() >= $1::pg_lsn`,
		sink.Token, before).Scan(&past, &reached); err != nil {
		t.Fatalf("token %q: %v", sink.Token, err)
	}
	if !past || !reached {
		t.Fatalf("token %s not after %s (past=%v reached=%v)", sink.Token, before, past, reached)
	}

	// Plain path returns the same shape.
	q, err := s.CreatePost(ctx, store.NewPost{ID: uuid.New(), AuthorID: alice.ID, Message: "plain", MediaType: "none"})
	if err != nil || q.Author != "alice" || q.Version != 1 {
		t.Fatalf("plain create: %+v, %v", q, err)
	}
	got, err := s.GetPost(ctx, id)
	if err != nil || !got.CreatedAt.Equal(p.CreatedAt) {
		t.Fatalf("read back %+v (%v); created_at should match %v", got, err, p.CreatedAt)
	}
}

func TestPipelinedCreatePostErrors(t *testing.T) {
	ctx := context.Background()
	pool := testutil.NewDB(t)
	s := store.New(pool)
	alice := mustUser(t, s, "alice")
	wctx, sink := store.WithTokenSink(ctx)

	// A failing write leaves no outbox row and no token, and the error is
	// still a PostgreSQL error (callers map constraint codes).
	id := uuid.New()
	_, err := s.CreatePost(wctx, store.NewPost{ID: id, AuthorID: alice.ID, Message: strings.Repeat("x", 5001), MediaType: "none"})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("err = %v, want CHECK violation (23514)", err)
	}
	if outboxCount(t, pool, id) != 0 || sink.Token != "" {
		t.Fatalf("failed write left outbox=%d token=%q", outboxCount(t, pool, id), sink.Token)
	}
	// The connection is reusable afterwards.
	for i := 0; i < 20; i++ {
		if _, err := s.CreatePost(wctx, store.NewPost{ID: uuid.New(), AuthorID: alice.ID, Message: "ok", MediaType: "none"}); err != nil {
			t.Fatalf("write %d after a failed pipeline: %v", i, err)
		}
	}
}
