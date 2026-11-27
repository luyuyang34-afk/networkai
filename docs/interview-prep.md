# NetworkAI interview prep

Three parts:

1. **The five design questions** an interviewer is most likely to ask about
   this project, each with a short spoken answer, the design behind it, the
   numbers, and the follow-ups.
2. **The five hardest problems** solved while building it, told as stories:
   situation, what went wrong, how it was found, the fix, and the proof.
3. **"What if users grow 100x?"**, the follow-up that closes most
   system design rounds: how to structure the answer, what breaks first,
   and the sub-questions that come after it.

Every number here comes from an experiment in this repository and has a
measurement condition. All experiments ran on one 4-vCPU machine; multiple
nodes and regions were simulated. Say so when quoting them.

---

## Part 1: The five design questions

### 1. "Design the home timeline. Push or pull? What about celebrities?"

**Short answer.** Hybrid.
- Posts by regular users are pushed into each follower's timeline, a Redis
  sorted set, when they are written.
- Posts by accounts with more than 10,000 followers are not pushed.
  Followers pull them at read time and merge them in.
- Each kind of user gets the cheap path: readers do one range query, and a
  celebrity post doesn't trigger hundreds of thousands of writes.

**Design points**
- Timelines are a cache: capped at 800 entries, and they expire after 7
  days without reads. They can be rebuilt from PostgreSQL.
- Only users who have a timeline in Redis receive pushes. Inactive users
  cost nothing.
- Fan-out runs asynchronously, as its own Kafka consumer group, so it never
  slows the write path.

**Numbers.** With pure push, a celebrity with 200k followers made other
users' posts wait **1.28 s** behind its fan-out. With hybrid, **14 ms**.
Read latency was unchanged.

**Likely follow-ups**
- *How did you pick 10,000?*
  - It balances push cost (one write per follower) against pull cost (one
    query per celebrity followed, on every read).
  - It should be tuned from the measured follower distribution.
- *What if Redis loses the data?*
  - The next read rebuilds the timeline from PostgreSQL, reading the
    primary, not a replica.
  - A rebuild from a lagging replica would miss a post whose fan-out already
    skipped the missing timeline, and the timeline would stay wrong for
    days.
- *What if someone follows 5,000 accounts?*
  - Most of their timeline is already pushed. The pull only covers the few
    celebrities they follow.

### 2. "How do search, timelines and caches stay in sync with the database without losing writes?"

**Short answer.**
- PostgreSQL is the only source of truth. The request never writes to two
  systems.
- The post and an outbox row are written in one transaction.
- A relay reads committed changes from the write-ahead log (logical
  decoding) and publishes them to Kafka.
- Each downstream job is an independent consumer group: search indexing,
  timeline fan-out, cache invalidation, stats.

**Design points**
- Delivery is **at-least-once**: offsets are committed after processing,
  and the WAL position is confirmed only after Kafka acknowledges the
  batch. Duplicates are expected, so every consumer is **idempotent**:
  - search uses external versions;
  - stats use a dedupe table;
  - invalidation is naturally repeatable.
- The partition key is the post id, so events for one post stay in order.
- Poison messages go to a per-group dead-letter topic, and the partition
  keeps moving.
- A group that is down only delays itself. A new group can replay history
  from the start of the topic.

**Numbers**
- An Elasticsearch outage of 150 s lost **0 writes**.
- With Redis down, a post was searchable in **0.73 s**, versus over 60 s
  when one worker did every job.

**Likely follow-ups**
- *Why not write to Kafka directly from the API?*
  - That is still a dual write. A crash between the commit and the publish
    loses the event.
- *Why not exactly-once?*
  - It needs distributed transactions across PostgreSQL, Kafka and each
    sink. At-least-once plus idempotent consumers gives the same result,
    more cheaply.
- *What is the operational risk of CDC?*
  - A stopped relay makes PostgreSQL retain WAL for its replication slot
    until the disk fills. There is an alert on slot lag for exactly this.

