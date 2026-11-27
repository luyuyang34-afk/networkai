# Experiment: two regions on one machine

A simulated cross-region link (`cmd/wanproxy`) adds 35 ms in each
direction: a 70 ms RTT, roughly US-East to EU-West. Two questions:

1. What does the link do to an API whose database is in the other region?
2. What happens to data when the regions lose contact and EU takes over?

## 1. Writes from the far region

`go run ./tools/experiments/wan`: the store connects through wanproxy and
creates posts, each time also fetching the consistency token (ADR 0007).

| Variant | Round trips | P50 | P95 |
|---|---|---|---|
| Before: transaction (BEGIN, INSERT, INSERT outbox, SELECT, COMMIT) + token query | 6 | 429 ms | 431 ms |
| One statement (data-modifying CTE) + token query | 2 | 144 ms | 146 ms |
| **Pipelined: the CTE and the token query in one round trip** | 1 | **73 ms** | 74 ms |

- On one machine the six round trips cost under 2 ms and nobody notices.
  Across regions, **latency is round trips × RTT**, and the code's shape
  decides it.
- The token must be taken *after* commit, or it can point before the
  commit record and break read-your-writes. Pipelining it with its own
  Sync keeps both properties.
- Not fixed: pgxpool pings a connection idle for more than 1 s before
  using it. On this link that is one more RTT (pipelined write after idle:
  144 ms). Fixes are a longer ping threshold or keeping connections warm.
  The right fix is to not write across regions at all for most requests;
  see "home region" in ADR 0010.

## 2. Region failover drill

`go run ./tools/experiments/failover -mode async|fenced|sync` (compose
profile `multiregion`, a throwaway cluster).

- **Cluster.**
  - `pg-us` is the primary in region US. US clients connect to it
    directly.
  - `pg-eu` is a replica in EU, streaming over the 70 ms link.
  - EU clients reach "the primary" through `vip-eu`, a switchable
    endpoint that stands in for a DNS name or load balancer.
- **Failure.** A network partition between the regions, not a crash. US
  clients can still reach `pg-us`, and that is the hard case.
- **Failover.**
  - EU declares US dead after 3 failed probes, 500 ms apart.
  - It promotes `pg-eu` with `pg_promote()` and points `vip-eu` at it.
- **Writers.** 4 per region, each writing sequentially.
- **Check.** Every acknowledged write is looked up on the new primary.

| Mode | US write P50 | EU write P50 | Acked writes lost (RPO) | Split-brain writes | EU write outage (RTO) | US writes during partition |
|---|---|---|---|---|---|---|
| **async** | 0.5 ms | 72 ms | 259 from before the partition + 15,027 during it | **54,803** | 2.0 s | accepted, then lost |
| **fenced** (async + leader lease) | 0.6 ms | 72 ms | 199 before + 22,478 during | **0** | 5.0 s | accepted until fenced at +2.6 s, then refused |
| **sync** (`synchronous_standby_names = pg_eu`) | **72.5 ms** | 143 ms | **0** | **0** | 2.0 s | **unavailable** (commits wait for EU) |

What it shows:

1. **Async replication + automatic failover = split brain.**
   - After promotion both regions accepted writes: 54,803 acknowledged
     writes went to a primary that no longer counts.
   - On top of that, every write the old primary accepted after the
     partition was lost.
   - Even writes acknowledged before the partition were lost (259): they
     were committed in US but still on the wire to EU.
2. **Fencing removes split brain, not data loss.**
   - The old primary holds a leader lease renewed through a witness. When
     it cannot renew, it makes itself read-only after the TTL; Patroni
     does the same with etcd.
   - EU waits out the TTL before promoting.
   - The price is a longer outage (5.0 s vs 2.0 s).
   - Writes accepted between the partition and the fence are still lost.
     Async means a non-zero RPO by definition.
3. **Synchronous replication gives RPO = 0 and costs a WAN round trip on
   every commit.**
   - US-local writes went from 0.5 ms to 72.5 ms, and throughput per
     sequential writer fell by about 130x.
   - During the partition, the US region could not write at all. This is
     the CAP trade-off made concrete: consistency was kept by giving up
     availability on the minority side.
4. **There is no free option.** Real systems pick per data type:
   - money and identity use synchronous or quorum writes (Spanner,
     Aurora's 4-of-6, DynamoDB global tables only for idempotent data);
   - feeds and likes accept async, a small RPO, and fencing.

## Pitfalls found

- **Synchronous replication cannot be on from the first start.** With
  `synchronous_standby_names` set at startup, the image's init step hung:
  its `CREATE DATABASE` waited for a standby that did not exist yet. The
  drill now starts async and switches with `ALTER SYSTEM` once the replica
  is streaming.
- **A TCP accept through a proxy proves nothing.** The local proxy accepts
  connections even when the far side is unreachable, so the lease agent
  requires a reply from the far side (a PostgreSQL SSLRequest answer)
  before it counts the witness as reachable.
- **After a sync failover the new primary has no standby.** It runs alone
  until the other region is rebuilt (`pg_rewind` or a fresh basebackup).
  So RPO = 0 holds for this failure only, not the next one.
- **CDC does not survive this failover.** In PostgreSQL 16, logical
  replication slots are not copied to standbys. The CDC relay (ADR 0006)
  would have to create a new slot on the new primary and reconcile from a
  snapshot. PostgreSQL 17 failover slots address this.
