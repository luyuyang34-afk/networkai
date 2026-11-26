// Package timeline builds the home timeline ("posts from people I follow")
// with a hybrid fan-out:
//
//   - fan-out on write (push): when a regular user posts, the post id is
//     appended to each follower's timeline in Redis. Reading a timeline is
//     then one sorted-set range query.
//   - fan-out on read (pull): posts by high-follower accounts ("celebrities",
//     follower_count >= CelebrityThreshold) are NOT pushed. Followers pull
//     them from PostgreSQL at read time and merge them in.
//
// Why hybrid: pushing a celebrity's post to 5 million followers is 5 million
// writes per post. Pulling every followee's posts on every read is slow for
// users who follow thousands of accounts. Each extreme fails for one kind of
// user; the hybrid gives each case the cheap path. This is the design
// described publicly for large social timelines (e.g. Twitter's home
// timeline).
//
// Redis is a cache here, not a source of truth: a missing timeline (Redis
// restart, eviction, a user who just followed someone) is rebuilt from
// PostgreSQL on first read.
package timeline

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"networkai/internal/outbox"
	"networkai/internal/store"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

type Store interface {
	GetPostForIndex(ctx context.Context, id uuid.UUID) (store.Post, error)
	FollowerCount(ctx context.Context, userID int64) (int64, error)
	FollowerIDsAfter(ctx context.Context, followeeID, after int64, limit int) ([]int64, error)
	Followees(ctx context.Context, userID int64) ([]store.Followee, error)
	RecentPostsByAuthors(ctx context.Context, authorIDs []int64, before time.Time, limit int) ([]store.TimelineEntry, error)
	GetPostsByIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]store.Post, error)
}

type Service struct {
	Redis *redis.Client
	Store Store
	// CelebrityThreshold: authors with at least this many followers are
	// pulled at read time instead of pushed. 0 disables pull entirely
	// (pure fan-out on write), which exists to measure the baseline.
	CelebrityThreshold int64
	// Cap is how many entries each timeline keeps (older ones are trimmed).
	Cap int64
	// BatchSize is how many followers are written per Redis pipeline.
	BatchSize int

	// Prefix namespaces Redis keys (tests use a random one per test).
	Prefix string

	// Stats is optional observability.
	OnFanout func(followers int, d time.Duration)
}

const (
	defaultCap   = 800
	defaultBatch = 1000
	// builtMarker keeps an empty timeline's key alive so "key exists" means
	// "already built". It has score 0, so range reads that stop at "(0"
	// never return it.
	builtMarker = "__built__"
	// Timelines of users who stop reading expire; only active users pay
	// for a cached timeline (and only they receive fan-out writes).
	ttl = 7 * 24 * time.Hour
)