### 3. "You added read replicas. How does a user see their own post right after posting?"

**Short answer.** Consistency tokens.
- After a write, the API returns the primary's WAL position (LSN) in a
  header. The client sends it back on later reads.
- A read goes to the replica only if the replica has replayed past that
  position; otherwise it goes to the primary.
- Reads without a token, which are most of them, always use the replica.

**Numbers.** With no artificial lag at all, **31%** of "post, then
immediately read my posts" requests missed the new post. With tokens,
**0%**, and anonymous browsing was still served **100%** by the replica.

**Likely follow-ups**
- *Why not read from the primary for N seconds after a write?*
  - N is a guess. Too small and the bug returns under load; too large and
    the primary loses the offload.
  - The token adapts to the actual lag.
- *What about another device?*
  - The token lives on the device, so a second device is not covered. That
    trade-off was accepted.
- *Where must you never read from a replica?*
  - Cache fills. A cache filled from a lagging replica stays wrong after
    the replica catches up.
- *How did you fit the token into a cross-region write?*
  - The token must be read after the commit, or it can point before the
    commit record.
  - It is pipelined behind the write with its own Sync: still one network
    round trip, 429 → 73 ms at a 70 ms RTT.

### 4. "A post goes viral. What breaks, and how do you fix it?"

**Short answer.** Three things break, and each has a known fix.

1. **Likes contend on one row lock.** The counter is split into 16 shards
   per post: increment a random shard, sum on read.
   - **932 → 3,410 likes/s** on one post, and counts stay exact.
2. **Cache stampede.** When the cached post expires or is invalidated,
   every reader goes to the database at once.
3. **Stale sets.** A slow reader writes the old version back into the cache
   after the writer invalidated it.

For 2 and 3, the post cache has three layers:
- **singleflight**: one database load per key per process;
- **leases**, from Facebook's memcache paper:
  - only the lease holder loads and fills;
  - invalidation deletes the lease, so a racing fill of old data is
    rejected;
  - others wait briefly for the fill;
- **XFetch** (probabilistic early refresh): one reader refreshes shortly
  before expiry, so nobody sees the miss.

**Numbers** (32 API instances, one hot post, edited 4 times a second)
- Database loads: **1,192/s → 4/s**, one per edit, cluster-wide.
- Stale reads: **29.5% → 0%**.
- With XFetch, the only expiry misses were the 384 cold-start reads,
  versus 3,840 without it.

**Likely follow-ups**
- *Isn't singleflight enough?*
  - No. It only works within one process, so loads grow with the instance
    count: 32/s with 8 instances, 128/s with 32.
- *What if the API crashes between the commit and deleting the cache key?*
  - A CDC consumer group deletes it again from the change stream, as
    Facebook's mcsqueal does. Deleting twice is harmless.
- *What if the lease holder dies?*
  - The lease expires after 2 s. In the meantime, other readers are served
    from the database without filling the cache.

### 5. "The data outgrows one database. How do you shard it, and how do you add nodes without downtime?"

**Short answer.**
- Shard by author into **64 fixed logical shards**, mapped to physical
  nodes by a directory.
  - "Post by id" and "posts by user" each touch one node; the post id
    carries its shard.
  - Adding nodes moves whole logical shards and never rehashes rows.
- To move a shard online:
  1. copy it in rounds without locks;
  2. lock only that shard for a few milliseconds to copy the final delta;
  3. the source gives up ownership, then the target takes it.

**Design points**
- **Ownership lives on the nodes, not in the routers.** Every write checks
  ownership inside its own transaction. A router with a stale map gets
  "moved", reloads the directory and retries.
- **Source first, then target.** A crash mid-cutover leaves the shard owned
  by nobody, so writes fail until the move is re-run. It never leaves two
  owners.
