package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ---------- follow graph ----------

var ErrSelfFollow = errors.New("cannot follow yourself")

type Profile struct {
	ID             int64
	Username       string
	FollowerCount  int64
	FollowingCount int64
	FollowedByMe   bool
}

// Follow makes followerID follow the user named followee. Following twice is
// a no-op. The follower counter changes in the same transaction as the edge,
// so it can never drift from the follows table.
func (s *Store) Follow(ctx context.Context, followerID int64, followee string) (int64, error) {
	var followeeID int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE username = $1`, followee).Scan(&followeeID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if followeeID == followerID {
			return ErrSelfFollow
		}
		tag, err := tx.Exec(ctx,
			`INSERT INTO follows (follower_id, followee_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
			followerID, followeeID)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE users SET follower_count = follower_count + 1 WHERE id = $1`, followeeID)
		return err
	})
	return followeeID, err
}

func (s *Store) Unfollow(ctx context.Context, followerID int64, followee string) (int64, error) {
	var followeeID int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE username = $1`, followee).Scan(&followeeID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM follows WHERE follower_id = $1 AND followee_id = $2`, followerID, followeeID)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE users SET follower_count = follower_count - 1 WHERE id = $1`, followeeID)
		return err
	})
	return followeeID, err
}

func (s *Store) GetProfile(ctx context.Context, username string, viewerID int64) (Profile, error) {
	var p Profile
	err := s.reader(ctx).QueryRow(ctx, `
		SELECT u.id, u.username, u.follower_count,
		       (SELECT count(*) FROM follows f WHERE f.follower_id = u.id),
		       EXISTS (SELECT 1 FROM follows f WHERE f.follower_id = $2 AND f.followee_id = u.id)
		FROM users u WHERE u.username = $1`, username, viewerID).
		Scan(&p.ID, &p.Username, &p.FollowerCount, &p.FollowingCount, &p.FollowedByMe)
	if errors.Is(err, pgx.ErrNoRows) {
		return Profile{}, ErrNotFound
	}
	return p, err
}

func (s *Store) FollowerCount(ctx context.Context, userID int64) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `SELECT follower_count FROM users WHERE id = $1`, userID).Scan(&n)
	return n, err
}

// FollowerIDsAfter pages through a user's followers in id order (keyset
// pagination, so a 5-million-follower list costs the same per page).
func (s *Store) FollowerIDsAfter(ctx context.Context, followeeID, after int64, limit int) ([]int64, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT follower_id FROM follows WHERE followee_id = $1 AND follower_id > $2
		ORDER BY follower_id LIMIT $3`, followeeID, after, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

type Followee struct {
	ID            int64
	FollowerCount int64
}

func (s *Store) Followees(ctx context.Context, userID int64) ([]Followee, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT u.id, u.follower_count FROM follows f JOIN users u ON u.id = f.followee_id
		WHERE f.follower_id = $1`, userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Followee, error) {
		var f Followee
		err := r.Scan(&f.ID, &f.FollowerCount)
		return f, err
	})
}

// TimelineEntry is the minimal (id, time) pair a timeline is made of.
type TimelineEntry struct {
	ID        uuid.UUID
	CreatedAt time.Time
}

// RecentPostsByAuthors returns live posts by any of authorIDs created before
// `before`, newest first. This is the "pull" half of the hybrid timeline
// (posts by high-follower authors) and the rebuild path for a cold timeline.
func (s *Store) RecentPostsByAuthors(ctx context.Context, authorIDs []int64, before time.Time, limit int) ([]TimelineEntry, error) {
	if len(authorIDs) == 0 {
		return nil, nil
	}
	rows, err := s.reader(ctx).Query(ctx, `
		SELECT id, created_at FROM posts
		WHERE author_id = ANY($1) AND deleted_at IS NULL AND created_at < $2
		ORDER BY created_at DESC, id DESC LIMIT $3`, authorIDs, before, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (TimelineEntry, error) {
		var e TimelineEntry
		err := r.Scan(&e.ID, &e.CreatedAt)
		return e, err
	})
}

// GetPostsByIDs loads live posts by id in one query.
func (s *Store) GetPostsByIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]Post, error) {
	out := make(map[uuid.UUID]Post, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.reader(ctx).Query(ctx, `SELECT `+postColumns+` FROM posts p JOIN users u ON u.id = p.author_id
		WHERE p.id = ANY($1) AND p.deleted_at IS NULL`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		p, err := scanPost(rows)
		if err != nil {
			return nil, err
		}
		out[p.ID] = p
	}
	return out, rows.Err()
}
