# 01 · 双写问题与 Transactional Outbox

## 问题：两个系统，没有共同的事务

帖子要存进 PostgreSQL（真相），也要存进 Elasticsearch（才能被搜到）。最直觉的写法：

```go
db.InsertPost(p)   // ①
es.Index(p)        // ②
```

会出什么事？

| 情况 | 结果 |
|---|---|
| ① 成功，② 网络抖动失败 | PG 有，ES 没有 → **帖子永远搜不到** |
| ① 成功，进程在 ①② 之间被 kill（部署、OOM） | 同上，而且连错误日志都没有 |
| 反过来先写 ES 再写 PG，PG 失败 | ES 里有一个**不存在的帖子**，用户点进去 404 |
| 两个请求同时改同一个帖子 | ① 的顺序和 ② 的顺序可能不一样 → ES 里留下旧版本 |

本质原因：**数据库事务只能管一个数据库**。跨两个系统没有原子性。这叫 dual-write problem（双写问题），是分布式系统面试的高频题。

> 你可能会想"失败了就重试 ES 呗"。但重试逻辑在进程内存里，进程一死重试就没了。要可靠，"需要同步这件事"本身必须**持久化**，并且和数据变更**一起**持久化。

## 解法：把"要通知 ES"这件事也写进 PG，放在同一个事务里

```sql
BEGIN;
  INSERT INTO posts  (...);
  INSERT INTO outbox (aggregate_id, event_type) VALUES ($post_id, 'post.changed');
COMMIT;
```

同一个数据库里的两张表，事务保证它们**同生共死**：
- 提交了 → 帖子在，"待同步"的记录也在，迟早会被处理
- 回滚了 → 两个都不在，ES 也不需要知道

然后另一个进程（`indexer`）专门去"清空收件箱"：

```
loop:
  取一批未处理的 outbox 记录（加锁）
  对每条：同步到 ES
    成功 → 标记 processed_at
    失败 → attempts+1，过一会儿再试
```

代码：`internal/store/store.go`（写端）、`internal/outbox/worker.go`（读端）。

### 证明它有效的测试

- `TestFailedPostWriteLeavesNoOutboxEvent`：故意插入一条违反 CHECK 约束的帖子（5001 字），断言 outbox 里也没有记录 → 回滚是一起的。
- `TestElasticsearchOutageLosesNoWrites`：ES 挂掉期间发 20 个帖子、编辑一个、删除一个，恢复后搜索结果完全正确。

## 几个关键细节（面试会追问的）

### 1. 投递语义：at-least-once（至少一次）

worker 是"先处理，再标记"。如果 ES 写成功了，但在 `UPDATE outbox SET processed_at` 之前进程挂了，下次启动会**再处理一遍**。

- 反过来"先标记再处理"就是 at-most-once：可能丢。
- exactly-once（恰好一次）在跨系统场景基本做不到，工业界的做法是 **at-least-once + 幂等处理** = 效果上的 exactly-once。幂等怎么做见 [02](02-idempotency.md)。

### 2. `FOR UPDATE SKIP LOCKED`：让多个 worker 并行

```sql
SELECT ... FROM outbox WHERE ... ORDER BY id LIMIT 100 FOR UPDATE SKIP LOCKED
```

- `FOR UPDATE`：把选中的行锁住，别的事务不能同时处理它们。
- `SKIP LOCKED`：遇到别人锁着的行**直接跳过**，去拿下一批没被锁的。

如果只有 `FOR UPDATE` 没有 `SKIP LOCKED`：结果**依然正确**（第二个 worker 会等第一个释放锁），但 worker 们会排队，加机器没用。测试 `TestConcurrentWorkersClaimDisjointEvents` 同时检查了"每条只处理一次"（正确性）和"确实并行了"（吞吐量）。这是个很好的面试点：**有些机制是为了正确性，有些是为了性能，要分清。**

### 3. 重试、退避、死信

- **指数退避 + 抖动**（`DefaultBackoff`）：第 n 次失败后等 `random(0, 0.5s × 2^n)`。抖动（jitter）是为了避免大量失败事件在同一时刻一起重试（惊群效应）。
- **死信**（dead letter）：重试 `MaxAttempts` 次还失败，就标记 `dead_at`，不再自动重试，等人工处理。防止一条坏数据永远堵在那里。
- **毒丸 / panic**：handler panic 了不能把整个 worker 搞挂，`handleOne` 里有 `recover`。

### 4. 依赖整体宕机 ≠ 单条事件有问题（我们真踩过的坑）

最初的实现里，ES 一挂，**每一条**事件都在快速消耗自己的重试次数。几分钟的宕机就会把所有事件打进死信，搜索从此悄悄地不再同步。

修复：区分两种失败。
- 这条事件本身坏了 → 按单条重试、最终死信
- ES 整个连不上 → **整个 worker 暂停**（1s、2s、4s… 最多 30s 再探测一次），不扣任何事件的重试次数

这其实就是一个简化版的**熔断器（circuit breaker）**。详见 ADR 0003 和 engineering log。

### 5. 代价是什么？

- **最终一致性**：搜索会比数据库晚一点（轮询 0.5s + ES refresh 1s）。所以"刚发的帖子立刻去搜"可能搜不到，但浏览（读 PG）一定能看到。
- **轮询开销**：每 0.5s 一次查询。用部分索引 `outbox_pending_idx`（只索引未处理的行）让它很便宜。
- outbox 表会增长 → 定期清理 7 天前已处理的行（`Prune`）。

## 还有哪些替代方案？

| 方案 | 一句话 |
|---|---|
| CDC（Debezium 读 PG 的 WAL 日志 → Kafka） | 不用改业务代码、不用轮询，大公司常用；但要运维 Kafka 全家桶。我们的 outbox 表正好是 Debezium "outbox router" 能直接消费的格式，将来可以平滑迁移。 |
| `LISTEN/NOTIFY` | PG 自带的通知，延迟低；但没人在听时通知就丢了，所以只能作为"唤醒信号"配合 outbox 表用。 |
| 消息队列（直接发 Kafka） | 又回到双写问题：写 PG 和发 Kafka 也不在一个事务里。outbox 就是为了解决这个而存在的。 |

## 自测

1. 如果 indexer 进程挂了一整天，会发生什么？恢复后呢？
2. 为什么 `/readyz` 故意不检查 ES？
3. 为什么事件里只放帖子 id，不放帖子内容？（答案在 02）
