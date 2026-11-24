// Package store is the PostgreSQL data access layer.
//
// Every write to a post also inserts an outbox row in the same transaction.
// That is the whole trick of the transactional outbox pattern: the post change
// and the "please re-index this post" message commit or roll back together, so
// Elasticsearch can lag behind but can never silently miss a change.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound  = errors.New("not found")
	ErrConflict  = errors.New("already exists")
	ErrForbidden = errors.New("forbidden")
)

const (
	AggregatePost    = "post"
	EventPostChanged = "post.changed"
)

type User struct {
	ID           int64
	Username     string
	PasswordHash string
	CreatedAt    time.Time
}

type Post struct {
	ID        uuid.UUID
	AuthorID  int64
	Author    string
	Message   string
	MediaType string
	MediaKey  string
	MediaURL  string
	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time
}

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// ---------- users ----------

func (s *Store) CreateUser(ctx context.Context, username, passwordHash string) (User, error) {
	u := User{Username: username, PasswordHash: passwordHash}
	err := s.pool.QueryRow(ctx,
		`INSERT INTO users (username, password_hash) VALUES ($1, $2) RETURNING id, created_at`,
		username, passwordHash).Scan(&u.ID, &u.CreatedAt)
	if isUniqueViolation(err) {
		return User{}, ErrConflict
	}
	return u, err
}

func (s *Store) GetUserByUsername(ctx context.Context, username string) (User, error) {
	var u User
	err := s.pool.QueryRow(ctx,
		`SELECT id, username, password_hash, created_at FROM users WHERE username = $1`, username).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

// ---------- posts ----------

type NewPost struct {
	ID        uuid.UUID
	AuthorID  int64
	Message   string
	MediaType string
	MediaKey  string
	MediaURL  string
}

const postColumns = `p.id, p.author_id, u.username, p.message, p.media_type,
	COALESCE(p.media_key, ''), COALESCE(p.media_url, ''), p.version,
	p.created_at, p.updated_at, p.deleted_at`

func scanPost(row pgx.Row) (Post, error) {
	var p Post
	err := row.Scan(&p.ID, &p.AuthorID, &p.Author, &p.Message, &p.MediaType,
		&p.MediaKey, &p.MediaURL, &p.Version, &p.CreatedAt, &p.UpdatedAt, &p.DeletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Post{}, ErrNotFound
	}
	return p, err
}

func (s *Store) CreatePost(ctx context.Context, np NewPost) (Post, error) {
	var p Post
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO posts (id, author_id, message, media_type, media_key, media_url)
			 VALUES ($1, $2, $3, $4, NULLIF($5, ''), NULLIF($6, ''))`,
			np.ID, np.AuthorID, np.Message, np.MediaType, np.MediaKey, np.MediaURL)
		if err != nil {
			return err
		}
		if err := enqueuePostChanged(ctx, tx, np.ID); err != nil {
			return err
		}
		p, err = scanPost(tx.QueryRow(ctx,
			`SELECT `+postColumns+` FROM posts p JOIN users u ON u.id = p.author_id WHERE p.id = $1`, np.ID))
		return err
	})
	return p, err
}

// UpdatePostMessage edits a post owned by authorID. It returns ErrNotFound if
// the post does not exist (or is deleted) and ErrForbidden if someone else
// owns it.
func (s *Store) UpdatePostMessage(ctx context.Context, id uuid.UUID, authorID int64, message string) (Post, error) {
	return s.mutateOwnedPost(ctx, id, authorID,
		`UPDATE posts SET message = $2, version = version + 1, updated_at = now() WHERE id = $1`, message)
}

// DeletePost soft-deletes a post owned by authorID and returns its final state
// (the caller uses MediaKey to clean up the stored file).
func (s *Store) DeletePost(ctx context.Context, id uuid.UUID, authorID int64) (Post, error) {
	return s.mutateOwnedPost(ctx, id, authorID,
		`UPDATE posts SET deleted_at = now(), version = version + 1, updated_at = now() WHERE id = $1`)
}

func (s *Store) mutateOwnedPost(ctx context.Context, id uuid.UUID, authorID int64, update string, args ...any) (Post, error) {
	var p Post
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// Lock the row so the ownership check and the update are atomic.
		var owner int64
		err := tx.QueryRow(ctx,
			`SELECT author_id FROM posts WHERE id = $1 AND deleted_at IS NULL FOR UPDATE`, id).Scan(&owner)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if owner != authorID {
			return ErrForbidden
		}
		if _, err := tx.Exec(ctx, update, append([]any{id}, args...)...); err != nil {
			return err
		}
		if err := enqueuePostChanged(ctx, tx, id); err != nil {
			return err
		}
		p, err = scanPost(tx.QueryRow(ctx,
			`SELECT `+postColumns+` FROM posts p JOIN users u ON u.id = p.author_id WHERE p.id = $1`, id))
		return err
	})
	return p, err
}

// GetPost returns a live (not deleted) post.
func (s *Store) GetPost(ctx context.Context, id uuid.UUID) (Post, error) {
	return scanPost(s.pool.QueryRow(ctx,
		`SELECT `+postColumns+` FROM posts p JOIN users u ON u.id = p.author_id
		 WHERE p.id = $1 AND p.deleted_at IS NULL`, id))
}

// GetPostForIndex returns a post including soft-deleted ones; the indexer
// needs deleted posts to propagate the deletion.
func (s *Store) GetPostForIndex(ctx context.Context, id uuid.UUID) (Post, error) {
	return scanPost(s.pool.QueryRow(ctx,
		`SELECT `+postColumns+` FROM posts p JOIN users u ON u.id = p.author_id WHERE p.id = $1`, id))
}

// PageCursor is a keyset pagination position: the (created_at, id) of the
// last item on the previous page.
type PageCursor struct {
	CreatedAt time.Time `json:"t"`
	ID        uuid.UUID `json:"id"`
}

type ListParams struct {
	Author string // optional username filter
	After  *PageCursor
	Limit  int
}

// ListPosts returns live posts newest-first using keyset pagination, which
// stays fast on deep pages (unlike OFFSET, which scans and discards rows).
func (s *Store) ListPosts(ctx context.Context, lp ListParams) ([]Post, *PageCursor, error) {
	q := `SELECT ` + postColumns + ` FROM posts p JOIN users u ON u.id = p.author_id
	      WHERE p.deleted_at IS NULL`
	var args []any
	if lp.Author != "" {
		args = append(args, lp.Author)
		q += fmt.Sprintf(" AND u.username = $%d", len(args))
	}
	if lp.After != nil {
		args = append(args, lp.After.CreatedAt, lp.After.ID)
		q += fmt.Sprintf(" AND (p.created_at, p.id) < ($%d, $%d)", len(args)-1, len(args))
	}
	// Fetch one extra row to know whether another page exists.
	args = append(args, lp.Limit+1)
	q += fmt.Sprintf(" ORDER BY p.created_at DESC, p.id DESC LIMIT $%d", len(args))

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	posts := make([]Post, 0, lp.Limit+1)
	for rows.Next() {
		p, err := scanPost(rows)
		if err != nil {
			return nil, nil, err
		}
		posts = append(posts, p)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	var next *PageCursor
	if len(posts) > lp.Limit {
		posts = posts[:lp.Limit]
		last := posts[len(posts)-1]
		next = &PageCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return posts, next, nil
}

// PostsForIndexAfter returns up to limit posts (including soft-deleted ones)
// with id > after, ordered by id. Used by the bulk reindex.
func (s *Store) PostsForIndexAfter(ctx context.Context, after uuid.UUID, limit int) ([]Post, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+postColumns+` FROM posts p JOIN users u ON u.id = p.author_id
		 WHERE p.id > $1 ORDER BY p.id LIMIT $2`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Post
	for rows.Next() {
		p, err := scanPost(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PostIDsUpdatedSince returns ids of posts (including deleted) changed at or
// after since. Used by the catch-up phase of a blue/green rebuild.
func (s *Store) PostIDsUpdatedSince(ctx context.Context, since time.Time) ([]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx, `SELECT id FROM posts WHERE updated_at >= $1`, since)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

// Now returns the database clock, so time windows compare against the same
// clock that stamps updated_at.
func (s *Store) Now(ctx context.Context) (time.Time, error) {
	var t time.Time
	err := s.pool.QueryRow(ctx, `SELECT now()`).Scan(&t)
	return t, err
}

// ---------- likes ----------

// Like records that userID likes a live post. Liking twice is a no-op.
func (s *Store) Like(ctx context.Context, userID int64, postID uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO likes (user_id, post_id)
		SELECT $1, id FROM posts WHERE id = $2 AND deleted_at IS NULL
		ON CONFLICT DO NOTHING`, userID, postID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		// Either already liked (fine) or the post does not exist.
		var exists bool
		if err := s.pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM posts WHERE id = $1 AND deleted_at IS NULL)`, postID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
	}
	return nil
}

func (s *Store) Unlike(ctx context.Context, userID int64, postID uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM likes WHERE user_id = $1 AND post_id = $2`, userID, postID)
	return err
}

