# What happens when each part fails

For each component: what users notice, what protects the system, how it
recovers, and the evidence. "Measured" links to the experiment that
tested it. The rest describes behaviour in the code that was not
separately measured.

| Component down | What users notice | What protects the system | Recovery | Evidence |
|---|---|---|---|---|
| **Elasticsearch** | Search returns 503. Posting, browsing and timelines work. | Writes go to PostgreSQL + outbox; the indexer pauses instead of burning retries | Indexing catches up from the log | Measured: 150 s outage, **0 writes lost** ([engineering log](../docs/engineering-log.md)) |
| **Redis** | Home timeline returns 503 (`timeline_unavailable`); "Latest" still works. Single-post reads fall back to PostgreSQL. | Fan-out pauses in its own consumer group; search indexing is unaffected | Timelines are rebuilt from the primary on the next read | Measured: post searchable in **0.73 s** during the outage ([cdc-experiment](../docs/cdc-experiment.md)) |
| **Kafka** | Nothing at first. Search, timelines and stats go stale. | The relay does not confirm WAL positions it could not publish | The relay resumes from the last confirmed position; consumers are idempotent | Alert `ReplicationSlotWALBacklog`: PostgreSQL retains WAL until the relay catches up |
| **CDC relay** | Same as Kafka down | Same | Restart; replays from the confirmed LSN | Tested: restart without loss; a clean stop mid-publish |
| **One consumer group** | Only that group's output goes stale (e.g. search) | Groups have independent offsets; poison messages go to that group's dead-letter topic | The group resumes from its committed offset | Tested: groups are independent; a restart does not skip records (21 of 21) |
| **Read replica** | Reads routed to the replica fail. Token reads that find it behind go to the primary. | — | Restart the replica | **Known gap:** no health-check fallback to the primary yet (ADR 0007) |
| **Primary (single region)** | Writes fail; replica reads continue | — | Restart, or promote the replica | Not drilled in single-region mode |
| **Region partition** | The cut-off region cannot write; EU writes resume after failover | Leader lease: the old primary makes itself read-only; EU promotes only after the lease TTL | Promote the EU replica and flip the VIP | Measured: async **54,803** split-brain writes → fenced **0**; EU outage 5.0 s ([multiregion-experiment](../docs/multiregion-experiment.md)) |
| **One API instance** | Requests on that instance fail; others serve | The API is stateless. A dead lease holder's lease expires in 2 s. | Restart or replace | — |
| **LLM provider** | "Improve with AI" returns a friendly error; nothing else changes | Timeout, one retry, per-user limit | Automatic on the next request | Unit-tested fallbacks (ADR 0004) |
| **Feed overload** | Some "For you" requests get 503 `feed_busy`; the UI falls back to "Latest" | Bulkhead (2 concurrent feed requests) | Automatic | Measured: search stayed at **P50 20 ms** at 1,000 req/s ([benchmark](../docs/benchmark.md)) |
| **Shard move interrupted** | Writes to that one logical shard fail until the move is re-run | The source gives up ownership before the target takes it, so never two owners | Re-run the move; it is idempotent | Tested: 0 of 46,608 writes lost under load ([sharding-experiment](../docs/sharding-experiment.md)) |

The pattern: **only PostgreSQL losing data loses data.** Every other
failure delays derived data or degrades one feature, and recovery is
replay or rebuild.
