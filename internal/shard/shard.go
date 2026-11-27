// Package shard splits posts across several PostgreSQL nodes and moves data
// between them online, while reads and writes continue.
//
// The layout follows what Instagram, Notion and Figma describe publicly:
//
//   - 64 logical shards are fixed forever. A post's logical shard is a hash
//     of its author, so one author's posts live together and "posts by X"
//     reads one node.
//   - A directory (shard_map, in the global database) maps each logical
//     shard to a physical node. Adding nodes moves whole logical shards;
//     rows are never rehashed.
//   - Post ids are UUIDv7-style, time-ordered, with the logical shard in one
//     byte. A lookup by id needs no directory query beyond the cached map.
//   - Each node has a shard_owner table, the node's own record of which
//     logical shards it may serve. Every write checks it inside its
//     transaction (SELECT ... FOR SHARE). That makes the node, not the
//     possibly stale router cache, the authority. A router with an old map
//     gets ErrMoved, reloads the map and retries.
//
// See mover.go for online resharding, and ADR 0009.
package shard

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/singleflight"
)

// LogicalShards is fixed: changing it would rehash every row. Physical
// nodes can go from 1 to 64 without touching it.
const LogicalShards = 64

var (
	// ErrMoved: this node does not own the logical shard (any more, or yet).
	ErrMoved    = errors.New("shard: logical shard not owned by this node")
	ErrNotFound = errors.New("shard: post not found")
)

const nodeSchema = `
CREATE TABLE IF NOT EXISTS shard_owner (
	lshard smallint PRIMARY KEY,
	owned  boolean  NOT NULL
);
CREATE TABLE IF NOT EXISTS shard_posts (
	id         uuid        PRIMARY KEY,
	lshard     smallint    NOT NULL,
	author_id  bigint      NOT NULL,
	message    text        NOT NULL,
	version    bigint      NOT NULL DEFAULT 1,
	created_at timestamptz NOT NULL DEFAULT now(),
	updated_at timestamptz NOT NULL DEFAULT now(),
	deleted_at timestamptz,
	-- transaction that last wrote the row; drives incremental copy
	writer_xid xid8        NOT NULL DEFAULT pg_current_xact_id()
);
CREATE INDEX IF NOT EXISTS shard_posts_author  ON shard_posts (author_id, id DESC);
CREATE INDEX IF NOT EXISTS shard_posts_recent  ON shard_posts (id DESC);
CREATE INDEX IF NOT EXISTS shard_posts_changes ON shard_posts (lshard, writer_xid);
`

const directorySchema = `
CREATE TABLE IF NOT EXISTS shard_map (
	lshard smallint PRIMARY KEY,
	node   text     NOT NULL,
	epoch  bigint   NOT NULL DEFAULT 1
);
`

// NewPostID returns a time-ordered id that carries its logical shard.
// Layout: UUIDv7 timestamp and randomness, version nibble 8 ("custom"),
// byte 9 = logical shard.
func NewPostID(lshard int) uuid.UUID {
	id := uuid.Must(uuid.NewV7())
	id[6] = 0x80 | (id[6] & 0x0f)
	id[9] = byte(lshard)
	return id
}

// LShardOfID extracts the logical shard; false for ids not made by NewPostID.
func LShardOfID(id uuid.UUID) (int, bool) {
	if id.Version() != 8 || int(id[9]) >= LogicalShards {
		return 0, false
	}
	return int(id[9]), true
}

// LShardOfAuthor hashes the author (splitmix64 finalizer), so sequential
// user ids spread evenly instead of filling shards in order.
func LShardOfAuthor(authorID int64) int {
	z := uint64(authorID) + 0x9e3779b97f4a7c15
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	z ^= z >> 31
	return int(z % LogicalShards)
}

// Bootstrap creates schemas and the initial placement. place maps a logical
// shard to a node name in nodes.
func Bootstrap(ctx context.Context, dir *pgxpool.Pool, nodes map[string]*pgxpool.Pool, place func(lshard int) string) error {
	if _, err := dir.Exec(ctx, directorySchema); err != nil {
		return fmt.Errorf("directory schema: %w", err)
	}
	for name, p := range nodes {
		if _, err := p.Exec(ctx, nodeSchema); err != nil {
			return fmt.Errorf("node %s schema: %w", name, err)
		}
	}
	for l := 0; l < LogicalShards; l++ {
		n := place(l)
		p, ok := nodes[n]
		if !ok {
			return fmt.Errorf("placement names unknown node %q", n)
		}
		if _, err := dir.Exec(ctx, `INSERT INTO shard_map (lshard, node) VALUES ($1, $2) ON CONFLICT DO NOTHING`, l, n); err != nil {
			return err
		}
		if _, err := p.Exec(ctx, `INSERT INTO shard_owner VALUES ($1, true) ON CONFLICT DO NOTHING`, l); err != nil {
			return err
		}
	}
	return nil
}

