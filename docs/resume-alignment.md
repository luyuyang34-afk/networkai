# 简历 ↔ 代码对齐表

原则：**简历上的每一句话，都要能在 repo 里指出对应的代码、测试或数据。** 指不出来的就不写，或者换成能指出来的说法。

状态：✅ 已实现并有证据　🟡 部分实现　❌ 未实现

## Bullet 1 · Social Product & API Integration — ❌

> Built a modular social publishing and discovery interface using Next.js, React, TypeScript, and shadcn/ui…

- 前端还没有开始做。后端 API 已经准备好：CORS 白名单默认放行 `http://localhost:3000`，错误格式统一，`X-Request-ID` 已暴露给前端。
- 在前端做出来之前，**这一条不能写**。

## Bullet 2 · Search Architecture — ✅（措辞需要微调）

> Maintained canonical post records in PostgreSQL while indexing searchable text and metadata in Elasticsearch; implemented an outbox-driven asynchronous indexing worker with retry and deduplication to reconcile failed updates, and tuned analyzers, filters, and relevance scoring to reduce search P95 latency from [X] ms to [X] ms across [X] indexed posts.

| 说法 | 证据 |
|---|---|
| PG 保存权威数据 | `internal/db/migrations/0001_init.sql`、ADR 0001 |
| outbox 驱动的异步索引 worker | `internal/outbox/worker.go` |
| retry | 指数退避 + 死信 + 依赖宕机时暂停（ADR 0003） |
| deduplication | 外部版本号让重放无害，同批事件合并（ADR 0002） |
| tuned analyzers, filters, relevance scoring | 自定义 English analyzer、`message.exact^2`、filter 子句（`search.go`） |
| **P95 从 X 降到 Y** | ❌ **没有数据。**我们也从没有过一个"慢的版本"可以对比 |

建议改写成下面这样（每个词都有出处）：

> **Search Architecture:** Kept canonical post records in **PostgreSQL** and synced a derived **Elasticsearch** index through a **transactional outbox** and asynchronous indexing worker. Made indexing idempotent and order-independent with **external versioning** and tombstones, so retries, duplicates and out-of-order deliveries converge. Verified with failure-injection tests that a **150-second search outage lost zero writes**, and tuned analyzers (stemming, ASCII folding), filters and field boosts for relevance.

"150 秒零丢失"是实测数字，有 engineering log 记录，可以写。如果之后要写 P95，需要先做 k6 压测，并且写清楚测试方法。

## Bullet 3 · Personalized Feed Ranking — ❌

> Built a two-stage recommendation pipeline… improved offline nDCG@10 by [X]% over [X] held-out user interactions.

- 现在只有按时间倒序的 feed（`GET /posts`），这正好可以作为 baseline。
- 前提是要有**交互数据**：点赞、浏览等表还没有建。需要找一个公开数据集，或者老老实实地生成模拟数据，并在 README 里写明数据来源。
- 这是工作量最大、风险最高的一条，排在压测之后做。

## Bullet 4 · Backend Reliability & Security — 🟡

> Implemented Go API endpoints with JWT verification, ownership-based authorization checks, pagination, and consistent structured error responses; added load tests and failure-path integration tests to sustain [X] RPS with P95 API latency below [X] ms.

| 说法 | 状态 |
|---|---|
| JWT verification | ✅ 固定算法、强制 exp 和 issuer，`TestTokenRejections` 覆盖 |
| ownership authorization | ✅ 带行锁的检查，403/404 区分清楚 |
| pagination | ✅ keyset（PG）+ search_after（ES） |
| consistent structured errors | ✅ 统一的 `{error:{code,message},request_id}` |
| failure-path integration tests | ✅ ES 宕机、事务回滚、上传失败补偿、并发 worker |
| **load tests, X RPS, P95 < X ms** | ❌ 还没有压测 |

在压测做完之前，去掉 RPS 和 P95 的部分，可以把"race detector"、"mutation-checked"这类真实做过的事写进去。

## Bullet 5 · Generative AI Assistance — ❌

- 还没有实现。之前讨论过可以把这一条换成 feed fan-out 或 Redis，以避免和 AI Document Q&A 项目重复。这个决定还没定。

## 技能栏

简历里出现的 Elasticsearch、PostgreSQL、Docker、Go、JWT，现在都有对应代码。GCS 有代码，但本地默认用的是磁盘存储，没有真正部署过，被问到时要如实说。
