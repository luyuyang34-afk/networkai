# NetworkAI mock interview

Two parts:

1. **Five questions that show he understands the whole project.** Each one
   requires connecting several parts of the system.
2. **A mock interview by resume bullet:** one main question and two
   follow-ups per bullet, the way an interviewer would drill in.

Each question lists what a good answer covers. **Trap** and **Bonus** notes
mark what separates an average answer from a strong one. Fuller answers are
in [interview-prep.md](interview-prep.md) (Part 1: design questions; Part 2:
stories).

---

## Part 1: Five questions that show he understands the whole project

None of these asks about a single feature. If he can answer all five, he
understands the system as a whole, not just a set of memorized facts.

### Q1. "Walk me through everything that happens from the moment a user creates a post until it shows up in a follower's timeline, in search results, and as a fresh copy in the cache."

A good answer covers:
- The post and its outbox row are written in one transaction, which is one
  network round trip even across regions.
- A consistency token is returned to the client.
- The relay reads the commit from the WAL and publishes it to Kafka,
  partitioned by post id.
- Three consumer groups handle it independently:
  - push it into followers' timelines, skipping celebrities;
  - index it in Elasticsearch with an external version;
  - invalidate the cache.
- If one consumer group goes down, only that group falls behind.

### Q2. "Which store is the source of truth? If Elasticsearch, Redis or Kafka lost all of its data tomorrow, what would you do in each case?"

A good answer covers:
- Only PostgreSQL is the source of truth. Everything else is derived data.
- Elasticsearch lost: reindex, or do a blue/green rebuild.
- Redis lost: each timeline is rebuilt from the **primary** on its next
  read, and the cache refills on its own.
- Kafka lost: consumers may have missed events, so reindex from PostgreSQL
  to fill the gaps. Consumers are idempotent, so replaying is safe.
- **Bonus:** explaining why caches and timelines must be rebuilt from the
  primary and never from a replica.

### Q3. "Where in your system can the same event be delivered twice? Why does nothing go wrong when it is?"

A good answer covers:
- When to expect duplicates:
  - after a restart, the relay replays from the last confirmed WAL position;
  - a consumer that crashes before committing its offset replays those
    records.
- Every consumer is idempotent:
  - Elasticsearch uses external versions;
  - stats use a dedupe table;
  - adding a post to a timeline (ZADD) is idempotent;
  - deleting a cache key is naturally repeatable.
- So the guarantee is "at-least-once plus idempotency", **not** exactly-once.

### Q4. "Name the places where your system deliberately gives up some consistency. What does it get in return each time?"

A good answer names at least three:
- Search and timelines lag by about a second, in exchange for decoupling
  and failure isolation.
- Replica reads can be behind, in exchange for offloading the primary.
  Tokens keep read-your-writes, but only on the same device.
- Async replication has a non-zero RPO, in exchange for low write latency.
  Synchronous replication gets RPO 0 but adds 72 ms per write.
- The cache TTL is only a backstop; freshness comes from active
  invalidation.
- **He should also know the counter-example:** like counts are kept exact,
  at the cost of some write throughput.

### Q5. "Which numbers on your resume would hold on real production hardware, and which would change? Why?"

This tests whether he understands his own experimental method. It best
separates "built it" from "memorized it".

A good answer covers:
- **Correctness results would hold**: 0 writes lost, 0 split-brain writes,
  0% stale reads. The mechanisms guarantee them, whatever the hardware.
- **Latency and throughput would change**: every component shared one
  4-vCPU machine with the load generator.
- **Ratios probably hold.** For example, going from 6 round trips to 1 cuts
  latency proportionally.
- **Untested claims are off-limits.** Throughput scaling with more machines
  was never measured.

---

## Part 2: Mock interview by resume bullet

### Bullet 1: Online Resharding

**Q: "Walk me through how you moved a shard to another database while it was still taking writes."**

Key points:
1. Copy the shard in rounds without locks.
2. Lock only that shard's ownership row, which waits for in-flight writes.
3. Copy the final delta.
4. The source gives up ownership first, then the target takes over, then
   the directory is updated.

The median write pause per shard was 46 ms.

**Follow-up 1: "A router has an old copy of the shard map and sends a write to the old node. What happens?"**
- Every write checks ownership on the node, inside its own transaction.
  The old node answers "moved", and the router reloads the map and
  retries.
- Key point: the node decides ownership, not the router.