// AddNode creates the schema on a new, empty node. It owns nothing until a
// Mover moves shards to it.
func AddNode(ctx context.Context, p *pgxpool.Pool) error {
	_, err := p.Exec(ctx, nodeSchema)
	return err
}

// Router sends each request to the node that owns its logical shard.
type Router struct {
	Dir   *pgxpool.Pool
	Nodes map[string]*pgxpool.Pool
	// MaxWait bounds how long a request retries while its shard is being
	// cut over (default 2s).
	MaxWait time.Duration
	// OnRetry counts ErrMoved retries (stale map or cutover in progress).
	OnRetry func()

	mu      sync.RWMutex
	m       [LogicalShards]string
	loaded  bool
	refresh singleflight.Group
}

// Refresh reloads the directory. Concurrent callers share one query.
func (r *Router) Refresh(ctx context.Context) error {
	_, err, _ := r.refresh.Do("map", func() (any, error) {
		rows, err := r.Dir.Query(ctx, `SELECT lshard, node FROM shard_map`)
		if err != nil {
			return nil, err
		}
		var m [LogicalShards]string
		n := 0
		for rows.Next() {
			var l int16
			var node string
			if err := rows.Scan(&l, &node); err != nil {
				return nil, err
			}
			m[l] = node
			n++
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if n != LogicalShards {
			return nil, fmt.Errorf("shard map has %d of %d logical shards", n, LogicalShards)
		}
		r.mu.Lock()
		r.m, r.loaded = m, true
		r.mu.Unlock()
		return nil, nil
	})
	return err
}

// Placement returns a copy of the cached map.
func (r *Router) Placement() [LogicalShards]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.m
}

func (r *Router) pool(ctx context.Context, l int) (*pgxpool.Pool, error) {
	r.mu.RLock()
	loaded := r.loaded
	r.mu.RUnlock()
	if !loaded {
		if err := r.Refresh(ctx); err != nil {
			return nil, err
		}
	}
	r.mu.RLock()
	name := r.m[l]
	r.mu.RUnlock()
	p, ok := r.Nodes[name]
	if !ok {
		return nil, fmt.Errorf("shard map names unknown node %q", name)
	}
	return p, nil
}

// on runs fn against the owner of logical shard l, refreshing the map and
// retrying with backoff while the node answers ErrMoved.
func (r *Router) on(ctx context.Context, l int, fn func(p *pgxpool.Pool) error) error {
	deadline := time.Now().Add(or(r.MaxWait, 2*time.Second))
	backoff := 2 * time.Millisecond
	for {
		p, err := r.pool(ctx, l)
		if err != nil {
			return err
		}
		err = fn(p)
		if !errors.Is(err, ErrMoved) || time.Now().After(deadline) {
			return err
		}
		if r.OnRetry != nil {
			r.OnRetry()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 50*time.Millisecond)
		if err := r.Refresh(ctx); err != nil {
			return err
		}
	}
}

// requireOwned is the per-node fence check. FOR SHARE lets writers run
// concurrently but blocks while a Mover holds FOR UPDATE on the row, and
// after it commits the re-read sees owned = false.
func requireOwned(ctx context.Context, tx pgx.Tx, l int) error {
	var owned bool
	err := tx.QueryRow(ctx, `SELECT owned FROM shard_owner WHERE lshard = $1 FOR SHARE`, l).Scan(&owned)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !owned) {
		return ErrMoved
	}
	return err
}

type Post struct {
	ID        uuid.UUID
	AuthorID  int64
	Message   string
	Version   int64
	CreatedAt time.Time
	Deleted   bool
}

func (r *Router) CreatePost(ctx context.Context, authorID int64, message string) (Post, error) {
	l := LShardOfAuthor(authorID)
	p := Post{ID: NewPostID(l), AuthorID: authorID, Message: message}
	err := r.on(ctx, l, func(pool *pgxpool.Pool) error {
		return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			if err := requireOwned(ctx, tx, l); err != nil {
				return err
			}
			return tx.QueryRow(ctx, `INSERT INTO shard_posts (id, lshard, author_id, message)
				VALUES ($1, $2, $3, $4) RETURNING version, created_at`,
				p.ID, l, authorID, message).Scan(&p.Version, &p.CreatedAt)
		})
	})
	return p, err
}