- Each incremental round starts from the previous round's snapshot xmin,
  so transactions that were still in flight are not skipped (see Part 2,
  #1).

**Numbers.** 2 → 4 nodes under 16 writers and 16 readers:
- **0 of 46,608** acknowledged writes lost, 0 errors;
- writes to the moving shard paused for a median of **46 ms**;
- a naive copy-then-flip **lost 98 posts** and reported no error.

**Likely follow-ups**
- *Why not `hash % N`?*
  - Adding a node would move almost every row.
- *Why not shard by time?*
  - All new writes would land on one shard, a permanent hot spot.
- *How do you query across shards?*
  - Scatter-gather, whose latency is the slowest node's: 6 → 11.5 ms P50
    from 2 to 4 nodes.
  - Global views like search and the global feed belong in derived stores
    such as Elasticsearch, not in the shards.
- *What about a whole region going down?* See Part 2, #4.

---

## Part 2: The five hardest problems

### 1. Moving data between databases without losing a single write

**Situation.** Resharding posts from 2 to 4 PostgreSQL nodes while 16
writers and 16 readers kept running.

**What went wrong**
- The obvious approach, copy the shard and then flip the routing map,
  produced no errors at all. Only checking every acknowledged write
  afterwards showed **98 posts lost**: writes that landed on the old node
  between the copy and the flip.
- Adding a final catch-up copy introduced a subtler bug. If each round
  copies "changes since the highest transaction I saw", it skips a
  transaction that started before the round and committed after it.
  - The load test caught that version only **1 time in 3**.

**Fix**
- A per-shard fence. Lock that shard's ownership row, which waits for
  in-flight writes; copy the last delta; hand over ownership.
- Each round starts from the previous snapshot's **xmin**, the oldest
  transaction still running.
- Ownership is checked on the node inside every write, so routers with a
  stale map are redirected instead of writing to the wrong place.

**Proof**
- **0 of 46,608** writes lost.
- A deterministic test holds a transaction open across the copy rounds. It
  fails every time with the wrong horizon.
- Removing the fenced delta copy loses 14 of 1,926 writes in the
  rebalance test.

**Bonus finding.** After the move, a "newest posts" query on the new nodes
took 150–280 ms instead of 0.2 ms.
- `auto_explain` showed a seq scan plus a sort, estimated at 5 rows: the
  planner still had the empty table's statistics after the bulk copy.
- Fix: `ANALYZE` before cutover. P99 during moves dropped from 640–860 ms to
  89 ms.

**Lesson.** In data migration, the dangerous failures are silent. Verify
every acknowledged write, and turn races into deterministic tests.

### 2. A cache race that the load test said did not exist

**Situation.** Caching a hot post across many API instances while it was
being edited.

**What went wrong**
- Plain cache-aside returned stale data on **29.5%** of reads. A slow
  reader loads version 1, the writer commits version 2 and deletes the key,
  then the reader writes version 1 back. The cache stays wrong until the
  next edit or the TTL.
- With singleflight added, the load test showed **0 stale reads**. That
  looked like a fix, but it was luck: misses happened right after an
  invalidation, and the next edit came 250 ms later, so the window never
  overlapped.

**Fix**
- Leases, from Facebook's memcache paper.
  - A miss takes a lease with `SET NX`. Only the holder fills, using a
    compare-and-set on the lease.
  - Invalidation deletes the lease, so any racing fill is dropped.
- Plus a second invalidation path from CDC, in case the API dies between
  the commit and the delete.

**Proof**
- A deterministic test injects the write in the middle of a load. Without
  leases, the stale version stays cached; with leases, the cache serves
  the new version.
- In the load test: database loads fell from 1,192/s to 4/s, with 0%
  stale.

**Lesson.** "The load test passed" is not evidence that a race is gone.
Write the interleaving down as a test.

### 3. A message consumer that silently dropped records on restart

**Situation.** Moving from a polling worker to CDC with Kafka consumer
groups.

**What went wrong**
- After a consumer restart, a probe post was never indexed.
  - On partitions it had never committed, the consumer started at the
    *end* of the log, like Kafka's default `auto.offset.reset=latest`.
  - Records produced while it was down were skipped.
  - Nothing logged an error.
- Separately, every restart stalled for **60 s**. Shutdown closed the
  client without leaving the group, so the coordinator waited out the old
  member's session before reassigning partitions.

**Fix**
- Durable groups start at the beginning of the log. Replays are safe
  because every handler is idempotent.
- Shutdown allows the rebalance and leaves the group explicitly.

**Proof**
- A regression test: with "latest", **7 of 21** records arrived; now **21
  of 21**.
- Rejoin time went from 60 s to 0.1 s.

**Related.** A rare CI failure, `relay: produce: context canceled`, turned
out to be the relay treating a shutdown during a publish as an error. It
passed 15 random runs. A deterministic test, where the broker never
answers and the test cancels mid-publish, fails every time on the old code.

**Lesson.** At-least-once is only true if restarts are tested. Defaults
that favour "skip to now" are data loss for a pipeline.

### 4. Region failover: async replication produced split brain

**Situation.** Primary in region US, replica in EU over a simulated 70 ms
link, then a network partition between them. US clients could still reach
the US primary, which is the hard case. EU detected the failure and
promoted its replica.

**What went wrong.** With async replication and automatic failover:
- **54,803** writes were accepted by the old US primary *after* EU had
  promoted. Both regions took writes: split brain.
- Every write the old primary accepted after the partition was lost.
- Even 259 writes acknowledged *before* the partition were lost: they were
  still on the wire.

**Fix and trade-off.** Three modes, all measured:

| Mode | Split-brain writes | Acknowledged writes lost | Cost |
|---|---|---|---|
| async | 54,803 | thousands | none |
| async + leader lease (self-fencing) | **0** | those before the fence | outage 2.0 → 5.0 s |
| synchronous replication | **0** | **0** | +72 ms per commit; the cut-off region cannot write at all |

With the lease:
- the primary must renew a lease through a witness, and makes itself
  read-only when it cannot (Patroni does this with etcd);
- EU waits out the lease TTL before promoting.

**Bonus finding.** Turning on synchronous replication at startup hung the
database's own initialisation. Its `CREATE DATABASE` waited for a standby
that did not exist yet. It is now switched on at runtime, once the replica
streams.

**Lesson.** This is the CAP trade-off made concrete. Choose by data type:
feed data uses async with fencing; money would use synchronous commit.

### 5. The personalised feed took search down, and my first hypothesis was wrong

**Situation.** A mixed load test at 1,000 req/s: 45% browse, 20% read,
20% search, 10% write, 5% personalised feed.

**What went wrong**
- Search P50 rose to **4.4 s**, and about 9% of requests failed.
- Elasticsearch had rejected **51,138** searches: its search thread pool
  was full.
- Without the feed, the same load ran with 0 failures.

**Investigation**
1. *Hypothesis:* each feed request runs three searches in parallel and
   fills the pool.
   - I added a bulkhead (at most N concurrent feed requests) and made
     retrieval sequential.
   - Search still collapsed, even with N = 1. **The hypothesis was wrong.**
2. I load-tested the feed alone: 25 feeds/s used **137% of a CPU core** in
   Elasticsearch, about 3x what the queries' reported `took` time
   suggested. kNN uses concurrent segment search, so one query burns
   several cores.
3. Timing each phase showed much of the time went to shipping embeddings
   for 400 candidates as JSON. Candidates now carry only ids, and only the
   final 20 posts are fetched in full.
4. Only **1%** of served items came solely from the most expensive query
   (`more_like_this`). Removing it left the top 20 unchanged for all 40
   users sampled.
5. The index had 28 segments, and kNN walks one graph per segment. Force-
   merging to 1 segment cut feed CPU by about 30%.

**Result.** At the same 1,000 req/s:
- search P50 **4.4 s → 20 ms**;
- rejections **51k → 0**;
- the only failures were feed requests shed on purpose (2.27%).

**Lesson**
- Measure resource cost, not only latency: `took` hid most of the cost.
- Test a hypothesis with a control before building on it.
- Measure a feature's value before paying for it.

---

## Part 3: "What if users grow 100x?"

This is usually the last question of the round. The interviewer wants to
see whether you can find the bottleneck from numbers, not whether you can
name every technology.

Each item below is tagged:
- **[built]**: implemented and measured in this project;
- **[built, isolated]**: implemented and measured as a subsystem, not yet
  wired into the API;
- **[proposed]**: not built; it is the design you would describe.

Never present a [proposed] item as done.

### How to structure the answer (about 2 minutes)

1. **Put numbers on it first.**
   - "100x is roughly 100k → 10M daily active users."
   - Then state the peaks (table below) and say which numbers grow
     faster than users: fan-out, and traffic on viral posts.
2. **Name what breaks first, and why**, from measurements, not guesses.
3. **Fix things in the order they break**, cheapest change with the most
   headroom first. Sharding comes late because it is the most expensive
   change.
4. **Say what stays the same.**
   - PostgreSQL as the source of truth, the outbox/CDC pipeline and
     idempotent consumers do not change at 100x. That is the point of
     the architecture.
5. **End with what you would measure** before committing to the plan.

### The estimate

Assumptions per daily user:
- 0.5 posts;
- 10 timeline loads of 20 posts;
- 20 likes;
- 200 followers on average, power-law distributed;
- peak = 3x average.

| | 100k DAU | 10M DAU (100x) |
|---|---|---|
| Posts written, peak | ~2/s | ~175/s |
| Timeline loads, peak | ~35/s | ~3,500/s |
| Post lookups for hydration, peak | ~700/s | **~70,000/s** |
| Likes, peak | ~70/s | ~7,000/s, concentrated on viral posts |
| Fan-out writes, peak | ~350/s | **~35,000/s** |
| New post text | 50 MB/day | 5 GB/day, ~1.8 TB/year (media separate) |
| Timeline cache, if every user had one | 5 GB | **~500 GB** |

Writes stay modest: 175 posts/s is easy for one PostgreSQL primary. The
growth is in **reads, fan-out and storage**, and in hot spots.

### What breaks first, in order

**1. The personalised "For you" feed.**
- *Why first:* it is measured as the most expensive endpoint by about 10x.
  Under mixed load it served about 22 req/s on one 4-vCPU machine before
  the bulkhead started shedding [built]. It scales with every reader.
- *Fix:*
  - precompute feeds for active users in the background and cache each
    user's candidates for a few minutes [proposed];
  - run vector search on its own Elasticsearch nodes, so it cannot starve
    keyword search [proposed];
  - meanwhile, the bulkhead keeps a feed overload from taking search down
    [built].
- *Also:* give the feed its own, looser SLO, or serve a cached
  non-personalised feed instead of a 503. Otherwise shedding burns the
  API's availability budget [proposed; the problem is measured in
  `slo.md`].

