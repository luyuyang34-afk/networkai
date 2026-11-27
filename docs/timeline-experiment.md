# Experiment: the celebrity problem in home timelines

Reproduce with `go run ./tools/experiments/fanout -followers 50000` (needs
PostgreSQL and Redis; it creates its own throwaway database).

## Setup

- One "celebrity" with N followers, **all with an active cached timeline**,
  which is the worst case for fan-out.
- 2,000 regular authors; each follower follows 20 random ones.
- One worker processes events in order, as the outbox worker does: the
  celebrity posts, then a regular author posts right after.
- Timeline reads: 300 random followers, page size 20.
- Same machine as [benchmark.md](benchmark.md): 4 vCPU, everything local.

## Results

| Celebrity followers | Mode | Redis writes for one celebrity post | Time to fan it out | Regular post waits behind it | Timeline read P50 / P95 |
|---|---|---|---|---|---|
| 10,000 | Pure fan-out on write | 10,000 | 46 ms | 49 ms | 0.5 / 0.7 ms |
| 10,000 | Hybrid | **0** | 0 | **2.3 ms** | 0.7 / 0.9 ms |
| 50,000 | Pure fan-out on write | 50,000 | 298 ms | 304 ms | 1.5 / 2.4 ms |
| 50,000 | Hybrid | **0** | 0 | **4.6 ms** | 1.5 / 2.4 ms |
| 200,000 | Pure fan-out on write | 200,000 | **1.26 s** | **1.28 s** | 1.4 / 2.2 ms |
| 200,000 | Hybrid | **0** | 0 | **14 ms** | 1.5 / 2.3 ms |

In hybrid mode, the regular post's wait is just its own fan-out to its
~100–2,000 followers.

## What it shows

1. **Pure push cost is linear in follower count**: about 6 µs per follower
   here, so 200k followers cost 1.26 s for a single post. Extrapolating, an
   account with 5 million followers would take about 30 s per post on this
   machine. That figure is an extrapolation, not a measurement.
2. **Head-of-line blocking.** While a celebrity's post is being fanned out,
   every event behind it waits. That includes a regular user's post and,
   because the same worker also indexes search, that post's search update.
   This is the coupling the CDC + message queue change removes.
3. **The hybrid removes the write cost entirely**: 0 writes for celebrity
   posts. **It costs readers almost nothing**: P95 changed by at most 0.2 ms,
   because pulling recent posts from a handful of followed celebrities is one
   indexed PostgreSQL query.

## Bugs found while building the experiment

Recorded because they are the kind of mistakes that make benchmarks lie.

- **The first design was wrong.** Every follower followed the same 20
  "regular" authors, so those authors also had 50k followers. In hybrid mode
  they counted as celebrities too, and the hybrid looked better than it is.
  Fixed: 2,000 authors with 20 random follows each.
- **The measurement recorded the wrong post.** The fan-out callback was
  overwritten by the regular post processed after the celebrity's, so the
  first table showed the regular post's 510 writes as the celebrity's.
  Fixed by recording the celebrity's numbers before handling the next post.
- **Seeding took 10+ minutes.** Computing follower counts with a correlated
  subquery on freshly loaded, un-analyzed tables made the planner scan all
  1.05 M follow rows once per user. `ANALYZE` plus a single GROUP BY made it
  take seconds.
