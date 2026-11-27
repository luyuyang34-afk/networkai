package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// Writes across a WAN.
//
// When the API runs in another region than the primary, every round trip to
// PostgreSQL costs a cross-region RTT (tens of milliseconds). The old
// CreatePost was BEGIN, INSERT post, INSERT outbox, SELECT, COMMIT: five
// round trips. Fetching the consistency token afterwards made six. At a
// 70 ms RTT that is about 420 ms per post, all waiting.
//
// Now:
//   - the post insert, the outbox insert and the read-back are one
//     statement (data-modifying CTE). A single statement is its own
//     transaction, so atomicity is unchanged;
//   - when the caller needs a consistency token, the token query is
//     pipelined behind it with its own Sync. It runs after the write
//     commits, so the LSN covers the commit record, and both go out in one
//     network round trip.

// TokenSink receives the post-commit WAL position from writes that can
// fetch it in the same round trip, so the HTTP layer does not need another
// query for X-Consistency-Token.
type TokenSink struct{ Token string }

const sinkKey ctxKey = 100

func WithTokenSink(ctx context.Context) (context.Context, *TokenSink) {
	s := &TokenSink{}
	return context.WithValue(ctx, sinkKey, s), s
}

func tokenSinkFrom(ctx context.Context) *TokenSink {
	s, _ := ctx.Value(sinkKey).(*TokenSink)
	return s
}

const createPostSQL = `
WITH p AS (
	INSERT INTO posts (id, author_id, message, media_type, media_key, media_url)
	VALUES ($1::uuid, $2::bigint, $3, $4, NULLIF($5, ''), NULLIF($6, ''))
	RETURNING *
), ev AS (
	INSERT INTO outbox (aggregate_type, aggregate_id, event_type)
	SELECT $7, p.id::text, $8 FROM p
)
SELECT p.id::text, p.author_id, u.username, p.message, p.media_type,
	COALESCE(p.media_key, ''), COALESCE(p.media_url, ''), p.version,
	(extract(epoch FROM p.created_at) * 1000000)::int8,
	(extract(epoch FROM p.updated_at) * 1000000)::int8
FROM p JOIN users u ON u.id = p.author_id`

// CreatePost inserts a post and its outbox event in one round trip (two
// statements in one pipeline if a TokenSink is attached).
func (s *Store) CreatePost(ctx context.Context, np NewPost) (Post, error) {
	if sink := tokenSinkFrom(ctx); sink != nil {
		return s.createPostPipelined(ctx, np, sink)
	}
	var vals [10]string
	ptrs := make([]any, len(vals))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	err := s.pool.QueryRow(ctx, createPostSQL, np.ID, np.AuthorID, np.Message, np.MediaType,
		np.MediaKey, np.MediaURL, AggregatePost, EventPostChanged).Scan(ptrs...)
	if err != nil {
		return Post{}, err
	}
	return postFromText(vals[:])
}

func (s *Store) createPostPipelined(ctx context.Context, np NewPost, sink *TokenSink) (Post, error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return Post{}, err
	}
	defer conn.Release()
	pl := conn.Conn().PgConn().StartPipeline(ctx)
	pl.SendQueryParams(createPostSQL, [][]byte{
		[]byte(np.ID.String()), []byte(strconv.FormatInt(np.AuthorID, 10)), []byte(np.Message),
		[]byte(np.MediaType), []byte(np.MediaKey), []byte(np.MediaURL),
		[]byte(AggregatePost), []byte(EventPostChanged),
	}, nil, nil, nil)
	pl.SendPipelineSync() // commits the insert
	pl.SendQueryParams(`SELECT pg_current_wal_lsn()::text`, nil, nil, nil, nil)
	pl.SendPipelineSync()
	if err := pl.Flush(); err != nil {
		pl.Close() //nolint:errcheck
		return Post{}, err
	}

	var row, lsn [][]byte
	var writeErr error
	read := func(dst *[][]byte) error {
		res, err := pl.GetResults()
		if err != nil {
			return err
		}
		rr, ok := res.(*pgconn.ResultReader)
		if !ok {
			return fmt.Errorf("pipeline: unexpected %T", res)
		}
		for rr.NextRow() {
			*dst = copyRow(rr.Values())
		}
		_, err = rr.Close()
		return err
	}
	sync := func() error {
		res, err := pl.GetResults()
		if err != nil {
			return err
		}
		if _, ok := res.(*pgconn.PipelineSync); !ok {
			return fmt.Errorf("pipeline: expected sync, got %T", res)
		}
		return nil
	}
	writeErr = read(&row)
	err = errors.Join(sync(), read(&lsn), sync(), pl.Close())
	if writeErr != nil {
		return Post{}, writeErr // keeps *pgconn.PgError for callers
	}
	if err != nil {
		return Post{}, err
	}
	if len(row) != 10 || len(lsn) != 1 {
		return Post{}, fmt.Errorf("pipeline: unexpected result shape")
	}
	vals := make([]string, len(row))
	for i, v := range row {
		vals[i] = string(v)
	}
	sink.Token = string(lsn[0])
	return postFromText(vals)
}

func copyRow(vs [][]byte) [][]byte {
	out := make([][]byte, len(vs))
	for i, v := range vs {
		out[i] = append([]byte(nil), v...)
	}
	return out
}

// postFromText parses createPostSQL's text-format columns.
func postFromText(v []string) (Post, error) {
	id, err := uuid.Parse(v[0])
	if err != nil {
		return Post{}, err
	}
	author, err1 := strconv.ParseInt(v[1], 10, 64)
	version, err2 := strconv.ParseInt(v[7], 10, 64)
	created, err3 := strconv.ParseInt(v[8], 10, 64)
	updated, err4 := strconv.ParseInt(v[9], 10, 64)
	if err := errors.Join(err1, err2, err3, err4); err != nil {
		return Post{}, err
	}
	return Post{ID: id, AuthorID: author, Author: v[2], Message: v[3], MediaType: v[4],
		MediaKey: v[5], MediaURL: v[6], Version: version,
		CreatedAt: time.UnixMicro(created).UTC(), UpdatedAt: time.UnixMicro(updated).UTC()}, nil
}