**2. Read volume: about 70k post lookups per second.**
- *Why:* every timeline load hydrates 20 posts.
- *Fix:*
  - add read replicas; consistency tokens keep read-your-writes [built];
  - add a multi-get post cache for hydration, reusing the lease-based
    single-post cache [the single-post cache is built; multi-get is
    proposed];
  - the stateless API scales horizontally behind a load balancer.
- *Watch out:* with several replicas, a user can see older data after
  newer data, when consecutive reads hit replicas at different lag. Fix:
  send the token on every read, or pin a session to one replica (noted in
  ADR 0007).

**3. Fan-out and timeline memory.**
- *Why:* about 35k timeline writes/s at peak, and about 500 GB if every
  user had a cached timeline.
- *Fix:*
  - only active users keep a timeline: a 7-day TTL and a cap of 800
    entries [built];
  - tune the celebrity threshold from the real follower distribution
    [built, configurable];
  - shard Redis by user id with Redis Cluster [proposed];
  - add Kafka partitions, since partitions cap consumer parallelism: 12
    today, scaled to the consumer count [proposed]. The post-id key keeps
    per-post ordering.

**4. One primary's storage and maintenance.**
- *Why:* about 1.8 TB/year of text plus likes and follows. Vacuum, backup
  and restore time grow with one machine's data before its write
  throughput runs out.
