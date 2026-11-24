# Runbook

What to do when an alert fires. Alerts are defined in
`deploy/prometheus/alerts.yml`.

## SearchIndexLagging

**Meaning.** The oldest unindexed outbox event is more than 2 minutes old,
so search results are out of date. Posting and browsing still work.

1. Is the worker paused? Check `networkai_outbox_paused` (indexer `:9091/metrics`).
   - **1**: Elasticsearch is unreachable or overloaded. Check cluster health
     (`curl $ES_URL/_cluster/health`). The backlog drains on its own once
     Elasticsearch recovers, with no action needed on the outbox.
   - **0**: go to step 2.
2. Is the indexer running? If not, restart it. Pending events are durable in
   PostgreSQL and nothing is lost.
3. Is it running but slow? Compare
   `rate(networkai_outbox_events_total{result="ok"}[5m])` with the write
   rate. If writes outpace indexing, run more indexer replicas. `SKIP LOCKED`
   lets them share the backlog safely.
4. Repeated `retry` results: check the indexer logs for `last_error`, or
   query the table directly:
   `SELECT aggregate_id, attempts, last_error FROM outbox WHERE processed_at IS NULL ORDER BY id LIMIT 20;`

## OutboxDeadLetters

**Meaning.** Some events failed `INDEXER_MAX_ATTEMPTS` times for reasons
other than an outage, for example a document Elasticsearch rejects.

1. Find them:
   `SELECT id, aggregate_id, last_error FROM outbox WHERE dead_at IS NOT NULL;`
2. Fix the cause. A mapping mismatch usually means a code change.
3. Requeue them:
   `UPDATE outbox SET dead_at = NULL, attempts = 0, next_attempt_at = now() WHERE dead_at IS NOT NULL;`
   Alternatively, rebuild the whole index with `networkai reindex`, which is
   safe to run while the worker is live.

## APIHighErrorRate / APIHighLatency

1. Find the failing route:
   `sum by (route, status) (rate(networkai_http_requests_total{status=~"5.."}[5m]))`.
2. Look up the matching log lines by `request_id`. Every 5xx response body
   carries one.
3. Check PostgreSQL first. Every route except `/posts/search` depends on it,
   and `/readyz` fails when it is unreachable.

## Routine jobs

| Job | How | Frequency |
|---|---|---|
| Purge tombstones older than 7 days | built into the indexer | every 6 h |
| Prune processed outbox rows older than 7 days | built into the indexer | hourly |
| Sweep orphaned media | `networkai sweep-media` (`-dry-run` first) | daily, via cron or a scheduled job |
| Rebuild the search index | `networkai reindex` | after a mapping change, or to repair |
