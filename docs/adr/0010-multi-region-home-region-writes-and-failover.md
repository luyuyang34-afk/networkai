# ADR 0010: Multi-region: home-region writes, one-round-trip writes, fenced failover

- Status: accepted (simulated on one machine; see Consequences)
- Date: 2026-11-27

## Context

Users far from the database pay its distance on every request, and a
region can lose contact with the others. Measured
([multiregion-experiment.md](../multiregion-experiment.md)), with a 70 ms
RTT link:

- creating a post from the far region took 429 ms: 6 round trips;
- in a partition followed by EU promotion:
  - async replication accepted 54,803 split-brain writes and lost every
    write the old primary took after the partition;
  - sync replication lost nothing, but made every US commit wait 72 ms
    and stopped US writes entirely during the partition.

## Decision

1. **One primary region per dataset; reads served locally.** Each region
   has replicas and caches. Reads use consistency tokens (ADR 0007), so a
   local replica serves them unless the user just wrote.
2. **Writes cost as few round trips as possible when they must cross
   regions.** `CreatePost` is one data-modifying CTE, with the consistency
   token pipelined behind it: 1 round trip, 73 ms instead of 429 ms.
3. **Home region (next step, not implemented).** Route a user's writes to
   the region that is primary for their shard. With per-shard primaries
   (ADR 0009 places logical shards on nodes, and nodes can live in
   different regions), most users write locally. This is how large
   systems avoid cross-region writes: Facebook's per-user home region,
   Spanner's leader placement.
4. **Failover is fenced.**
   - The primary holds a leader lease renewed through a witness. If it
     cannot renew within the TTL, it makes itself read-only.
   - The other region promotes only after the TTL has passed since it
     lost contact.
   - This trades about 3 s of extra outage for no split brain.
5. **Replication mode per data type.**
   - Feed data (posts, likes, follows) stays async plus fencing: a bounded
     RPO of seconds for a 0.5 ms commit.
   - Data that must never be lost (payments, account security) would use
     synchronous or quorum commit and accept the latency.

## Alternatives considered

| Option | Why not (for feed data) |
|---|---|
| Async + automatic failover without fencing | Measured split brain: two primaries accepting writes. |
| Synchronous replication everywhere | RPO 0, but +1 WAN RTT on every commit, and the region that cannot reach the standby cannot write. |
| Multi-primary (writes accepted in both regions) | Needs conflict resolution (last-writer-wins loses data, CRDTs fit counters but not edits). Too much for this data model now. |
| Manual failover only | No split brain, but RTO is however long it takes to page a human. |
| A distributed SQL database (Spanner, CockroachDB) | Consensus per write gives RPO 0 with automatic failover. It is the real answer at large scale; building this shows the problem it solves. |

## Consequences

- Cross-region write latency is bounded by one RTT for post creation.
  Other writes (edits, likes, follows) still pay a transaction plus a
  token query, 2 to 4 round trips.
- Failover without fencing must never be enabled.
- Fenced failover has an RTO of detection + lease TTL + promotion (5 s
  measured) and an RPO of roughly the replication lag plus the TTL.
  Writes the old primary accepted before fencing are lost and must be
  treated as such.
- CDC must be rebuilt on the new primary after failover, because PG 16
  does not copy logical slots to standbys. Consumers are idempotent (ADR
  0002, 0006), so replaying from a snapshot is safe.
- Everything here ran on one machine with a simulated link. Real regions
  add jitter, asymmetric routes and partial partitions, which a drill on
  real infrastructure would have to cover.