- *Fix:* the sharded store, with 64 logical shards by author and online
  moves [built, isolated]. Moving the API onto it means:
  - co-locating likes and outbox rows with their post's shard;
  - one CDC relay per node;
  - replacing the user join with a lookup.

  That is a planned migration, not a flag flip.

**5. Users on several continents.**
- *Why:* every cross-region round trip costs tens of milliseconds.
- *Fix:*
  - local read replicas and caches in each region;
  - one-round-trip writes [built: 429 → 73 ms at 70 ms RTT];
  - route each user's writes to a home region that owns their shard
    [proposed];
  - fenced failover [built in a drill].

**Also mention briefly**
- Media goes to object storage behind a CDN. Storage on GCS is built; the
  CDN is proposed.
- Search: add Elasticsearch shards and replicas. The shard count is
  changed by building a new index and swapping the alias. The blue/green
  rebuild is built; changing the shard count with it was not exercised.
- Operations: per-feature SLOs, burn-rate alerts [built], autoscaling the
  stateless tiers, and load shedding at the edge.

### Sub-questions that usually follow

**"Which one do you do first?"**
- Measure first. Find the per-request cost of each endpoint at the
  current load, then fix what breaks soonest with the cheapest change.
- Here that means feed precomputation and caching, then replicas and the
  hydration cache. Sharding comes last: it is the most expensive change,
  and storage gives warning months ahead.

