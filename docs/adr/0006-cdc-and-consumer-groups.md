# ADR 0006: CDC from the outbox to a partitioned log with per-job consumer groups

- Status: accepted (supersedes the polling worker of ADR 0001 as the default)
- Date: 2026-11-26

## Context

ADR 0001 introduced a transactional outbox drained by one polling worker.
After the home timeline (ADR 0005), that worker does two jobs per event:
index for search, then fan out to timelines. Measured problems
([cdc-experiment.md](../cdc-experiment.md)):

- **Coupling.** A Redis outage pauses the worker, so search indexing stops
  too. An Elasticsearch outage stops timeline fan-out.
- **Head-of-line blocking.** A large fan-out delays every event behind it,
  including unrelated search updates.
- **Polling latency and load.** Every 500 ms the worker queries the table,
  whether or not anything changed.
- **Every new job is another handler in the same loop.** Adding one means
  redeploying the worker, and nothing can replay history.

## Decision

Keep the outbox: it is still what makes "post row + event" atomic. Change
how it is consumed:

1. **CDC instead of polling.** PostgreSQL logical replication (`pgoutput`)
   streams inserts into `outbox` through a replication slot. The relay
   (`networkai relay`) decodes them and publishes to the `post-events`
   topic. Changes are pushed as they commit; nothing polls.
2. **A partitioned log.** `post-events` has 12 partitions keyed by post id.
   All events for one post land in one partition, in commit order.
3. **At-least-once.**
   - The relay confirms a WAL position to PostgreSQL only after the broker
     acknowledged every event up to it.
   - Consumers commit offsets only after handling.
   - A crash anywhere replays rather than loses.
   - Handlers were already idempotent (ADR 0002, ADR 0005), and the stats
     consumer deduplicates explicitly.
4. **One consumer group per job:** `search-indexer`, `timeline-fanout`,
   `post-stats`. Each group commits its own offsets, so groups never wait
   for each other. Each runs as its own process
   (`networkai consume <group>`), so a deploy, crash or slow dependency
   affects one job.
5. **Failure handling per group** (same rule as ADR 0003).
   - Dependency down: retry the same record with backoff; only that group's
     partition stalls.
   - Poison record: a few retries, then the dead-letter topic
     `post-events.dlq.<group>`, and the partition moves on.
6. **Replay.** 7-day retention means a group added later (post-stats starts
   from the beginning of the log) processes history without any producer
   change.

The polling worker remains (`--profile poll`) for comparison.

## Alternatives considered

| Option | Why not |
|---|---|
| Keep polling, run one worker per job with its own cursor column | Rebuilds a log on top of a table; still polling; replay limited to what the table keeps. |
| Debezium + Kafka Connect | The standard production choice. Not used here: its images were unreachable from the build environment, and writing the relay directly on `pgoutput` shows how CDC works. The outbox table is already in the shape Debezium's outbox router expects. |
| Publish to Kafka from the API handler | Back to the dual write (ADR 0001): the post and the event could diverge. |
| LISTEN/NOTIFY | Not durable; notifications are lost when no one listens. |

## Consequences

- **New failure mode: an idle or dead relay makes PostgreSQL retain WAL
  until the disk fills.** The relay confirms positions even when nothing
  relevant was written, the `networkai_replication_slot_lag_bytes` gauge
  exposes the backlog, and the `ReplicationSlotWALBacklog` alert pages on
  it (see the runbook).
- **CDC captures only what happens after the slot exists.** The slot is
  created before the relay starts streaming, and existing rows are
  backfilled with `networkai reindex` (Debezium's "initial snapshot").
- **Consumer group names are a cluster-wide namespace.** Reusing names
  across environments or test runs makes members wait for stale ones; this
  was found as a flaky test.
- **Three more moving parts:** the broker, the relay and the consumers. Each
  has metrics (relay events and delay, per-group results and lag) and alert
  rules.
- **Per-partition head-of-line blocking still exists within a group.** A
  huge fan-out still delays the timeline events behind it in the same
  partition. It just no longer delays search.