// LikeInfo returns like counts for postIDs and which of them userID liked
// (userID 0 = anonymous).
func (s *Store) LikeInfo(ctx context.Context, postIDs []string, userID int64) (map[string]int, map[string]bool, error) {
	counts, err := s.LikeCounts(ctx, postIDs)
	if err != nil || userID == 0 {
		return counts, map[string]bool{}, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT post_id::text FROM likes WHERE user_id = $1 AND post_id = ANY($2::uuid[])`, userID, postIDs)
	if err != nil {
		return nil, nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, nil, err
	}
	mine := make(map[string]bool, len(ids))
	for _, id := range ids {
		mine[id] = true
	}
	return counts, mine, nil
}

func (s *Store) LikeCounts(ctx context.Context, postIDs []string) (map[string]int, error) {
	out := map[string]int{}
	if len(postIDs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT post_id::text, count(*) FROM likes WHERE post_id = ANY($1::uuid[]) GROUP BY post_id`, postIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

func (s *Store) RecentLikedPostIDs(ctx context.Context, userID int64, limit int) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT l.post_id::text FROM likes l JOIN posts p ON p.id = l.post_id
		WHERE l.user_id = $1 AND p.deleted_at IS NULL
		ORDER BY l.created_at DESC LIMIT $2`, userID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func (s *Store) AuthorAffinity(ctx context.Context, userID int64) (map[string]int, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT u.username, count(*) FROM likes l
		JOIN posts p ON p.id = l.post_id JOIN users u ON u.id = p.author_id
		WHERE l.user_id = $1 GROUP BY u.username`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var name string
		var n int
		if err := rows.Scan(&name, &n); err != nil {
			return nil, err
		}
		out[name] = n
	}
	return out, rows.Err()
}

// MediaKeysInUse returns which of keys are referenced by a live post.
func (s *Store) MediaKeysInUse(ctx context.Context, keys []string) (map[string]bool, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT media_key FROM posts WHERE media_key = ANY($1) AND deleted_at IS NULL`, keys)
	if err != nil {
		return nil, err
	}
	used, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(used))
	for _, k := range used {
		out[k] = true
	}
	return out, nil
}

func enqueuePostChanged(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO outbox (aggregate_type, aggregate_id, event_type) VALUES ($1, $2, $3)`,
		AggregatePost, id.String(), EventPostChanged)
	return err
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
