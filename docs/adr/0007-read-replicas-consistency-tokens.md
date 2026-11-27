# ADR 0007: Read replicas with consistency tokens for read-your-writes

- Status: accepted
- Date: 2026-11-26

## Context

Reads far outnumber writes in a social product. Serving them all from the
primary makes it the bottleneck for both kinds of traffic. Streaming
replicas absorb reads, but they replicate asynchronously and can be behind.
A user who writes and immediately reads may not see their own write.
Measured: 31% of immediate read-backs missed the write, even with no
injected lag ([replica-experiment.md](../replica-experiment.md)).

## Decision

1. **A streaming hot-standby replica** cloned with `pg_basebackup`. The API
   routes staleness-tolerant reads to it:
   - list and get posts;
   - profiles;
   - like counts;
   - timeline hydration and celebrity pulls.
   Writes, logins and anything that decides authorization stay on the
   primary.
2. **Consistency tokens.**
   - Every successful write response carries `X-Consistency-Token`, the
     primary's WAL position (LSN) after the commit.
   - Clients send the latest token on every request.
   - A read with a token goes to the replica only if
     `pg_last_wal_replay_lsn() >= token` (checked at most every 20 ms);
     otherwise it goes to the primary.
   - The frontend keeps the token per tab.
3. **Cache fills always read the primary** (`store.ForcePrimary`). A cache
   populated from a lagging replica stays wrong after the replica catches
   up.
4. **Observability**: routing decisions by target, and replica lag in WAL
   bytes and seconds.

## Alternatives considered

| Option | Why not |
|---|---|
| Synchronous replication (`synchronous_commit = remote_apply`) | Every write waits for the replica: higher write latency, and the replica's availability becomes the primary's. |
| Sticky primary for N seconds after a write | N is a guess: violations under heavier lag, or wasted offload when N is large. |
| Read everything from the primary | No offload; the primary stays the bottleneck. |
| Route by endpoint only ("profile pages read primary") | Any endpoint can show a user their own data; correctness would depend on remembering every one. |

This is the LSN form of what other systems call causal-consistency tokens:
Aurora and PostgreSQL LSNs, MongoDB's cluster time, MySQL GTID waits.

## Consequences

- Read-your-writes holds for a client that sends its token: 0 violations
  measured.
- Across devices it does not, because each device has its own token. A
  user posting on a phone and reading on a laptop within milliseconds can
  still miss it. Accepted.
- Monotonic reads across *different* replicas would need the token on
  every read, even without a write by this client. That only matters with
  more than one replica.
- Every write pays one extra `pg_current_wal_lsn()` call, and token reads
  pay a cached replay-position check.
- A replica outage today makes replica-routed reads fail. A production
  setup would health-check replicas and fall back to the primary; that is
  noted as future work.
