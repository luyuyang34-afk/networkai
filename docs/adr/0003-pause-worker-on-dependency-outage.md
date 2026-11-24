# ADR 0003: Pause the worker during dependency outages instead of spending retries

- Status: accepted
- Date: 2026-11-23

## Context

The outbox worker retries a failed event with exponential backoff and
dead-letters it after `INDEXER_MAX_ATTEMPTS` (default 10). While running the
outage demo we saw every pending event reach attempt 4–5 within about two
seconds of Elasticsearch going down. With the default schedule, an outage of
a few minutes would dead-letter **every** pending event. Search would stop
syncing silently, and recovery would need a manual reindex.

The retry budget exists to quarantine *bad events* (for example, a document
Elasticsearch rejects). It was being spent on an *unavailable dependency*,
which says nothing about any individual event.

## Decision

The worker takes an `IsUnavailable(error) bool` classifier. The indexer
reports connection errors, HTTP 5xx and HTTP 429 from Elasticsearch as
unavailable. When a handler returns such an error:

- processing stops for the rest of the batch;
- the event's `attempts` counter is **not** incremented (only `last_error`
  is recorded);
- the worker pauses as a whole, with its own exponential backoff (1 s, 2 s,
  4 s, … capped at 30 s), probing with one event per attempt;
- the outage is logged once when it starts and once when it ends, instead of
  once per event per retry.

Other errors keep the per-event retry, backoff and dead-letter behaviour.

This is a simple circuit breaker: "open" while the dependency fails,
"half-open" on each probe, "closed" on the first success.

## Consequences

- An Elasticsearch outage of any length loses nothing and dead-letters
  nothing. Verified by `TestUnavailableDependencyPausesWithoutSpendingRetries`
  and by a 150-second live outage against docker compose (see the
  engineering log).
- Recovery latency after an outage is at most `MaxPause` (30 s) plus drain
  time.
- Misclassification risk: if a permanent error were reported as
  "unavailable", the worker would stall on it forever. The classifier is
  therefore narrow (transport errors, 5xx, 429), and the outbox stats on
  `/readyz` (`pending`, `oldest_pending_seconds`) make a stall visible. An
  alert on `oldest_pending_seconds` is the natural next step.
