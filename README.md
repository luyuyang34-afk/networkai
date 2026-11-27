# NetworkAI

[![CI](https://github.com/polo2444172276/networkai/actions/workflows/ci.yml/badge.svg)](https://github.com/polo2444172276/networkai/actions/workflows/ci.yml)

A social posting and discovery app, with a Go backend and a Next.js
frontend. Users post text, images and video, browse a chronological feed,
get a personalised "For you" feed, search, like posts, and can ask an LLM to
polish a draft.

The engineering focus is the data path and how it behaves under failure and
load:
- **PostgreSQL is the source of truth.** Elasticsearch is a derived index
  kept in sync by a **transactional outbox**, so a search outage loses no
  writes.
- **The personalised feed** is a two-stage recommender: kNN retrieval on
  post embeddings, then a learned ranker, evaluated offline with nDCG@10.
- Every claim below has a test, a script, or a measured number behind it.

<img src="docs/img/profile.png" alt="Profile page" width="520">

```mermaid
flowchart LR
    W[Next.js web] -->|REST + JWT<br/>X-Consistency-Token| API[Go API]
    API -->|"1 round trip: post + outbox row"| PG[(PostgreSQL primary<br/>source of truth)]
    API -->|reads, unless token is ahead| RP[(streaming replica)]
    PG -.->|WAL| RP
    API -->|hot posts: leases + XFetch<br/>home timelines| R[(Redis)]
    API -->|search, feed retrieval| ES[(Elasticsearch)]
    API -.->|rewrite draft| LLM[LLM API]
    PG -->|logical decoding| RL[CDC relay]
    RL -->|post-events, 12 partitions| K[[Kafka / Redpanda]]
    K --> C1[search-indexer] --> ES
    K --> C2[timeline-fanout] --> R
    K --> C3[post-stats]
    K --> C4[cache-invalidation] --> R
    P[Prometheus<br/>SLO burn-rate alerts] -.->|scrape| API & RL & C1
```

## Key numbers

| | Result | Evidence |
|---|---|---|
| Search outage | 150 s outage: **0 writes lost, 0 events dead-lettered**; alert fired at ~270 s and resolved after recovery | [engineering log](docs/engineering-log.md), `TestElasticsearchOutageLosesNoWrites` |
| Throughput (one 4-vCPU VM, load generator included) | ~1,200 req/s mixed traffic, **0% errors on core endpoints**, search P95 67 ms | [benchmark](docs/benchmark.md) |
| Feed overload fix | at 1000 req/s: search P50 **4.4 s → 20 ms**, ES rejections 51k → 0 | [benchmark](docs/benchmark.md#beforeafter-the-feed-starving-search) |
| Full reindex, 100k posts | **134 s → 4.7 s** (`_bulk`, A/B on the same data) | [engineering log](docs/engineering-log.md) |
| Feed ranking (offline, **synthetic data**) | nDCG@10 **+14.0%** vs chronological on 300k held-out impressions | [ranking eval](docs/ranking-eval.md), [why synthetic](docs/ranking.md#the-data-is-synthetic-and-this-is-what-that-means) |
| Tests | Go: unit + integration + e2e with the race detector (74% coverage); Playwright: 8 UI flows; CI runs all of it | [CI](.github/workflows/ci.yml) |

## Scaling out: each problem reproduced, fixed and measured

Each row started as a failure reproduced on purpose, at the scale where it
appears. Each fix is switchable, so the before/after comes from the same
code and data.

| Problem at scale | Fix | Before → after | Evidence |
|---|---|---|---|
| A celebrity post written to every follower's timeline | Hybrid fan-out: push for most authors, pull for authors above 10k followers | 200k followers: a regular post waits **1.28 s → 14 ms** behind it | [timeline-experiment](docs/timeline-experiment.md), [ADR 0005](docs/adr/0005-hybrid-fanout-home-timeline.md) |
| One slow job (fan-out, Redis outage) stalls search indexing | CDC from the WAL, Kafka, one consumer group per job | Redis down: search **stalled > 60 s → 0.73 s** | [cdc-experiment](docs/cdc-experiment.md), [ADR 0006](docs/adr/0006-cdc-and-consumer-groups.md) |
| A user's own post missing right after posting (replica lag) | LSN consistency tokens | **31% → 0%** violations; browsing still 100% on the replica | [replica-experiment](docs/replica-experiment.md), [ADR 0007](docs/adr/0007-read-replicas-consistency-tokens.md) |
| Likes on a viral post queue on one row lock | 16-way sharded counters | **932 → 3,410** likes/s on one post, exact counts | [hotkey-experiment](docs/hotkey-experiment.md) |
| Cache stampede and stale sets on a hot post (32 instances) | singleflight + leases (Facebook memcache) + XFetch | DB loads **1,192/s → 4/s**, stale reads **29.5% → 0%** | [hotkey-experiment](docs/hotkey-experiment.md), [ADR 0008](docs/adr/0008-hot-keys-sharded-counters-and-cache-leases.md) |
| One primary is not enough: resharding 2 → 4 nodes under load | Logical shards, node-side ownership, xmin-horizon copy, per-shard fence | Naive move: **98 posts lost**; fenced: **0 lost of 46,608**, write block P50 46 ms | [sharding-experiment](docs/sharding-experiment.md), [ADR 0009](docs/adr/0009-sharding-and-online-resharding.md) |
| Creating a post from another region (70 ms RTT) | One CTE statement, token pipelined | 6 round trips → 1: **429 → 73 ms** | [multiregion-experiment](docs/multiregion-experiment.md) |
| Region partition followed by failover | Leader lease + self-fencing; sync replication where RPO must be 0 | Async: **54,803 split-brain writes**; fenced: **0**; sync: 0 lost, but +72 ms per commit | [multiregion-experiment](docs/multiregion-experiment.md), [ADR 0010](docs/adr/0010-multi-region-home-region-writes-and-failover.md) |
| Alerts that page on blips and miss slow leaks | SLOs with multiwindow burn-rate alerts | 6 scenarios unit-tested with promtool in CI | [slo.md](docs/slo.md) |

What these experiments do **not** show is throughput scaling. All of
them ran on one 4-vCPU machine, so extra nodes share the same CPUs. They
measure correctness, latency and the cost of each mechanism.

## Quick start

Requires Docker, plus `curl` and `jq` for the scripts. k6 is needed for the
load test.

```bash
make up             # postgres, elasticsearch, api (:8080), indexer
make seed           # a few demo users and posts
make demo-outage    # stop Elasticsearch, keep posting, recover, lose nothing
cd web && npm ci && npm run dev    # http://localhost:3000

# Optional: 100k synthetic posts, embeddings, trained ranker, load test
make synth train load-synth
RATE=500 make loadtest
docker compose --profile observability up -d   # Prometheus on :9090
```

## API

| Method | Path | Auth | Notes |
|---|---|---|---|
| `POST` | `/users`, `/auth/login` | – | bcrypt; JWT (HS256, pinned) |
| `POST` | `/posts` | JWT | JSON `{"message"}` or multipart with `media_file` (type sniffed) |
| `GET` | `/posts?author=&cursor=` | optional | chronological, from PostgreSQL, keyset pagination |
| `GET` | `/posts/search?q=&author=&type=&cursor=` | optional | Elasticsearch, `search_after` pagination |
| `GET` / `PATCH` / `DELETE` | `/posts/{id}` | owner for writes | soft delete |
| `POST` / `DELETE` | `/posts/{id}/like` | JWT | idempotent |
| `GET` | `/feed` | JWT | personalised; each item explains its score, features and sources |
| `POST` | `/assist/rewrite` | JWT | `{"mode": "polish\|shorten\|hashtags", "draft"}` |
| `GET` | `/healthz`, `/readyz`, `/metrics` | – | `/readyz` includes outbox lag |

Errors always look like `{"error":{"code","message"},"request_id"}`.

## Design decisions

| ADR | Decision |
|---|---|
| [0001](docs/adr/0001-postgres-source-of-truth-with-outbox.md) | PostgreSQL as source of truth; transactional outbox instead of dual writes or CDC |
| [0002](docs/adr/0002-idempotent-order-independent-indexing.md) | Events carry ids; indexer re-reads the row; external versions + tombstones make replays and reordering harmless |
| [0003](docs/adr/0003-pause-worker-on-dependency-outage.md) | A dependency outage pauses the worker instead of burning per-event retries (bug found in the outage demo) |
| [0004](docs/adr/0004-llm-writing-assistant.md) | The LLM is an unreliable, metered dependency: timeout, one retry, per-user limit, token/cost accounting, injection mitigation, graceful fallback |
| [0005](docs/adr/0005-hybrid-fanout-home-timeline.md) | Hybrid fan-out home timeline in Redis (push regular authors, pull celebrities) |
| [0006](docs/adr/0006-cdc-and-consumer-groups.md) | CDC from the WAL to Kafka; one consumer group per downstream job |
| [0007](docs/adr/0007-read-replicas-consistency-tokens.md) | Read replicas with LSN consistency tokens for read-your-writes |
| [0008](docs/adr/0008-hot-keys-sharded-counters-and-cache-leases.md) | Sharded like counters; post cache with singleflight, leases, XFetch, CDC invalidation |
| [0009](docs/adr/0009-sharding-and-online-resharding.md) | Posts sharded by author over 64 logical shards; fenced online moves |
| [0010](docs/adr/0010-multi-region-home-region-writes-and-failover.md) | One-round-trip writes across regions; fenced failover; replication mode per data type |
| [ranking.md](docs/ranking.md) | Two-stage feed: kNN retrieval → logistic-regression ranker → author diversity; bulkhead so the feed cannot starve search |

## Documentation

- [benchmark.md](docs/benchmark.md): load-test method, results, before/after, caveats
- [engineering-log.md](docs/engineering-log.md): every real bug and bottleneck, how it was found, fixed and verified
- [bottlenecks.md](docs/bottlenecks.md): open environment blockers and remaining limits (Chinese)
- [system-design.md](docs/system-design.md): the project as a "design a news feed" interview answer, with numbers
- [slo.md](docs/slo.md): objectives, burn-rate alerts and error budget policy
- [runbook.md](docs/runbook.md): what to do when each alert fires
- [ranking.md](docs/ranking.md) / [ranking-eval.md](docs/ranking-eval.md): feed design and generated offline evaluation
- [docs/learn/](docs/learn/README.md): study notes and an interview question bank (Chinese)

## Testing

```bash
make test               # Go unit tests (integration tests skip without databases)
make test-integration   # + PostgreSQL / Elasticsearch integration and e2e tests
make web-test           # frontend lint, typecheck, build, Playwright
```

Key properties are checked by tests that were **mutation-checked**: we
deliberately broke the code and confirmed the test fails. The broken
versions were:
- no external versioning;
- no outbox insert;
- no `SKIP LOCKED`;
- no outage pause;
- no rebuild catch-up;
- a TF formula that drifts from the Python trainer.

## Known limitations

- **The feed is the most expensive endpoint by ~10x.** With 2 concurrent
  slots it serves ~22 req/s under mixed load on this VM and sheds the rest
  (the UI falls back to Latest). Next steps:
  - cache each user's candidates;
  - precompute feeds for active users;
  - isolate vector search on its own Elasticsearch capacity.
- **The ranking evaluation uses synthetic data.** No public dataset was
  reachable from the build environment. The pipeline accepts a real one
  unchanged.
- **The LLM assistant's real latency and cost are unmeasured.** No provider
  was reachable from the build environment; the instrumentation is in
  place.
- **The sharded store is not yet behind the API.** It is a measured
  subsystem (ADR 0009). Moving the API onto it means:
  - co-locating likes and outbox rows with their posts;
  - one CDC relay per node;
  - replacing the user join.
- **Multi-region is simulated on one machine** with a latency proxy. Real
  regions add jitter, asymmetric routes and partial partitions.
- **Benchmarks ran with the load generator on the same 4 vCPUs** as the
  services.

## Project history

The first two commits are the original prototype: Go, Elasticsearch as the
only datastore, plaintext passwords, and public-ACL GCS objects. Everything
after them rebuilds it. Each commit message explains what was wrong and why
it changed.

## Layout

```
cmd/networkai/        api | relay | consume <group> | indexer | migrate | reindex [-fresh] | sweep-media
cmd/wanproxy/         TCP proxy simulating a cross-region link (delay, partition, VIP flip)
internal/httpapi/     routing, middleware, handlers, error mapping
internal/store/       PostgreSQL; every post write also writes the outbox
internal/outbox/      outbox worker: claiming, retries, dead-letter, pause
internal/cdc/         WAL -> Kafka relay, consumer groups with DLQ
internal/timeline/    hybrid fan-out home timelines (Redis)
internal/postcache/   hot-key cache: singleflight, leases, XFetch
internal/shard/       sharded posts: router, ownership, online mover
internal/stats/       idempotent post-stats consumer
internal/indexer/     events -> Elasticsearch, bulk reindex, blue/green rebuild
internal/search/      Elasticsearch client: mapping, analyzers, search, kNN
internal/feed/        two-stage personalised feed + bulkhead
internal/embed/       LSA embeddings (parity-tested against the trainer)
internal/assist/      LLM writing assistant
internal/metrics/     Prometheus metrics
internal/auth, media, maintenance, db, e2e
web/                  Next.js frontend + Playwright tests
tools/                synthetic data, ranking trainer/evaluator, k6 load test
tools/experiments/    fanout, coupling, replica, hotkey, hotpost, reshard, wan, failover
models/               trained embedding + ranker (small JSON files)
deploy/prometheus/    scrape config, alert rules, SLO rules + promtool tests
```