// pushScript appends a post to a timeline only if the timeline exists, then
// trims it, atomically. Fan-out therefore never creates timelines for
// inactive users; a missing timeline is rebuilt from PostgreSQL on read.
var pushScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 0 then return 0 end
redis.call('ZADD', KEYS[1], ARGV[1], ARGV[2])
redis.call('ZREMRANGEBYRANK', KEYS[1], 0, -(tonumber(ARGV[3]) + 1))
return 1`)

func (s *Service) key(userID int64) string {
	return s.Prefix + "tl:" + strconv.FormatInt(userID, 10)
}

func (s *Service) cap() int64 {
	if s.Cap > 0 {
		return s.Cap
	}
	return defaultCap
}

func (s *Service) isCelebrity(followers int64) bool {
	return s.CelebrityThreshold > 0 && followers >= s.CelebrityThreshold
}

// ---------- write path: fan-out ----------

// HandlePostChanged pushes a new post into followers' timelines, or removes a
// deleted one. It is idempotent (ZADD with the same score, ZREM of a missing
// member), so outbox replays are harmless.
func (s *Service) HandlePostChanged(ctx context.Context, postID uuid.UUID) error {
	p, err := s.Store.GetPostForIndex(ctx, postID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if p.DeletedAt == nil && p.Version > 1 {
		return nil // an edit: timelines hold ids, nothing to change
	}
	followers, err := s.Store.FollowerCount(ctx, p.AuthorID)
	if err != nil {
		return err
	}
	if p.DeletedAt == nil && s.isCelebrity(followers) {
		return nil // pulled at read time instead
	}
	if err := pushScript.Load(ctx, s.Redis).Err(); err != nil {
		return fmt.Errorf("redis: %w", err)
	}
	start := time.Now()
	member := p.ID.String()
	score := p.CreatedAt.UnixMilli()
	n, err := s.forEachFollowerBatch(ctx, p.AuthorID, func(pipe redis.Pipeliner, ids []int64) {
		for _, f := range ids {
			if p.DeletedAt != nil {
				pipe.ZRem(ctx, s.key(f), member)
				continue
			}
			pipe.EvalSha(ctx, pushScript.Hash(), []string{s.key(f)}, score, member, s.cap())
		}
	})
	if s.OnFanout != nil && p.DeletedAt == nil {
		s.OnFanout(n, time.Since(start))
	}
	return err
}

func (s *Service) forEachFollowerBatch(ctx context.Context, authorID int64, fn func(redis.Pipeliner, []int64)) (int, error) {
	batch := s.BatchSize
	if batch <= 0 {
		batch = defaultBatch
	}
	total := 0
	var after int64
	for {
		ids, err := s.Store.FollowerIDsAfter(ctx, authorID, after, batch)
		if err != nil {
			return total, err
		}
		if len(ids) == 0 {
			return total, nil
		}
		pipe := s.Redis.Pipeline()
		fn(pipe, ids)
		if _, err := pipe.Exec(ctx); err != nil {
			return total, fmt.Errorf("fan-out pipeline: %w", err)
		}
		total += len(ids)
		after = ids[len(ids)-1]
	}
}

// ---------- read path ----------

type Page struct {
	Posts      []store.Post
	NextCursor *time.Time
	Pulled     int // entries that came from the pull (celebrity) path
	Rebuilt    bool
}

// Read returns up to limit posts older than `before` (zero = now) for userID.
func (s *Service) Read(ctx context.Context, userID int64, before time.Time, limit int) (Page, error) {
	if before.IsZero() {
		before = time.Now().Add(time.Second)
	}
	followees, err := s.Store.Followees(ctx, userID)
	if err != nil {
		return Page{}, err
	}
	var pushed, pulled []int64
	for _, f := range followees {
		if s.isCelebrity(f.FollowerCount) {
			pulled = append(pulled, f.ID)
		} else {
			pushed = append(pushed, f.ID)
		}
	}

	var page Page
	k := s.key(userID)
	exists, err := s.Redis.Exists(ctx, k).Result()
	if err != nil {
		return Page{}, fmt.Errorf("redis: %w", err)
	}
	if exists == 0 {
		if err := s.rebuild(ctx, userID, pushed); err != nil {
			return Page{}, err
		}
		page.Rebuilt = true
	} else {
		s.Redis.Expire(ctx, k, ttl) // reading keeps the timeline alive
	}

	// Push half: one range query on the cached timeline.
	zs, err := s.Redis.ZRangeArgsWithScores(ctx, redis.ZRangeArgs{
		Key: k, Start: "(" + strconv.FormatInt(before.UnixMilli(), 10), Stop: "(0",
		ByScore: true, Rev: true, Count: int64(limit), Offset: 0,
	}).Result()
	if err != nil {
		return Page{}, fmt.Errorf("redis: %w", err)
	}
	entries := make([]store.TimelineEntry, 0, len(zs)+limit)
	for _, z := range zs {
		id, err := uuid.Parse(z.Member.(string))
		if err != nil {
			continue
		}
		entries = append(entries, store.TimelineEntry{ID: id, CreatedAt: time.UnixMilli(int64(z.Score))})
	}

	// Pull half: recent posts by celebrities, straight from PostgreSQL.
	if len(pulled) > 0 {
		ce, err := s.Store.RecentPostsByAuthors(ctx, pulled, before, limit)
		if err != nil {
			return Page{}, err
		}
		page.Pulled = len(ce)
		entries = append(entries, ce...)
	}

	// Merge newest first, dedupe (an author may have crossed the threshold
	// after some of their posts were pushed), take one page.
	sort.Slice(entries, func(i, j int) bool {
		if !entries[i].CreatedAt.Equal(entries[j].CreatedAt) {
			return entries[i].CreatedAt.After(entries[j].CreatedAt)
		}
		return entries[i].ID.String() > entries[j].ID.String()
	})
	seen := map[uuid.UUID]bool{}
	var ids []uuid.UUID
	var last time.Time
	for _, e := range entries {
		if seen[e.ID] || len(ids) == limit {
			continue
		}
		seen[e.ID] = true
		ids = append(ids, e.ID)
		last = e.CreatedAt
	}
	posts, err := s.Store.GetPostsByIDs(ctx, ids)
	if err != nil {
		return Page{}, err
	}
	for _, id := range ids {
		if p, ok := posts[id]; ok { // deleted posts drop out here
			page.Posts = append(page.Posts, p)
		}
	}
	if len(ids) == limit {
		page.NextCursor = &last
	}
	return page, nil
}

// rebuild fills a missing timeline from PostgreSQL with the most recent posts
// of pushed (non-celebrity) followees.
func (s *Service) rebuild(ctx context.Context, userID int64, pushed []int64) error {
	// Fill the cache from the primary, never a replica: a post committed but
	// not yet replayed would be missing, and its fan-out already skipped this
	// (then nonexistent) timeline, so the cache would stay wrong for days.
	entries, err := s.Store.RecentPostsByAuthors(store.ForcePrimary(ctx), pushed, time.Now().Add(time.Second), int(s.cap()))
	if err != nil {
		return err
	}
	k := s.key(userID)
	pipe := s.Redis.TxPipeline()
	pipe.Del(ctx, k)
	pipe.ZAdd(ctx, k, redis.Z{Score: 0, Member: builtMarker})
	for _, e := range entries {
		pipe.ZAdd(ctx, k, redis.Z{Score: float64(e.CreatedAt.UnixMilli()), Member: e.ID.String()})
	}
	pipe.Expire(ctx, k, ttl)
	_, err = pipe.Exec(ctx)
	return err
}

// Invalidate drops a user's cached timeline; it is rebuilt on next read.
// Called when the follow graph changes.
func (s *Service) Invalidate(ctx context.Context, userID int64) error {
	return s.Redis.Del(ctx, s.key(userID)).Err()
}

// Handle is an outbox.Handler for post events.
func (s *Service) Handle(ctx context.Context, ev outbox.Event) error {
	if ev.AggregateType != store.AggregatePost {
		return nil
	}
	id, err := uuid.Parse(ev.AggregateID)
	if err != nil {
		return fmt.Errorf("%w: bad post id %q", outbox.ErrPermanent, ev.AggregateID)
	}
	if err := s.HandlePostChanged(ctx, id); err != nil {
		if isRedisDown(err) {
			return fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		return err
	}
	return nil
}

// ErrUnavailable marks Redis connectivity failures, so the outbox worker can
// pause instead of spending per-event retries (ADR 0003).
var ErrUnavailable = errors.New("timeline store unavailable")

func isRedisDown(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) || errors.Is(err, syscall.ECONNREFUSED) ||
		strings.Contains(err.Error(), "connection refused") || strings.Contains(err.Error(), "i/o timeout")
}
