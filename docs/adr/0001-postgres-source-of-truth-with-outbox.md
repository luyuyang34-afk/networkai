# ADR 0001: PostgreSQL as source of truth, Elasticsearch synced via a transactional outbox

- Status: accepted
- Date: 2026-11-23

## Context

The first version stored everything (users, posts, even passwords) only in
Elasticsearch, and wrote media to GCS and metadata to Elasticsearch as two
independent steps.

Elasticsearch is a search engine, not a transactional database: no
multi-document transactions, near-real-time visibility (refresh interval),
and its operational guidance is to treat indices as rebuildable. Users and
post ownership need constraints (unique usernames, foreign keys) and atomic
read-check-write operations (ownership checks).

Once PostgreSQL holds the data, every post change must also reach
Elasticsearch. Writing to both from the request handler (a "dual write") is
unsafe: there is no transaction spanning two systems, so a crash or a
network error between the two writes leaves them permanently inconsistent.

## Decision

1. PostgreSQL is the only source of truth. Elasticsearch is a derived index
   that can be rebuilt at any time (`networkai reindex`).
2. Each post mutation inserts a row into an `outbox` table **in the same
   transaction** as the change. Either both commit or neither does.
3. A separate `indexer` process polls the outbox (`FOR UPDATE SKIP LOCKED`,
   so several indexers can run in parallel), applies each event to
   Elasticsearch, and marks it processed only after success: at-least-once
   delivery.
4. The API does not depend on Elasticsearch for writes or for browsing
   (`GET /posts` reads PostgreSQL). Only `/posts/search` needs it, and it
   returns `503 search_unavailable` during an outage.

## Alternatives considered

| Option | Why not (for now) |
|---|---|
| Dual write in the handler | Inconsistent on partial failure; no way to recover the lost write. |
| Write ES first, then PG | Same problem, plus search can show posts that never committed. |
| Change Data Capture (Debezium reading the PostgreSQL WAL, via Kafka) | The most robust option at scale and needs no application changes, but adds Kafka, Kafka Connect and Debezium to operate. Overkill for one consumer. The outbox table is also exactly what Debezium's "outbox event router" consumes, so this is a natural migration path. |
| PostgreSQL `LISTEN/NOTIFY` only | Notifications are lost if no listener is connected; still needs a durable table. Could be added on top to cut polling latency. |
| Synchronous ES write with retry, no outbox | Retries die with the process; a crash loses the write. |

## Consequences

- Search is **eventually consistent**: typically under a second behind
  (poll interval 500 ms + ES refresh 1 s). Browsing reads from PostgreSQL and
  is always up to date.
- Delivery is at-least-once, so indexing must be idempotent and tolerate
  reordering. See ADR 0002.
- A polling worker adds load on PostgreSQL proportional to poll frequency; the
  partial index `outbox_pending_idx` keeps each poll cheap. Processed rows
  are pruned after 7 days.
- Media files live outside the database, so creating a post is still two
  systems (object storage, then PostgreSQL). We upload first and delete the
  file if the database write fails (a compensating action). A crash between
  the two can still orphan a file; a periodic sweep is listed as future
  work.
