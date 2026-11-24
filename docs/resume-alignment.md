# 简历 ↔ 代码对齐表（第二轮）

原则不变：**简历上的每一句话，都要能在 repo 里指出代码、测试或实测数据。** 下面每条都给出"可以直接用的英文 bullet"，只用了真实测过的数字。

状态：✅ 有证据　🟡 部分　❌ 没有

---

## Bullet 1 · Social Product & API Integration — ✅

原稿要求：Next.js、React、TypeScript、shadcn/ui；注册、发帖、feed、编辑；集中式的请求中间件。

| 说法 | 证据 |
|---|---|
| Next.js + React + TypeScript | `web/`，App Router |
| shadcn/ui | 🟡 **按 shadcn 的模式手写的组件**（cva + tailwind-merge + Radix Slot），因为 shadcn 的组件仓库连不上。面试时如实说："shadcn-style primitives"，或者以后在能联网的环境里用 `npx shadcn add` 替换 |
| 注册、发帖、Feed、编辑 | 全部完成，另外还有删除、点赞、搜索、For you |
| 集中式请求中间件 | `web/src/lib/api.ts`：统一附加 JWT、解析错误、401 时自动登出 |
| 测试 | Playwright 8 个流程，CI 中跑 |

建议的写法：
> **Social Product & API Integration:** Built a **Next.js/React/TypeScript** client with **TanStack Query** (cursor-based infinite scroll, optimistic likes and deletes, targeted cache updates) over **Go REST APIs**, with one typed request layer for JWT auth, error mapping and session expiry; covered 8 user flows with **Playwright** end-to-end tests in CI.

## Bullet 2 · Search Architecture — ✅

> **Search Architecture:** Kept canonical posts in **PostgreSQL** and synced **Elasticsearch** through a **transactional outbox** and async indexer with external versioning, so retries and out-of-order events converge. Failure-injection tests verified a **150-second search outage lost zero writes**. Cut full reindex time **28× (134 s → 4.7 s)** with batched reads and the `_bulk` API, and added zero-downtime **blue/green index rebuilds** with atomic alias swaps.

原稿写的是"把 search P95 从 X ms 降到 Y ms"。我们没有一个"慢的搜索版本"可以拿来对比，所以不写这个。可以改用真实测得的数字：混合负载下搜索 P95 为 67ms（1000 req/s）。

## Bullet 3 · Personalized Feed Ranking — ✅（数据是合成的，必须说清楚）

> **Personalized Feed Ranking:** Built a two-stage recommender: **kNN retrieval** over post embeddings in Elasticsearch, then a logistic-regression ranker on freshness, engagement, semantic similarity and author affinity, with an author-diversity pass. Improved offline **nDCG@10 by 14%** over a chronological baseline on **300K held-out impressions** (time-split evaluation with feature ablations).

⚠️ 面试时必须主动说明，这是诚信底线：
- 数据是**合成的**（真实数据集在开发环境里下载不了）；
- 生成器的结构和模型特征是一致的，所以 14% 代表"流程正确、模型能学到信号"，**不能当成真实用户上的提升**；
- embedding 用的是 LSA（TF-IDF + SVD），不是神经网络模型。

如果想让这条更硬：放行一个真实数据集（见 [bottlenecks.md](bottlenecks.md) 第 1 条），重新跑 `make train`，就能得到真实数据上的数字。

## Bullet 4 · Backend Reliability & Security — ✅

> **Backend Reliability & Security:** Built **Go** APIs with pinned-algorithm **JWT** auth, row-locked ownership checks, keyset pagination and a uniform error contract. Load-tested with **k6** at **~1,200 req/s** with zero errors on core endpoints, and fixed a feed-induced Elasticsearch thread-pool collapse (search P50 **4.4 s → 20 ms**) using payload slimming, query pruning, segment merging and a **bulkhead**.

数字的出处：[benchmark.md](benchmark.md)。必须能说清楚的前提：
- 单台 4 vCPU 机器；
- 压测工具 k6 和被测服务在同一台机器上。

## Bullet 5 · Generative AI Assistance — 🟡

| 说法 | 状态 |
|---|---|
| 接入 OpenAI API、prompt 模板、超时、token 计量、优雅降级 | ✅ 代码和测试都有（`internal/assist`，ADR 0004） |
| `[X]% successful completions`、`$[X] average cost` | ❌ **开发环境连不上 OpenAI，也没有 key，没有真实数据** |

在拿到真实数据之前，可以先这样写（不带数字）：
> **Generative AI Assistance:** Integrated an **OpenAI-compatible LLM** into the post editor behind timeouts, a single retry for 429/5xx, per-user rate limits, input/output caps, prompt-injection delimiting and graceful fallback; instrumented **token usage, latency and cost per request** in Prometheus.

配置好 key 之后用 k6 或手动跑几百次，就能从 `/metrics` 拿到成功率和平均成本，再把数字补回去。

---

## 去重：三个项目之间

之前讨论过 NetworkAI 的 GenAI 这条和 Doc Q&A 项目有重叠。现在 NetworkAI 的分量来自 Bullet 2、3、4。**如果简历版面紧张，建议删掉 Bullet 5**，或者压缩成半行。

## 技能栏核对

| 技能 | 本项目是否用到 |
|---|---|
| Go, SQL, TypeScript | ✅ |
| Next.js, TanStack Query, Tailwind | ✅ |
| shadcn/ui | 🟡 手写的同类组件 |
| PostgreSQL, Elasticsearch, Docker | ✅ |
| Python | ✅（训练和评估脚本，只用了 numpy 和 pandas） |
| Prometheus, k6 | ✅ 可以考虑加进技能栏 |
| Kotlin, Jetpack Compose, LangChain | ❌ 三个项目都没有用到，建议从技能栏删掉 |
