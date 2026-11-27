package store

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Read/write splitting with read-your-writes.
//
// Reads that tolerate a little staleness (browsing, profiles, like counts,
// timeline hydration) go to a streaming read replica, which takes load off
// the primary. Replication is asynchronous, so a replica can be behind: a
// user who just posted and reloads could get a replica that has not applied
// the post yet ("my post disappeared").
//
// Fix: consistency tokens. After a write, the API returns the primary's WAL
// position (LSN) in X-Consistency-Token; the client sends it back on later
// reads. A read with a token goes to the replica only if the replica has
// replayed at least that far, otherwise to the primary. Reads without a
// token, the vast majority, always use the replica. This is the LSN form of
// causal-consistency tokens (Aurora, MongoDB cluster time, MySQL GTIDs).

type ctxKey int

const (
	tokenKey ctxKey = iota
	primaryKey
)

// WithConsistencyToken attaches the minimum LSN a read must observe.
func WithConsistencyToken(ctx context.Context, token string) context.Context {
	lsn, err := pglogrepl.ParseLSN(token)
	if err != nil || lsn == 0 {
		return ctx
	}
	return context.WithValue(ctx, tokenKey, lsn)
}

// ForcePrimary marks reads that must see the latest committed state, such
// as filling a cache: a cache built from a lagging replica stays wrong after
// the replica catches up.
func ForcePrimary(ctx context.Context) context.Context {
	return context.WithValue(ctx, primaryKey, true)
}

// WithReplica enables read routing. onRoute (optional) observes each routing
// decision: "replica", "primary_token" (replica behind the client's token),
// "primary_forced", "primary_error".
func (s *Store) WithReplica(replica *pgxpool.Pool, onRoute func(target string)) *Store {
	s.replica = replica
	s.onRoute = onRoute
	return s
}

func (s *Store) reader(ctx context.Context) *pgxpool.Pool {
	if s.replica == nil {
		return s.pool
	}
	if forced, _ := ctx.Value(primaryKey).(bool); forced {
		s.route("primary_forced")
		return s.pool
	}
	min, ok := ctx.Value(tokenKey).(pglogrepl.LSN)
	if !ok {
		s.route("replica")
		return s.replica
	}
	replayed, err := s.replayLSN(ctx)
	if err != nil {
		s.route("primary_error")
		return s.pool
	}
	if replayed >= min {
		s.route("replica")
		return s.replica
	}
	s.route("primary_token")
	return s.pool
}

func (s *Store) route(target string) {
	if s.onRoute != nil {
		s.onRoute(target)
	}
}

// replayLSN returns how far the replica has applied WAL, cached for a few
// milliseconds so a burst of token reads costs one query, not one each.
func (s *Store) replayLSN(ctx context.Context) (pglogrepl.LSN, error) {
	s.lsnMu.Lock()
	defer s.lsnMu.Unlock()
	if time.Since(s.lsnAt) < 20*time.Millisecond {
		return s.lsnVal, nil
	}
	var text string
	if err := s.replica.QueryRow(ctx, `SELECT COALESCE(pg_last_wal_replay_lsn(), '0/0')::text`).Scan(&text); err != nil {
		return 0, err
	}
	lsn, err := pglogrepl.ParseLSN(text)
	if err != nil {
		return 0, err
	}
	s.lsnVal, s.lsnAt = lsn, time.Now()
	return lsn, nil
}

// WriteToken returns the primary's current WAL position. Called right after
// a write commits, it is at or past that write's commit LSN.
func (s *Store) WriteToken(ctx context.Context) (string, error) {
	var text string
	err := s.pool.QueryRow(ctx, `SELECT pg_current_wal_lsn()::text`).Scan(&text)
	return text, err
}

// ReplicaLag reports how far the replica is behind the primary, in bytes of
// WAL and in seconds since the last replayed transaction. ok is false when
// no replica is configured.
func (s *Store) ReplicaLag(ctx context.Context) (bytes, seconds float64, ok bool, err error) {
	if s.replica == nil {
		return 0, 0, false, nil
	}
	var primary string
	if err := s.pool.QueryRow(ctx, `SELECT pg_current_wal_lsn()::text`).Scan(&primary); err != nil {
		return 0, 0, true, err
	}
	var replayed string
	if err := s.replica.QueryRow(ctx, `SELECT COALESCE(pg_last_wal_replay_lsn(), '0/0')::text,
		COALESCE(EXTRACT(EPOCH FROM now() - pg_last_xact_replay_timestamp()), 0)::float8`).
		Scan(&replayed, &seconds); err != nil {
		return 0, 0, true, err
	}
	p, _ := pglogrepl.ParseLSN(primary)
	r, _ := pglogrepl.ParseLSN(replayed)
	if p > r {
		bytes = float64(p - r)
	}
	return bytes, seconds, true, nil
}

type lsnCache struct {
	lsnMu  sync.Mutex
	lsnVal pglogrepl.LSN
	lsnAt  time.Time
}
