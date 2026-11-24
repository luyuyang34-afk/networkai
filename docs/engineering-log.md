# Engineering log

Real problems hit while building this, with how each was found and fixed.
Written down at the time, so nothing has to be reconstructed later.

---

## 2026-11-23 · The personalised feed starved plain search under load

**Symptom.** In the mixed load test (45% browse, 20% read, 20% search,
10% write, 5% personalised feed), the system collapsed at 1000 req/s:
- search P50 rose to **4.5 s**;
- about 9% of all requests failed.

Without the feed, the same rate ran with 0 failures.

**Investigation, step by step:**

1. API logs showed the failures were `503 search_unavailable`, caused by
   Elasticsearch `429`. `_cat/thread_pool/search` showed **51,138
   rejections**: the search thread pool (7 threads, queue 1000) was full.
2. Hypothesis: each feed request runs 3 searches in parallel and occupies
   the pool. I added a **bulkhead** (at most N concurrent feed requests,
   shed the rest fast with `503 feed_busy`) and made retrieval
   **sequential**. Search still collapsed, even with N=1. Hypothesis wrong.
3. I load-tested the feed alone: **25 feeds/s used 137% of an Elasticsearch
   CPU core** (~50 ms of CPU per request), about 3x the queries' `took` time.
   kNN uses concurrent segment search, so a query burns several cores'
   worth of CPU for a single unit of wall time.
4. I timed each phase from the client. Much of the wall time went to
   fetching `_source` with embeddings (400 candidates x 64 floats of JSON),
   not to the searches. **Fix:** candidates carry only id, author and time;
   kNN candidates get their cosine from the score (`2*score - 1`); only the
   final 20 posts are hydrated with text. Idle feed P50 went from 55 ms to
   31 ms.
5. I sampled which retrieval source each served item came from. Only **1%**
   (12 of 1,200) came solely from `more_like_this`, the most expensive
   query. I turned it off by default and lowered kNN `k` from 200 to 100.
   Top-20 overlap with the old configuration was **100%** over 40 users,
   so quality was unchanged.
6. The index had **28 segments** from the bulk load, and kNN walks one HNSW
   graph per segment. A force merge to 1 segment cut feed ES CPU by ~30% and
   raised feed-only capacity from ~53 to ~69 req/s. The rebuild now force
   merges before swapping the alias.

**Outcome and the honest limit.** See [benchmark.md](benchmark.md). The
feed is still the most expensive endpoint by an order of magnitude. On a
4-vCPU machine shared by Elasticsearch, PostgreSQL, the API and the load
generator, it bounds total capacity. The bulkhead keeps a feed overload from
taking search down with it. The real fixes are architectural (see
[bottlenecks.md](bottlenecks.md)).

**Lessons.**
- Measure resource cost (CPU), not just latency; `took` hid most of the
  cost.
- Check the hypothesis with a control experiment before building on it (the
  bulkhead alone did not help).
- Measure feature value before paying for it: 1% unique contribution did not
  justify the most expensive query.

---

## 2026-11-23 · The load test was measuring Docker, not the service

**Symptom.** At 2000 req/s, 88% of requests failed in about 0 ms, yet the
API logged only ~3,300 errors out of ~100k failed requests.

**Cause.** The client error was `dial: connection refused`. The load
generator was hitting `localhost:8080`, which goes through Docker's
userland port proxy, and the proxy refused connections under the burst. The
same 2000 req/s sent straight to the container IP: **0 failures, P95
8.6 ms**.

**Fix.** The k6 script takes `API=http://<container-ip>:8080`, and the
benchmark methodology says so. Lesson: before trusting a capacity number,
confirm the bottleneck is inside the system under test.

---

## 2026-11-23 · Reindex was bound by round trips: 134 s → 4.7 s

Reindexing 100k posts did one PostgreSQL query and one Elasticsearch request
per post, serially: ~750 docs/s. Batched reads plus the `_bulk` API (500 per
request, same external-version semantics) brought it to 4.7 s (~21k docs/s).
Both numbers were measured into an empty index with the same data. The old
binary was rebuilt from git history for a fair A/B.

---

## 2026-11-23 · Adding embeddings: the in-place reindex silently did nothing

After adding the embedding field, a reindex reported success but **0
documents had embeddings**. External versioning rejects writes whose version
is not newer, and post versions had not changed. This is the safety property
from ADR 0002 working as designed, and it means **document-shape changes
need a new index**. `networkai reindex -fresh` now does a blue/green
rebuild:
1. load a new index;
2. force merge it;
3. swap the alias atomically;
4. replay posts changed during the load;
5. drop the old index.

A test makes an edit mid-rebuild and checks it survives.

---

## 2026-11-23 · The load test found an API design gap

