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

// ForEachPostID streams every post id (including deleted) in batches. Used by
// the full reindex.
func (s *Store) ForEachPostID(ctx context.Context, batch int, fn func(uuid.UUID) error) error {
	var after uuid.UUID // zero UUID sorts first
	for {
		rows, err := s.pool.Query(ctx, `SELECT id FROM posts WHERE id > $1 ORDER BY id LIMIT $2`, after, batch)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		if err != nil {
			return err
		}
		for _, id := range ids {
			if err := fn(id); err != nil {
				return err
			}
		}
		if len(ids) < batch {
			return nil
		}
		after = ids[len(ids)-1]
	}
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