**"How many machines would you need?"**
- Do not extrapolate from one shared 4-vCPU box: every service and the
  load generator shared those CPUs.
- The method: profile each tier's cost per request, multiply by the peak
  rate, add headroom for failover (N+1 per tier), and load-test the
  plan.
- Single-node figures that are safe to quote: one Redis node handles on
  the order of 100k simple operations per second, so ~3,500 timeline
  reads/s is not the problem; memory is.

**"What happens to consistency at that scale?"**
- The guarantees stay the same: at-least-once with idempotent consumers,
  and read-your-writes per device via tokens.
- What changes: more replicas need monotonic reads (see above), and
  cross-device read-your-writes would need the token stored with the
  user's session, not on the device.

**"A celebrity with 50 million followers posts. Walk me through it."**
1. The write is one row plus one outbox event; there is no fan-out.
2. Followers pull the post at read time. That makes it a hot key, so the
   post cache with leases and XFetch serves it with about one database
   load per change [built].
3. Its likes go to sharded counters [built]. For extreme posts, make the
   shard count adaptive [proposed].

**"What would you not change?"**
- PostgreSQL as the source of truth; the outbox/CDC pipeline; idempotent,
  independent consumers; tokens for read-your-writes.
- Each of these was chosen so that adding capacity means adding nodes
  (replicas, partitions, shards), not redesigning.

**"What is the biggest risk in your plan?"**
- The sharding migration: cross-shard queries and transactions disappear,
  and every query has to be checked.
- The online move itself is proven [built, isolated]: 0 of 46,608 writes
  lost under load. Rewriting the application's queries is the real work.

### Red lines for this question

- Do not say the system "handles 10M users". It was never run at that
  scale.
- Do not extrapolate machine counts linearly from the single-machine
  benchmark.
- Mark proposed items as proposed. "I built X and measured Y; at 100x I
  would add Z" is much stronger than claiming Z exists.
