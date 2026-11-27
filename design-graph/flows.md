# Request and data flows

Sequence diagrams for the paths interviewers ask about most. GitHub
renders them; elsewhere, paste them into any Mermaid viewer.

- [1. Creating a post](#1-creating-a-post)
- [2. Reading the home timeline](#2-reading-the-home-timeline)
- [3. Reading your own write through a replica](#3-reading-your-own-write-through-a-replica)
- [4. Reading a hot post through the cache](#4-reading-a-hot-post-through-the-cache)
- [5. Moving a shard online](#5-moving-a-shard-online)
- [6. Region partition and fenced failover](#6-region-partition-and-fenced-failover)

## 1. Creating a post

One statement writes the post and its outbox event. Everything after the
commit is asynchronous and independent per consumer group.

```mermaid
sequenceDiagram
    autonumber
    participant C as Client
    participant A as Go API
    participant P as PostgreSQL primary
    participant R as CDC relay
    participant K as Kafka
    participant S as search-indexer
    participant T as timeline-fanout
    participant I as cache-invalidation
    C->>A: POST /posts (JWT)
    A->>P: one round trip: INSERT post + outbox (CTE), then pg_current_wal_lsn()
    P-->>A: post row + commit LSN
    A-->>C: 201 Created + X-Consistency-Token
    P-)R: WAL: outbox insert (logical decoding)
    R->>K: publish, keyed by post id
    K-->>R: ack
    R-)P: confirm LSN (only after the ack)
    par independent consumer groups
        K-)S: event
        S->>P: re-read post (latest version)
        S->>S: index into Elasticsearch, external version
    and
        K-)T: event
        T->>P: author's follower count
        T->>T: push id into followers' Redis timelines (skip if 10k+ followers)
    and
        K-)I: event
        I->>I: DEL cache key + lease in Redis
    end
```

If any consumer is down, its group simply lags and catches up later. The
write already succeeded.

## 2. Reading the home timeline

```mermaid
sequenceDiagram
    autonumber
    participant C as Client
    participant A as Go API
    participant RD as Redis
    participant P as PostgreSQL (primary)
    participant RP as Replica
    C->>A: GET /timeline
    A->>RD: EXISTS tl:{user}
    alt timeline missing (new user, expired, Redis restarted)
        A->>P: rebuild from followees' recent posts (always the primary)
        A->>RD: write timeline (cap 800, TTL 7 days)
    end
    A->>RD: ZRANGE newest ids (pushed posts)
    A->>RP: recent posts of followed celebrities (pull)
    A->>A: merge and dedupe by time
    A->>RP: hydrate the 20 ids (primary if the client's token is ahead)
    A-->>C: page + cursor
```

## 3. Reading your own write through a replica

```mermaid
sequenceDiagram
    autonumber
    participant C as Client
    participant A as Go API
    participant P as Primary
    participant RP as Replica
    C->>A: POST /posts
    A->>P: write
    A-->>C: X-Consistency-Token: 0/3A1F2B8
    C->>A: GET /posts?author=me  (token 0/3A1F2B8)
    A->>RP: pg_last_wal_replay_lsn() (cached 20 ms)
    alt replica replayed >= token
        A->>RP: read
    else replica behind
        A->>P: read
    end
    A-->>C: includes the new post
```

Reads without a token, the vast majority, always go to the replica.

## 4. Reading a hot post through the cache

```mermaid
sequenceDiagram
    autonumber
    participant A1 as API instance 1
    participant A2 as API instance 2
    participant RD as Redis
    participant P as Primary
    A1->>RD: GET post:{id}
    RD-->>A1: miss
    A2->>RD: GET post:{id}
    RD-->>A2: miss
    A1->>RD: SET post-lease:{id} token NX PX 2000
    RD-->>A1: OK (lease holder)
    A2->>RD: SET post-lease:{id} NX
    RD-->>A2: taken, so poll for the fill (up to 300 ms)
    A1->>P: load post (primary)
    A1->>RD: Lua: if lease == token then SET post + DEL lease
    A2->>RD: GET post:{id}
    RD-->>A2: hit
    Note over A1,RD: Concurrent requests inside one instance share A1's load (singleflight)
    Note over RD: Close to expiry, one reader refreshes early in the background (XFetch)
    Note over RD,P: An edit deletes the key and the lease, so a racing fill of old data is rejected
```

## 5. Moving a shard online

```mermaid
sequenceDiagram
    autonumber
    participant M as Mover
    participant S as Source node
    participant T as Target node
    participant D as Directory
    participant W as Writers (via routers)
    M->>T: shard_owner(l) = false (refuse writes for now)
    loop copy rounds, no locks
        M->>S: REPEATABLE READ: xmin, rows with writer_xid >= previous xmin
        M->>T: upsert (higher version wins)
    end
    M->>T: ANALYZE
    M->>S: begin, then SELECT ... FOR UPDATE on shard_owner(l)
    Note over S,W: waits for in-flight writes, blocks new ones for this shard only
    M->>S: copy the final delta to the target
    M->>S: shard_owner(l) = false, then COMMIT
    W->>S: write: owner check returns ErrMoved
    M->>T: shard_owner(l) = true
    M->>D: shard_map(l) = target, epoch + 1
    W->>D: reload map
    W->>T: retry, succeeds
    M->>S: delete moved rows, then VACUUM (ANALYZE)
```

Between the source's commit and the target taking ownership, nobody owns
the shard. Writes for it retry. Two owners are never possible.

## 6. Region partition and fenced failover

```mermaid
sequenceDiagram
    autonumber
    participant US as US primary (+ lease agent)
    participant WI as Witness
    participant EU as EU replica
    participant VIP as EU VIP / detector
    US->>WI: renew lease (every 500 ms)
    Note over US,EU: network partition between the regions
    US-xWI: renew fails
    VIP-xUS: probes fail (3 x 500 ms)
    Note over US: lease TTL (3 s) expires
    US->>US: default_transaction_read_only = on, drop sessions (self-fence)
    Note over VIP: waits for the TTL since it lost contact
    VIP->>EU: pg_promote()
    VIP->>VIP: point VIP at EU
    Note over US,EU: no split brain. Writes US accepted before fencing are lost (async RPO).
```
