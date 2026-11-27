# ADR 0009: Posts sharded by author, with online resharding

- Status: accepted (as a subsystem; the API still uses the unsharded store, see Consequences)
- Date: 2026-11-26

## Context

One PostgreSQL primary has a ceiling: write throughput, storage, vacuum
time, and backup and restore time all grow with one machine's data. Read
replicas (ADR 0007) and caches (ADR 0008) only help reads. Past that point
the data has to be split across primaries, and moved again as it grows,
without downtime.

## Decision

1. **Fixed logical shards.**
   - 64 logical shards, chosen by hash of the author id.
   - A directory table maps logical shard → physical node.
   - Growing the cluster moves whole logical shards; rows are never
     rehashed. This is the scheme Instagram, Notion and Figma describe.
2. **Shard by author.**
   - "Post by id" and "posts by author" each touch one node.
   - The post id is time-ordered (UUIDv7 layout, version 8) and carries
     its logical shard in one byte, so an id lookup needs no extra query.
3. **Ownership lives on the node.**
   - Each node has `shard_owner(lshard, owned)`.
   - Every write checks it inside its transaction with `SELECT ... FOR
     SHARE`, and reads check it in the same statement or snapshot.
   - A router with a stale map gets `ErrMoved`, reloads the directory and
     retries with backoff, for up to 2 s.
4. **Online move of one logical shard** (`shard.Mover`):
   1. The target marks the shard not-owned.
   2. Copy rounds run without locks:
      - round 0 copies everything;
      - each later round copies rows whose `writer_xid` is at least the
        previous round's snapshot xmin;
      - copies are version-guarded upserts, so repeating one is harmless.
   3. `ANALYZE` the target.
   4. Fence:
      - `SELECT ... FOR UPDATE` on the source's owner row: this waits for
        in-flight writes and blocks new ones for this shard only;
      - copy the final delta;
      - mark the source not-owner and commit;
      - mark the target owner;
      - update the directory.
   5. Delete the rows from the source, then `VACUUM (ANALYZE)` it.
5. **Safety over availability during cutover.**
   - The source gives up ownership before the target takes it.
   - A crash mid-cutover leaves the shard owned by nobody, so its writes
     fail until the move is re-run (it is idempotent).
   - Two nodes never accept writes for the same shard.

## Alternatives considered

| Option | Why not |
|---|---|
| `hash(author) % nodes` | Adding a node rehashes almost every row. |
| Consistent hashing on rows (Dynamo-style) | Moves less data than modulo, but rows still move individually. Logical shards make a move a bulk copy of a known set with one fence. |
| Range sharding by post id or time | All new writes go to the newest range: a permanent hot shard. |
| Shard by post id | Even spread, but "posts by author" becomes scatter-gather, and that is a primary read. |
| PostgreSQL logical replication for the copy | The right tool at scale (Figma). It needs replication slots and a publication per shard, and the cutover fence is the same problem; the xmin-horizon copy keeps the mechanism visible. |
| Dual writes from the application during migration | Two writes that can partially fail. They still need backfill plus verification, and the cutover moment is still a race. |
| Router-side ownership (trust the cached map) | A stale router writes to the old node after the move: the naive failure measured below. |
| Citus / Vitess | The production answer for many teams. Building it once shows what they do: the fence, the horizon and directory versioning. |

## Consequences

Measured ([sharding-experiment.md](../sharding-experiment.md)):

- 2 → 4 nodes under 16 writers and 16 readers:
  - 0 errors;
  - 0 lost and 0 rolled-back writes out of 46,608;
  - per-shard write block P50 46 ms, max 134 ms.
- The naive copy-then-flip lost 98 posts and rolled back 52 edits.

Other consequences:

- Cross-shard queries (newest posts globally) are scatter-gather, and their
  latency is the slowest node's: 6.0 → 11.5 ms P50 from 2 to 4 nodes. Those
  views should come from derived stores (Elasticsearch, Redis timelines),
  not from the shards.
- Cross-shard transactions do not exist. A post and its author's row can
  live on different nodes. The outbox/CDC path (ADR 0006) would need one
  relay per shard.
- **Not wired into the API yet.** The API's post store still joins posts
  with users and likes, and writes the outbox in the same transaction.
  Moving it onto the router means:
  - co-locating likes and outbox rows with their post's shard;
  - one CDC relay per node;
  - replacing the user join with a lookup.
  That is the migration a real team does next, and it is recorded here
  rather than half-done.
- Throughput scaling was not measured: all nodes shared one 4-vCPU machine.
