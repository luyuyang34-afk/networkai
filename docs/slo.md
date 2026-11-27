# Service level objectives

An SLO states how reliable the service must be, as users experience it.
The rest of the budget is room for risk: deploys, migrations, experiments.
Alerts fire on **how fast the error budget is being spent**, not on raw
thresholds. That is the multiwindow, multi-burn-rate method from the
Google SRE Workbook (chapter 5).

Rules: [deploy/prometheus/slo.yml](../deploy/prometheus/slo.yml).
Tests: [deploy/prometheus/slo_test.yml](../deploy/prometheus/slo_test.yml),
run by `make check-alerts` and in CI.

## The objectives

| SLO | SLI (what is measured) | Target, 30 days | Error budget |
|---|---|---|---|
| <a id="api-availability"></a>**API availability** | Share of API requests that do not return 5xx. Health checks are excluded. | 99.9% | 43 minutes of full outage, or 0.1% of requests |
| <a id="read-latency"></a>**Read latency** | Share of `GET /posts`, `GET /posts/{id}`, `GET /timeline`, `GET /users/{username}` answered in under 250 ms. | 99% | 1% of reads may be slower |
| **Search freshness** | A new post is searchable within 2 minutes. | alerting only (`SearchIndexLagging`) | — |

Why these numbers:

- **250 ms.** The load test measured read P99 at 6.8 ms at 600 req/s and
  124.5 ms at 1,200 req/s, with the machine saturated
  ([benchmark.md](benchmark.md)). 250 ms leaves headroom over the worst
  measured P99, and is still faster than people perceive as a delay.
- **99.9% and not 99.99%.** A single primary database with fenced
  failover has an RTO of about 5 s per event
  ([multiregion-experiment.md](multiregion-experiment.md)). A few
  failovers, deploys and dependency incidents a month fit in 43 minutes;
  they do not fit in 4.
- **Freshness is not an SLO with a budget yet.** Lag is a gauge, not a
  per-event count. The right SLI is "share of posts indexed within X",
  which needs a per-event latency histogram in the consumer.

## The alerts

| Alert | Fires when | Severity | Meaning |
|---|---|---|---|
| `…BudgetBurnFast` | 1h burn > 14.4x **and** 5m burn > 14.4x | page | 2% of the month's budget gone in an hour; at this rate it is all gone in about 2 days |
| | 6h burn > 6x **and** 30m burn > 6x | page | 5% gone in 6 hours |
| `…BudgetBurnSlow` | 1d burn > 3x **and** 2h burn > 3x | ticket | 10% gone in a day |
| | 3d burn > 1x **and** 6h burn > 1x | ticket | on track to miss the month |

"Burn rate" is the error ratio divided by the budget. Burn 1 means "the
budget runs out exactly at the end of 30 days".

- **Why two windows per condition.** The long window gives significance:
  a single bad minute does not page. The short window makes the alert stop
  soon after the problem is fixed, instead of an hour later.

Tested behaviour (all in `slo_test.yml`):

| Scenario | Page | Ticket |
|---|---|---|
| 2% errors (20x burn) | fires within 70 min | — |
| 0.05% errors for 3 days (inside the SLO); also a failing health check | silent | silent |
| 0.2% errors for 3 days (2x burn) | silent | fires |
| 2% errors for 1 h, then fixed | fires at 55 min, **clears by 95 min** | stays (budget was spent) |
| 20% of reads slower than 250 ms | fires | — |
| 5% of reads slower than 250 ms (5x burn) | silent | fires |

A mistake caught by these tests: the first version of the latency test
expected a page for 5% slow reads. That is a 5x burn, below both page
thresholds, so the rule was right and the test was wrong. Writing the
scenario down made the threshold math concrete.

## Error budget policy

- **Budget left:** ship normally; risky migrations (resharding, failover
  drills) are allowed.
- **Budget spent:** only reliability fixes ship until the 30-day window
  recovers. The ticket alert is the signal to look before that happens.
- **One incident used more than 25% of the budget:** postmortem with
  action items, whatever the remaining budget.

## How the earlier experiments map to the budget

| Event | Measured cost | Budget impact (99.9% / 30 d) |
|---|---|---|
| Fenced region failover | 5.0 s of EU write outage | about 0.2% of the budget per event |
| Online move of one logical shard | writes to 1/64 of data blocked P50 46 ms; 0 errors | ≈ 0 |
| Elasticsearch outage (ADR 0001) | 0 API errors; search stale | 0 (freshness only) |
| Feed overload at 1,200 req/s | 2.27% of requests shed (`503 feed_busy`) | **would burn the budget in hours**: the feed needs its own SLO or capacity |

The last row is the useful one. The bulkhead protected search and browse,
but under an availability SLO that counts every 5xx, shedding the feed is
still spending budget. Two fixes are possible: a separate, looser feed SLO
(it is a non-critical, degradable feature), or serving a cached
non-personalised feed instead of a 503.
