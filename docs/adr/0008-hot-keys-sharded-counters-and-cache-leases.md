# ADR 0008: Hot keys — sharded like counters and a lease-protected post cache

- Status: accepted
- Date: 2026-11-26

## Context

Traffic in a social product concentrates on a few items. A viral post gets
thousands of likes per second and is read by clients on every API instance.
Measured on one post ([hotkey-experiment.md](../hotkey-experiment.md)):

- a single counter row serializes likes on its row lock: 932 likes/s, P99
  336 ms, versus 6600/s without a counter;
- without a counter, the like count is a `COUNT(*)` that grows with the
  post's likes;
- plain cache-aside sends every concurrent reader to the database on each
  expiry or invalidation (1192 loads/s for about 4 changes/s), and 29.5% of
  reads returned a stale version because of stale sets.

## Decision

1. **Sharded like counters.**
   - `post_like_counts(post_id, shard, n)` has 16 shards per post.
   - A like adds +1 to a random shard in the same transaction as the
     `likes` row, so counts stay exact.
   - A read sums the shards.
   - `WithLikeShards(0|1|n)` keeps the alternatives for comparison.
2. **A Redis cache for `GET /posts/{id}`** (`internal/postcache`) with three
   protections, each switchable through `POST_CACHE=off|naive|singleflight|protected`:
   - *singleflight*: one load per key per process;
   - *leases* (Facebook memcache, NSDI 2013):
     - a miss takes `SET NX` on a lease key;
     - only the holder loads, and its fill is a Lua compare-and-set on the
       lease;
     - invalidation deletes the lease, so a racing fill of the old version
       is dropped;
     - non-holders wait up to 300 ms for the fill, then read the database
       without filling;
   - *XFetch* (Vattani et al., VLDB 2015): each hit refreshes in the
     background with probability that rises as expiry approaches, scaled
     by the measured load time.
3. **Invalidation on write, twice.**
   - After an edit or delete commits, the API deletes the key and the
     lease.
   - The `cache-invalidation` consumer group deletes them again from the
     CDC stream, in case the API died before its `DEL`.
   - The TTL (60 s) is a backstop for a failed invalidation, not the
     freshness bound.
   - "Not found" is cached for 5 s, so a deleted viral post does not
     stampede either.
4. **Fills read the primary** (ADR 0007), and Redis failures fall back to
   the database.

## Alternatives considered

| Option | Why not |
|---|---|
| One counter row, updated in the like transaction | Serializes all likes of a post on one lock (measured 7x slower). |
| Count likes in Redis only (`INCR`) | Fast, but the count is no longer derived from the source of truth, and a Redis loss loses it. It could be added later as a cache in front of the shards. |
| Batch +1s in memory and flush | Higher throughput, but a crash loses unflushed likes. Counts would become approximate. |
| Longer TTL instead of invalidation | Fewer misses, but edits and deletes stay visible for the whole TTL. |
| Version check on fill (only write if newer) | Prevents stale overwrites but not the stampede; leases do both. |
| Request coalescing in a proxy layer (e.g. a shared cache tier) | The right step with many services; for one service, leases in Redis give the same cross-instance guarantee. |

## Consequences

- Database loads for a hot post are one per change, cluster-wide: 4 loads/s
  with 32 instances, versus 128 with singleflight alone. Stale reads were
  0%, versus 29.5% for plain cache-aside.
- Readers that miss during a fill wait for it: P99 29 → 32 ms versus
  singleflight alone.
- If a lease holder dies, fills for that key pause for the lease TTL (2 s).
  Readers are still served, from the database.
- Like counts are exact; liking a hot post is still slower than an
  ordinary one, because 64 writers collide on 16 rows. More shards per hot
  post is possible later, chosen adaptively.
- Only single-post reads are cached. List pages and timeline hydration
  still read the database (or replica). A multi-get cache for hydration is
  future work.
- An invalidation from the API is lost if the process dies between the
  commit and the `DEL`. The `cache-invalidation` consumer group closes that
  gap: it invalidates every post event from the CDC stream, as Facebook's
  mcsqueal does from the MySQL commit log. Invalidations are idempotent,
  so the double delete costs one Redis call.
