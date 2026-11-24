# ADR 0002: Idempotent, order-independent indexing with external versions and tombstones

- Status: accepted
- Date: 2026-11-23

## Context

The outbox (ADR 0001) delivers each event **at least once**, and with several
indexer workers, events for the same post can be applied **out of order**:
worker A picks up "post edited" while worker B picks up the later "post
deleted", and B may finish first. A naive indexer would then re-create a
deleted post or overwrite new text with old text.

## Decision

1. **Events carry only the post id, not the data.** The indexer re-reads the
   post's *current* row from PostgreSQL (including soft-deleted rows) and
   indexes that.
2. **Every post row has a `version`**, incremented by each update and delete
   in the same transaction as the change.
3. **Writes use Elasticsearch external versioning**
   (`PUT /posts/_doc/{id}?version=N&version_type=external`). Elasticsearch
   accepts the write only if N is greater than the stored version. Otherwise
   it returns `409 Conflict`, which the indexer treats as success ("a newer
   version is already indexed").
4. **Deletes are soft in PostgreSQL and tombstones in Elasticsearch.** A
   deleted post is indexed as `{deleted: true}` with its new version, and
   every search filters `deleted = false`.

## Why tombstones instead of a real ES delete

A real delete would remove the stored version. A late, stale write (version
2 arriving after the version-3 delete) would then find no document and
succeed, resurrecting the post. Elasticsearch keeps delete tombstones only
for `index.gc_deletes` (60 s by default), so relying on them is a race.
Keeping our own tombstone document makes the version check permanent.

## Consequences

- Replaying any event is a no-op (409 = already applied), so retries are safe.
- Whatever order events arrive in, the index converges to the highest
  version, which is the latest PostgreSQL state. Verified by
  `TestOutOfOrderIndexWritesConverge`.
- Several events for one post in the same batch are coalesced into one
  handler call, since the handler indexes current state anyway.
- Reindex can run concurrently with the live worker, for the same reason.
- Tombstones take index space. A periodic purge of tombstones older than any
  possible in-flight event (e.g. 7 days) is listed as future work.
- The "re-read current state" approach costs one PostgreSQL read per event.
  At higher volume, carrying the full payload plus version in the event would
  avoid it; the versioning rule keeps that equally safe.