func (r *Router) UpdatePost(ctx context.Context, id uuid.UUID, authorID int64, message string) (Post, error) {
	l, ok := LShardOfID(id)
	if !ok {
		return Post{}, ErrNotFound
	}
	p := Post{ID: id, AuthorID: authorID, Message: message}
	err := r.on(ctx, l, func(pool *pgxpool.Pool) error {
		return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			if err := requireOwned(ctx, tx, l); err != nil {
				return err
			}
			err := tx.QueryRow(ctx, `UPDATE shard_posts
				SET message = $3, version = version + 1, updated_at = now(), writer_xid = pg_current_xact_id()
				WHERE id = $1 AND author_id = $2 AND deleted_at IS NULL
				RETURNING version, created_at`, id, authorID, message).Scan(&p.Version, &p.CreatedAt)
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		})
	})
	return p, err
}

// GetPost checks ownership and reads the row in one statement, so a node
// that just gave the shard away answers ErrMoved instead of an old copy.
func (r *Router) GetPost(ctx context.Context, id uuid.UUID) (Post, error) {
	l, ok := LShardOfID(id)
	if !ok {
		return Post{}, ErrNotFound
	}
	var p Post
	err := r.on(ctx, l, func(pool *pgxpool.Pool) error {
		var owned bool
		var author, version *int64
		var msg *string
		var created *time.Time
		var deleted *bool
		err := pool.QueryRow(ctx, `SELECT o.owned, p.author_id, p.message, p.version, p.created_at, p.deleted_at IS NOT NULL
			FROM shard_owner o LEFT JOIN shard_posts p ON p.id = $2
			WHERE o.lshard = $1`, l, id).Scan(&owned, &author, &msg, &version, &created, &deleted)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !owned) {
			return ErrMoved
		}
		if err != nil {
			return err
		}
		if author == nil || *deleted {
			return ErrNotFound
		}
		p = Post{ID: id, AuthorID: *author, Message: *msg, Version: *version, CreatedAt: *created}
		return nil
	})
	return p, err
}

// ListByAuthor is a single-shard query: the reason to shard by author.
func (r *Router) ListByAuthor(ctx context.Context, authorID int64, before uuid.UUID, limit int) ([]Post, error) {
	l := LShardOfAuthor(authorID)
	var out []Post
	err := r.on(ctx, l, func(pool *pgxpool.Pool) error {
		// One snapshot for the ownership check and the read.
		return pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
			var owned bool
			err := tx.QueryRow(ctx, `SELECT owned FROM shard_owner WHERE lshard = $1`, l).Scan(&owned)
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && !owned) {
				return ErrMoved
			}
			if err != nil {
				return err
			}
			out, err = scanPosts(tx.Query(ctx, `SELECT id, author_id, message, version, created_at FROM shard_posts
				WHERE author_id = $1 AND id < $2 AND deleted_at IS NULL ORDER BY id DESC LIMIT $3`,
				authorID, cursor(before), limit))
			return err
		})
	})
	return out, err
}

// ListRecent is the scatter-gather query: every node returns its newest
// posts (only from shards it owns right now) and the router merges them.
// Its latency is the slowest node's latency.
func (r *Router) ListRecent(ctx context.Context, before uuid.UUID, limit int) ([]Post, error) {
	type res struct {
		posts []Post
		err   error
	}
	names := make([]string, 0, len(r.Nodes))
	for n := range r.Nodes {
		names = append(names, n)
	}
	results := make([]res, len(names))
	var wg sync.WaitGroup
	for i, n := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ps, err := scanPosts(r.Nodes[n].Query(ctx, `SELECT id, author_id, message, version, created_at FROM shard_posts
				WHERE lshard IN (SELECT lshard FROM shard_owner WHERE owned)
				  AND id < $1 AND deleted_at IS NULL ORDER BY id DESC LIMIT $2`, cursor(before), limit))
			results[i] = res{ps, err}
		}()
	}
	wg.Wait()
	var all []Post
	for i, rs := range results {
		if rs.err != nil {
			return nil, fmt.Errorf("node %s: %w", names[i], rs.err)
		}
		all = append(all, rs.posts...)
	}
	sort.Slice(all, func(i, j int) bool { return lessID(all[j].ID, all[i].ID) })
	if len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

func cursor(before uuid.UUID) uuid.UUID {
	if before == uuid.Nil {
		return uuid.Max
	}
	return before
}

func lessID(a, b uuid.UUID) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

func scanPosts(rows pgx.Rows, err error) ([]Post, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Post, error) {
		var p Post
		err := row.Scan(&p.ID, &p.AuthorID, &p.Message, &p.Version, &p.CreatedAt)
		return p, err
	})
}

func or(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}