**Follow-up 2: "Your incremental rounds copy 'rows changed since the last round'. How do you avoid missing a transaction that started before a round but committed after it?"**
- Each round starts from the previous round's snapshot xmin, the oldest
  transaction still running then, not from "the highest transaction id
  seen".
- **Bonus:** mentioning that the load test caught this bug only 1 time in
  3, so he turned it into a deterministic test that fails every time.

### Bullet 2: Cache Consistency at Scale

**Q: "You delete the cache key on every write. How can the cache still serve stale data?"**

Key points: the stale-set race.
1. A slow reader loads version 1.
2. The writer commits version 2 and deletes the key.
3. The slow reader writes version 1 back into the cache.

The cache stays wrong until the next edit or the TTL. Measured: 29.5% of
reads were stale.

**Follow-up 1: "Why wasn't singleflight enough?"**
- Singleflight only merges requests within one process, so database loads
  grow with the instance count: 32/s with 8 instances, 128/s with 32. It
  also cannot stop stale write-backs.
- Leases work across instances. Only the lease holder may fill the cache,
  and deleting the key also deletes the lease.
- **Bonus:** the load test showed 0% stale with singleflight, but that was
  timing luck.

**Follow-up 2: "What if the API crashes right after committing, before it deletes the key? What if the lease holder crashes?"**
- If the API crashes after committing, a CDC consumer group deletes the
  key again from the change stream. Deleting twice is harmless.
- If the lease holder crashes, the lease expires after 2 s. Meanwhile,
  other readers are served from the database without filling the cache.

### Bullet 3: Region Failover

**Q: "How did your drill produce 54,803 split-brain writes, and how does fencing prevent that?"**

Key points:
- During the partition, US clients could still write to the US primary.
  EU detected the failure and promoted its replica, so both regions
  accepted writes.
- How fencing works:
  - the primary must renew a lease through a witness, and makes itself
    read-only when it can't;
  - EU waits for the lease to expire before promoting.

**Follow-up 1: "So fencing fixed it. Did you lose any data?"**
- Yes. Writes the primary accepted between the partition and the fence are
  still lost. Async replication always has a non-zero RPO.
- Synchronous replication loses nothing, but adds 72 ms per write, and the
  cut-off US region cannot write at all. That is the CAP trade-off, and the
  choice depends on the type of data.
- **Trap:** if he says fencing solved everything, he hasn't understood it.

**Follow-up 2: "You cut cross-region post latency from 429 to 73 ms. How? And why couldn't the consistency token be read in the same SQL statement as the insert?"**
- BEGIN, two INSERTs, a SELECT and COMMIT became one CTE statement. The
  token query is pipelined right behind it with its own Sync. Six round
  trips became one.
- An LSN read inside the same statement comes before the commit record. A
  replica could reach that position without having applied the commit,
  which breaks read-your-writes.

### Bullet 4: Performance Debugging

**Q: "Search collapsed at 1,000 requests per second. How did you figure out the personalized feed was the cause?"**

Key points:
- Elasticsearch's search thread pool was full: 51,138 rejected requests.
- A control experiment: the same load without the feed had 0 failures.

**Follow-up 1: "Your first fix, the bulkhead, didn't help. Why not, and what did that tell you?"**
- The hypothesis was "too much concurrency fills the thread pool". Search
  still collapsed with feed concurrency limited to 1, so the hypothesis was
  wrong.
- Load-testing the feed alone showed each request used about 3x the CPU
  its `took` time suggested: kNN uses concurrent segment search, so one
  query occupies several cores.
- Lesson: measure resource cost, not just latency.

**Follow-up 2: "You removed the more_like_this query. How did you know recommendations didn't get worse?"**
- He tracked which retrieval source each served item came from: only 1%
  came solely from that query. Without it, the top 20 results were 100% the
  same for all 40 users sampled.
- **Bonus:** "measure what a feature is worth before paying for it."

---

## How to run the mock interview

1. **Part 1:** have him answer the five questions without notes.
   - Strong answers to Q1 and Q5 mean he really understands the project.
   - If he can't answer Q5, he is reciting numbers without understanding
     the method behind them.
2. **Part 2:** play the interviewer.
   - Let him talk for about 2 minutes on each main question.
   - Aim the follow-ups at whatever he left out.
   - The **Trap** and **Bonus** notes mark what separates an average answer
     from a strong one.
3. For anything he can't answer, point him to
   [interview-prep.md](interview-prep.md): Part 1 covers the design
   questions, Part 2 the stories.
