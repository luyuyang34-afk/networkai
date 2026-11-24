# Engineering log

Real problems hit while building this, with how each was found and fixed.
Written down at the time, so nothing has to be reconstructed later.

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
