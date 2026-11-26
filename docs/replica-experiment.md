# Experiment: read replicas and read-your-writes

Reproduce with `REPLICA_APPLY_DELAY=1s docker compose up -d`, then
`go run ./tools/experiments/replica -n 100`.

## Setup

- One primary and one streaming replica (asynchronous physical
  replication). The API sends staleness-tolerant reads (browse, single post,
  profile, like counts, timeline hydration) to the replica.
- One iteration: a user creates a post, then **immediately** lists their own
  posts. A violation is the new post missing from that list.
- 100 iterations without the consistency token, then 100 with it (the
  `X-Consistency-Token` returned by the write, sent back on the read).
- Routing decisions are read from `networkai_db_read_routes_total`. One list
  request makes two decisions: the posts query and the like-info query.

## Results

| Replica | Mode | Own post missing right after writing | Reads routed |
|---|---|---|---|
| applies WAL 1 s late (`recovery_min_apply_delay`) | plain read/write splitting | **100 of 100** | replica 200 |
| applies WAL 1 s late | consistency token | **0 of 100** | primary 200 |
| **no artificial delay** | plain read/write splitting | **31 of 100** | replica 200 |
| no artificial delay | consistency token | **0 of 100** | replica 20, primary 180 |

500 anonymous browse requests (no token): **100% served by the replica.**

## What it shows

1. **Replication lag is not an edge case.** With no artificial delay, on
   one machine, a third of "write then read back immediately" requests
   missed the write. Asynchronous replication lags by milliseconds, and a
   user reloading right after posting is that fast.
2. **Consistency tokens make read-your-writes exact** (0 violations in both
   conditions), at a cost of one replay-position check per token read. It
   is cached for 20 ms, and read P50 was 0.8 vs 0.9 ms.
3. **The offload is kept.**
   - Reads without a token always use the replica.
   - A token read uses the replica as soon as it has caught up: 10% of
     token reads even immediately after writing, with no artificial delay.
   - Only the writer's own reads, right after writing, go to the primary.
4. **Time-based alternative and why not.** "Send a user's reads to the
   primary for N seconds after any write" needs a guessed N. Too small and
   violations come back under load; too large and the primary loses the
   offload. The token adapts to the actual lag.

## Bugs and pitfalls found

- **Cache fills from a replica make caches wrong for days.** A timeline
  rebuilt from a lagging replica misses a just-committed post, and that
  post's fan-out already skipped the not-yet-existing timeline. Rebuilds now
  read the primary (`store.ForcePrimary`).
- **A replica must not have lower limits than its primary.** Recovery
  refused to start because the primary had `max_wal_senders=64` and the
  replica 10 ("insufficient parameter settings"). The replica entrypoint
  passes matching settings.
- **Physical replication needs a `replication` entry in pg_hba.conf.** The
  default image has none, so CI appends one before starting the replica.
