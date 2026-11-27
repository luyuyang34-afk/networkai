# Experiment: online resharding from 2 to 4 PostgreSQL nodes

Reproduce:

```bash
docker compose --profile shards up -d        # four independent PostgreSQL 16 nodes
go run ./tools/experiments/reshard           # fenced moves (the design)
go run ./tools/experiments/reshard -naive    # copy, then flip the map (no fence)
```

## Setup

- **Cluster.** 64 logical shards; a post's shard is a hash of its author.
  - Start: nodes n1 and n2 own 32 shards each, with 200,000 seeded posts.
  - Nodes n3 and n4 start empty.
  - The directory (`shard_map`) lives in the main database.
- **Load**, throughout the run: 16 writers and 16 readers.
  - Each writer and reader has its own router, standing in for a separate
    API instance with its own cached shard map.
  - Writers alternate creating posts and editing random acknowledged ones.
  - Readers mix get by id (50%), posts by author (25%, single shard) and
    the global newest-posts page (25%, scatter-gather to every node).
- **Timeline.**
  - After 10 s, `Mover.Rebalance` moves the 32 shards whose new home is n3
    or n4, one at a time.
  - Load continues for 10 s after the last move.
- **Verification**, at the end: every acknowledged write is read back
  through a fresh router. **Lost** means the post is missing. **Rolled
  back** means it is there with a lower version than was acknowledged.
- **Machine.** One 4-vCPU machine runs all four nodes, the directory and
  the load generator. Throughput cannot scale with nodes here: they share
  the CPUs. This experiment measures correctness and the latency cost of
  moving data. Throughput scaling needs one machine per node, for example
  four GCE VMs and the `-nodes` flag.

## Results

### Fenced moves (the design)

| Operation | Before (2 nodes) P50 / P99 | During moves P50 / P99 | After (4 nodes) P50 / P99 | Errors |
|---|---|---|---|---|
| create | 7.3 / 47.6 ms | 6.4 / 71.2 ms | 7.9 / 61.2 ms | 0 |
| edit | 7.5 / 47.6 ms | 6.6 / 71.7 ms | 8.1 / 61.9 ms | 0 |
| get by id | 1.1 / 15.6 ms | 1.2 / 27.5 ms | 1.5 / 20.0 ms | 0 |
| posts by author (1 shard) | 3.8 / 31.8 ms | 3.7 / 48.4 ms | 4.5 / 39.9 ms | 0 |
| newest posts (all nodes) | 6.0 / 43.5 ms | 10.8 / 88.7 ms | 11.5 / 67.6 ms | 0 |

- 32 moves in 45.7 s: on average 3,524 rows per move, P50 1.4 s per move.
- **Writes to the moving shard were blocked for P50 46 ms, max 134 ms per
  move.** The other 63 shards were never blocked.
- 949 requests got `ErrMoved` and were retried transparently. These came
  from routers with an old map, and from writes during a cutover.
- **46,608 acknowledged writes: 0 lost, 0 rolled back.**
- Rows after cleanup: 246,608 = 200,000 seeded + 46,608 new, with no
  duplicates left on old nodes.

### Naive moves (copy, then flip the map; no fence, no final delta)

| | Fenced | Naive |
|---|---|---|
| Acknowledged posts lost | **0** | **98** |
| Acknowledged edits rolled back | **0** | **52** |
| Edit errors (post vanished) | 0 | 58 |
| Row count matches seeded + created | yes | no |

The naive move loses every write that lands on the old node between its
copy and the map flip. Nothing fails at the moment of the move. The posts
are simply gone afterwards, and the writer had been told "200 OK".

## What it shows

1. **Moving data while serving is a correctness problem before it is a
   performance problem.**
   - The naive approach looks fine: no errors during the move, fast
     cutover.
   - Its damage only shows when every acknowledged write is checked
     afterwards.
2. **A short per-shard fence is enough.**
   - Copy rounds take the bulk of the time (P50 1.4 s per move) without
     blocking anyone.
   - Writes to one shard wait at most 134 ms while the last few hundred
     rows are copied under the lock.
   - Clients do not see errors: the router retries `ErrMoved` with
     backoff.
3. **The node, not the router, decides ownership.**
   - 949 requests used a stale map and were redirected.
   - If routers decided ownership from their cache, each of those would
     have been a write to a node that no longer owns the data. That is the
     naive failure again.
4. **Scatter-gather pays for every node.**
   - The global newest-posts page went from 6.0 to 11.5 ms P50 going from
     2 to 4 nodes: its latency is the slowest node's.
   - Single-shard queries (by id, by author) did not change.
   - This is why the shard key is the author: the common queries stay on
     one node.
   - Fan-out queries get worse as the cluster grows ("The Tail at Scale",
     Dean & Barroso 2013). They belong in a derived store: Elasticsearch
     and the Redis timelines already serve that role.

## Bugs and pitfalls found

- **A new node plans queries with an empty table's statistics.**
  - Symptom: in the first run, newest-posts on n3/n4 took 150–280 ms per
    query, versus 0.2 ms when idle. P99 during moves was 640–860 ms.
  - Cause: `auto_explain` showed a seq scan plus sort with `rows=5`. The
    planner still believed the table had the 5 rows it held before the
    bulk copy.
  - Fix: the mover now runs `ANALYZE` on the target before cutover, and
    `VACUUM (ANALYZE)` on the source after deleting the moved rows.
  - Result: newest-posts P99 during moves dropped to 89 ms.
- **The copy horizon must be the snapshot's xmin, not "the highest change
  seen".**
  - A write transaction that started before a copy round and commits
    after it would be skipped by a "highest xid seen" (xmax) horizon.
  - The load test caught this mutation only 1 time in 3.
  - `TestInFlightWriteDuringCopyIsNotSkipped` holds such a transaction
    open across the copy and catches it every time.
- **The final delta matters even though it is small.** Removing the copy
  under the fence lost 14 of 1,926 acknowledged writes in
  `TestRebalanceUnderLoadKeepsEveryWrite`. That is under 1%, which is
  exactly the kind of loss nobody notices until a user does.
