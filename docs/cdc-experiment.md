# Experiment: polling worker vs CDC + consumer groups

Reproduce with `tools/experiments/coupling.sh` against the docker compose
stack. It switches the stack between the two modes itself.

## Setup

- **Polling mode**: the original outbox worker. One loop polls every
  500 ms and, per event, indexes for search and then fans out to timelines.
- **CDC mode**: the relay streams outbox inserts from the WAL to
  `post-events` (12 partitions). `search-indexer` and `timeline-fanout`
  are separate consumer groups in separate processes.
- One celebrity with 100,000 followers, all with an active cached timeline.
  `CELEBRITY_THRESHOLD` is set very high, so the celebrity is pushed (worst
  case).
- A probe = post over HTTP, then poll `/posts/search` until the post
  appears. Timeout 60 s.
- Same machine as [benchmark.md](benchmark.md): 4 vCPU, everything local.

## Results

| | New post searchable (P50 of 10) | ...while Redis is down | ...right after a 100k-follower fan-out |
|---|---|---|---|
| Polling worker (index + fan-out in one loop) | 1,009 ms | **not searchable within 60 s** | 1,416 ms |
| CDC + consumer groups | 1,001 ms | **730 ms** | **734 ms** |

## What it shows

1. **Failure isolation.**
   - With Redis down, the polling worker pauses on the fan-out failure.
     Search indexing stops with it, for every post until Redis returns.
   - With CDC, only `timeline-fanout` pauses; search is unaffected.
   - The first post after Redis fails still gets indexed in polling mode,
     because the worker indexes first and then fails on fan-out. The
     experiment therefore posts a trigger post before measuring.
2. **No head-of-line blocking across jobs.**
   - In polling mode, a post right after a 100k-follower fan-out waits for
     it: +0.4 s here, linear in follower count (see
     [timeline-experiment.md](timeline-experiment.md)).
   - With CDC, search does not wait for fan-out.
3. **Normal latency is the same (~1 s)**, because Elasticsearch's 1 s refresh
   interval dominates, not polling. Removing the 500 ms poll does not show
   up end to end here.
   - CDC's value is isolation, replay and not polling the database.
   - It is not lower latency on this setup.

## Bugs found while building this

- **Silent data loss on consumer restart (real bug, fixed).**
  - The consumer started at the *end* of the log on partitions it had never
    committed, like Kafka's default `auto.offset.reset=latest`.
  - After a restart, records produced while it was down on those partitions
    were skipped. A probe post was never indexed.
  - A regression test reproduces it: with "latest", 7 of 21 records arrived
    and 14 were lost. Durable groups now start at the beginning (replays are
    safe, since handlers are idempotent): 21 of 21.
- **60 s stall on every consumer restart (fixed).**
  - Shutdown closed the client without leaving the group. Because of
    `BlockRebalanceOnPoll`, the leave was held back, so the coordinator
    waited for the old member's rebalance timeout before reassigning
    partitions.
  - The consumer now allows the rebalance and leaves the group explicitly.
  - Rejoin time went from 60 s to 0.1 s.
- **Flaky tests from reused consumer group names.** Group names are
  cluster-wide; tests now use unique names. Suite time went from 20–40 s to
  5 s.
- **CDC only sees changes after its slot exists.** Posts written before the
  slot was created were missing in early tests. The slot is now created
  before streaming starts, and pre-existing data is backfilled with
  `reindex` (Debezium calls this the initial snapshot).
- **An experiment bug.** The first version of this script put the mode
  label (spaces, parentheses) into search words. Every probe "timed out",
  and the whole run hit its 25-minute limit with no results.
