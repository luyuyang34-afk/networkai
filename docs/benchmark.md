# Benchmark

Every number here comes from a run whose command is given below, with no
rounding in our favour. Read the **Environment** and **Caveats** sections
before quoting anything.

## Environment

| | |
|---|---|
| Machine | 1 VM, 4 vCPU (Intel Xeon @ 2.10 GHz), 15 GB RAM |
| Services | docker compose: PostgreSQL 16, Elasticsearch 8.15.3 (single node, 512 MB heap), API, indexer, all on the same VM |
| Load generator | k6 v1.8.3, **on the same VM** (it competes for the same 4 cores) |
| Data | 100,032 posts (synthetic, `make synth load-synth`), 5,000 users, 41,253 likes; index force-merged to one segment |
| Target | the API container's IP directly, not the published `localhost` port (see Caveats) |
| Code | commit `1375f07` (feed optimisations), `FEED_MAX_CONCURRENT=2`, `FEED_LEXICAL=false` |

## Workload

`tools/loadtest/mixed.js`, an **open model** (`constant-arrival-rate`):
requests start at the target rate regardless of how slow responses get, so
queueing shows up as latency instead of being hidden.

| Operation | Share | Request |
|---|---|---|
| browse | 45% | `GET /posts?limit=20` (half also fetch the next cursor page) |
| read | 20% | `GET /posts/{id}` |
| search | 20% | `GET /posts/search?q=<1-2 topic words>` |
| write | 10% | `POST /posts` (JSON, authenticated) |
| foryou | 5% | `GET /feed?limit=20` (personalised, authenticated, users with like history) |

The achieved request rate is ~1.2x the target because of the extra cursor
page fetched by half of the browse iterations.

```bash
make up && make synth load-synth
RATE=1000 DURATION=60s make loadtest
```

## Results (final configuration)

Latency in ms (P50 / P95 / P99), 60 s per stage.

| Target rate | Achieved req/s | browse | read | search | write | foryou | Failed | ES rejections |
|---|---|---|---|---|---|---|---|---|
| 250/s | 300 | 1.3 / 2.0 / 3.7 | 0.8 / 1.2 / 2.7 | 4.0 / 7.8 / 11.7 | 1.6 / 2.8 / 4.9 | 26.7 / 37.7 / 45.3 | 0.00% | 0 |
| 500/s | 598 | 1.4 / 4.0 / 8.9 | 0.8 / 3.0 / 6.8 | 5.4 / 15.5 / 27.2 | 1.7 / 5.0 / 11.4 | 34.9 / 86.7 / 127.2 | 0.00% | 0 |
| 750/s | 899 | 2.0 / 9.6 / 34.2 | 1.1 / 8.7 / 33.0 | 10.1 / 31.7 / 57.3 | 2.4 / 10.8 / 26.0 | 100.3 / 160.1 / 220.6 | 0.56%\* | 0 |
| 1000/s | 1195 | 4.9 / 60.8 / 122.2 | 3.5 / 58.6 / 124.5 | 19.6 / 67.2 / 95.2 | 5.3 / 39.0 / 68.2 | 102.7 / 237.5 / 323.5 | 2.27%\* | 0 |

\* **All failures are `GET /feed` requests shed by the bulkhead**
(`503 feed_busy`). Browse, read, search and write succeeded 100% at every
rate. At 1000/s the feed served 1,343 of 3,011 requests (~22/s): that is its
capacity under this mixed load on this machine.

**Summary for a resume line.** On a single 4-vCPU VM, with the load
generator on the same machine:
- ~1,200 req/s of mixed traffic;
- 0% errors on the core endpoints;
- search P95 67 ms; browse P95 61 ms.

At 600 req/s every endpoint, the feed included, has P95 under 90 ms with
no errors.

## Before/after: the feed starving search

Same workload, same data. "Before" is commit `4cf997b`: parallel retrieval
including `more_like_this`, k=200, vectors shipped for every candidate, 28
segments, no bulkhead.

| At 1000/s target | Before | After |
|---|---|---|
| search P50 / P95 | 4,396 / 5,727 ms | **19.6 / 67.2 ms** |
| browse P95 | 325 ms | **61 ms** |
| foryou P95 | 8,575 ms | **238 ms** (excess shed in ≤100 ms) |
| requests failed | 8.58% (search + feed) | 2.27% (feed only, by design) |
| ES search-pool rejections | 51,138 over the 1000 and 1500/s stages | **0** |

| At 500/s target | Before | After |
|---|---|---|
| search P95 | 67.5 ms | **15.5 ms** |
| foryou P95 | 223.2 ms | **86.7 ms** |

What each change bought, measured in isolation (feed-only load):

| Change | Measurement |
|---|---|
| Slim candidates (no vectors for kNN hits, hydrate only the final page) | idle feed P50 55 → 31 ms; kNN round trip 19.9 → 9.8 ms |
| Drop `more_like_this`, k 200 → 100 | top-20 overlap with the old configuration: 100% (40 users); 1% of served items had come only from MLT |
| Force-merge 28 segments → 1 | ES CPU at 25 feeds/s 137% → 95%; feed-only capacity ~53 → ~69 req/s |
| Bulkhead (2 concurrent) + sequential retrieval | ES rejections at 1000/s: 51k → 0 |

Control experiment, run midway through the investigation (before the force
merge and the MLT change): the same 1000/s with `FEED_SHARE=0` had 0%
failures. That run is what proved the feed caused the collapse.

## Other measured numbers

| What | Result | Where |
|---|---|---|
| Full reindex of 100,030 posts into an empty index | 134.0 s → **4.7 s** (per-document → `_bulk`; runs 4.7/4.8/4.6 s) | engineering-log |
| Blue/green rebuild with embeddings + force merge, 100k posts | 17.2 s, alias swap atomic | `reindex -fresh` |
| Elasticsearch outage of 150 s | 0 events lost, 0 dead-lettered | engineering-log |
| `SearchIndexLagging` alert | pending → firing at ~270 s of outage; resolved after recovery | Prometheus |

## Caveats

- **The load generator shares the 4 vCPUs** with everything it measures.
  These numbers are good for comparing configurations on this machine, not
  for sizing production. A credible capacity figure needs the load
  generator on separate hardware.
- **Docker's userland port proxy is not in the path.** Hitting the published
  `localhost:8080` at 2000 req/s produced 88% `connection refused`. That is
  the proxy failing, not the service: the same load sent to the container IP
  had 0 failures.
- 20 distinct authenticated users generate the authenticated traffic. Nothing
  is cached per user, so this does not flatter the feed numbers. A per-user
  cache would, and is deliberately not part of these results.
- Data is synthetic (short texts from a fixed vocabulary). Real posts are
  longer and would make search and indexing somewhat more expensive.