The first k6 run had 7.6% failures. Every write was rejected, because k6
sent form-encoded bodies and `POST /posts` accepted only multipart. A text
post shouldn't need multipart: the endpoint now also takes JSON
`{"message"}`.

---

## 2026-11-23 · A long search outage would have dead-lettered every pending event

**Symptom.** While running `make demo-outage`, the indexer logs showed each
pending event reaching retry attempt 4–5 within about two seconds of
Elasticsearch going down:

```
"msg":"outbox event failed, will retry","id":8,...,"attempt":4,"retry_in":3813376235
```

The demo passed only because the outage lasted about 20 seconds.

**Analysis.** Each event has a retry budget (`INDEXER_MAX_ATTEMPTS=10`)
with exponential backoff and full jitter. Summed over the schedule, an
event is dead-lettered after a few minutes of continuous failure. So any
Elasticsearch outage longer than that would push every pending event to
`dead`, and search would silently stop syncing until someone ran
`networkai reindex`. Every event was also logged on every retry, burying
the one useful signal.

The real mistake was treating two different failures the same way:

- *this event is bad*: retry a bit, then quarantine it;
- *the dependency is down*: says nothing about any event, so no event's
  budget should be spent.

**Fix** (commit "Pause the outbox worker during dependency outages…", see
ADR 0003). Errors are now classified. A dependency-unavailable error
(transport error, 5xx, 429):
- stops the batch;
- leaves attempt counters unchanged;
- pauses the whole worker with its own backoff (1 s → 30 s cap);
- logs once at the start of the outage and once at the end.

**Verification.**
- New test `TestUnavailableDependencyPausesWithoutSpendingRetries`, with
  `MaxAttempts=2` and 20 failed probes: 0 dead, attempts still 0.
- The e2e outage test now runs with `MaxAttempts=3` and 10 probes.
- Disabling the fix makes both tests fail (checked).
- A live 150-second Elasticsearch outage against docker compose:

```
after 150s down:   {"pending":3,"dead":0,"oldest_pending_seconds":150.035}
recovered:         {"pending":0,"dead":0,"oldest_pending_seconds":0}
indexer log lines during the outage:
      1 "msg":"dependency unavailable; pausing outbox processing"
      1 "msg":"dependency recovered; resuming outbox processing"
```

**Remaining risk.** If a permanent error were ever misclassified as
"unavailable", the worker would stall on it. The classifier is kept narrow,
and `/readyz` exposes `oldest_pending_seconds`, which should be alerted on.

---

## 2026-11-23 · A test that passed with the key line deleted

**Symptom.** To check that the tests protect what they claim to, I deleted
key lines one at a time and re-ran the tests (mutation testing):
- removing external versioning: 4 e2e tests failed ✅
- removing the outbox insert: 5 tests failed ✅
- changing `FOR UPDATE SKIP LOCKED` to plain `FOR UPDATE`: **all tests passed** ❌

**Analysis.** That result was right, not a broken test. Without
`SKIP LOCKED`, a second worker *waits* for the first one's row locks instead
of skipping to other rows, so every event is still handled exactly once.
`SKIP LOCKED` buys **parallelism, not correctness**. The test only checked
correctness.

**Fix.** The concurrency test now also records how many handler calls are
in flight at once and fails if it never exceeds 1. With plain `FOR UPDATE`
it fails with "workers never ran concurrently (max in flight 1)". With
`SKIP LOCKED` it passed 5 out of 5 runs.

**Lesson.** Before writing a test, decide whether the mechanism under test
exists for correctness or for performance. Then test the property it
actually provides.

---

## 2026-11-23 · Defects found in the original prototype

Found while reading the first two commits. All are fixed, and the ones a
test can express are now covered by one:

| Defect | Impact | Test |
|---|---|---|
| Passwords stored and compared in plaintext in Elasticsearch | Full credential leak on any index exposure | `TestSignupStoresHashNotPassword` |
| Signup regex `^[a-z0-9]$` used as a *deny* rule | Only single-character usernames were rejected; `"Bad Name!<script>"` was accepted | `TestSignupValidation` |
| Dual write: GCS upload, then Elasticsearch write | Orphaned files on failure; deleting a post never removed its file | `TestUploadIsCleanedUpWhenDBWriteFails` |
| Per-object `AllUsers` read ACL | Fails on buckets with uniform access; permissions scattered per object | (config) |
| Media type taken from the file extension | HTML renamed to `.jpg` was accepted | `TestUploadSniffsContent` |
| Unmaintained JWT libraries; no algorithm pinning | Exposure to algorithm-confusion attacks | `TestTokenRejections` (`alg none`) |
| `context.Background()` everywhere, no server timeouts | One slow dependency could hold connections forever | — |
| Search returned every matching document, unpaginated | Response size grows without bound | `TestSearchPaginationAndFilters` |
