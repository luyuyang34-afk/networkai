# Experiment: hot keys (one viral post)

Two questions:

1. Writes: what happens when thousands of users like the same post at once?
2. Reads: what happens when hundreds of clients on many API instances read
   the same post, and its cache entry expires or gets invalidated?

All runs: one machine with 4 vCPUs and 15 GB RAM. PostgreSQL 16 and Redis 7
run in Docker; the load generator runs on the same machine. Absolute numbers
are small-machine numbers, so compare the ratios.

## 1. Writes: likes on one post

`go run ./tools/experiments/hotkey -users 50000 -workers 64`

50,000 distinct users like one post, 64 at a time. Each like is a
transaction that inserts the `likes` row and, with a counter, bumps it.

| Layout | Likes/s | Like P50 | Like P99 | Count read P50 (50k likes) |
|---|---|---|---|---|
| No counter (`COUNT(*)` on read) | 6600 | 8.1 ms | 31.4 ms | 5.0 ms |
| 1 counter row per post | **932** | 45.4 ms | **335.7 ms** | 0.2 ms |
| 16 counter shards | 3410 | 13.5 ms | 99.0 ms | 0.2 ms |

What it shows:

- **The obvious fix makes writes 7x slower.** A single counter row makes
  every like of the post wait for the same row lock, held until the
  previous like's transaction commits. 64 concurrent likers become a queue.
- **Not counting is fast to write but slow to read.** `COUNT(*)` reads every
  like row, so it grows with the post's likes: 5 ms at 50k likes, and the
  count is read on every view.
- **Sharding the counter recovers most of the write throughput** (3.7x the
  single row), and reads stay a sum of at most 16 small rows. The cost is
  more rows per post and a slightly more complex read. This is the classic
  "sharded counter" (Google App Engine docs; also how Instagram-scale like
  counts are discussed publicly).
- The remaining gap to "no counter" is the counter update itself: 64
  writers on 16 rows still collide. More shards or batching would close it.
  Batching means buffering +1s in memory or Redis and flushing them. It
  trades exactness on a crash for throughput, and this design keeps counts
  exact.

## 2. Reads: cache stampede on one post

`go run ./tools/experiments/hotpost -instances 32 -readers 12 -load-cost 20ms`

- 32 "API instances" share one Redis. Each instance is a separate cache
  object, so it has its own singleflight group, like separate processes.
  Each instance runs 12 readers in a loop: 384 concurrent readers in total.
- The TTL is 1 s, to force expiries. A writer edits the post every 250 ms and
  invalidates the cache, as the API does after an edit.
- `-load-cost 20ms` adds 20 ms of database time to each load. It stands in
  for a heavier page build (joins, hydration); the bare primary-key lookup
  takes about 0.3 ms.
- A **stale read** is a read that returned an older version than one whose
  write *and* invalidation had already finished before the read started.

| Mode | Reads/s | DB loads/s | Read P50 | Read P99 | Stale reads |
|---|---|---|---|---|---|
| No cache | 1551 | 1551 | 239 ms | 345 ms | 0 |
| Cache-aside, no protection | 12258 | 1192 | 5.5 ms | 282 ms | **29.5%** |
| + singleflight | 62800 | 128 | 5.3 ms | 29 ms | 0 |
| + singleflight + leases | 60350 | **4** | 5.4 ms | 32 ms | 0 |
| + singleflight + leases + XFetch | 58956 | 4 | 5.5 ms | 33 ms | 0 |

Same run with 8 instances x 50 readers: singleflight made 32 loads/s, and
leases still made 4. With the cheap 0.3 ms load and 32 instances, no cache
gave P50 14.8 ms and DB 25k queries/s. The protected cache gave 5 loads/s.

Expiry only, with no edits (`-edit-every 0`, 10 s, 384 readers):

| Mode | DB loads | Reads that hit a miss |
|---|---|---|
| Cache-aside, no protection | 3072 | 3072 |
| + singleflight | 320 | 3840 |
| + singleflight + leases | 10 | 3840 |
| + singleflight + leases + XFetch | 16 | **384** |

What it shows:

1. **Plain cache-aside does not survive a hot key.**
   - Every expiry or invalidation sends every concurrent reader to the
     database at once: 1192 loads/s from what should be about 4 changes
     per second.
   - Those loads queue on the 32-connection pool, so P99 is 282 ms.
2. **It also serves stale data.** 29.5% of reads returned a version older
   than one whose write had already finished. A reader that loaded the old
   row writes it into the cache *after* the writer's invalidation. It then
   stays there until the next invalidation or the TTL. This is the
   stale-set race in Facebook's memcache paper (NSDI 2013).
3. **Singleflight fixes the stampede within a process, not across
   processes.**
   - Loads scale with the instance count: 32/s with 8 instances and 128/s
     with 32.
   - It narrows the stale-set window but does not close it. The unit test
     `TestLeasesPreventStaleSet` reproduces the race deterministically with
     singleflight on.
4. **Leases make the database cost independent of the instance count.**
   - One load per change, cluster-wide: 4 loads/s for 4 edits/s.
   - The stale-set race is closed: invalidation deletes the lease, so a
     fill of the old version is rejected.
   - The cost is a slightly higher P99 (29 → 32 ms): callers without the
     lease poll for the holder's fill.
5. **XFetch removes expiry misses.**
   - Without it, every reader sees a miss each time the TTL runs out (3840
     reads in 10 s).
   - With it, a reader refreshes the entry shortly before expiry, so only
     the 384 cold-start reads missed.
   - The cost is a few extra loads (16 vs 10).
   - XFetch does nothing for invalidation-driven misses, so it shows no
     difference in the edit runs.

## Bugs and pitfalls found

- **The single-row counter looked fine in unit tests** and collapsed only
  under concurrency on one post. Correctness tests and contention tests are
  different tests.
- **"0 stale reads" with singleflight is not a proof.** It held in the load
  test only because misses happen right after an invalidation and the next
  edit comes 250 ms later. A deterministic test of the interleaving shows
  the race.
- **Cache fills must read the primary.** A fill from a lagging replica
  caches an old version with a fresh TTL (ADR 0007), so the cache uses
  `store.ForcePrimary`.

## Invalidation from the CDC stream

This checks the second invalidation path. A post was edited directly in
PostgreSQL, with an outbox row and without the API, so the API's `DEL` never
ran:

- with `consumer-cache` stopped, `GET /posts/{id}` still served the old
  version 2 s later, and would until the TTL;
- after `consumer-cache` started, it was fresh about 200 ms later.
