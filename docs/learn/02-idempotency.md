# 02 · 幂等、乱序与版本号

上一篇的结论：outbox 是 **at-least-once**，消息可能**重复**；有多个 worker 时还可能**乱序**。这一篇讲怎么让结果依然正确。

## 两个概念

- **幂等（idempotent）**：同一个操作做一次和做 N 次，结果一样。`x = 5` 幂等，`x = x + 1` 不幂等。
- **顺序无关（order-independent）**：不管消息按什么顺序到，最终状态一样。

## 会出错的场景

```
时间 →
PG:       v1 创建 "hello"     v2 编辑 "hi"      v3 删除
outbox:   e1                  e2                e3

worker A 拿到 e2 ──────────(很慢)──────────────→ 写 ES "hi"
worker B 拿到 e3 ──→ 删除 ES 文档
```

最终 ES 里是 "hi"——一个已经被删除的帖子复活了。

## 我们的三板斧

### ① 事件里只放 id，handler 回 PG 读"当前状态"

```go
// internal/indexer/indexer.go
p, _ := store.GetPostForIndex(ctx, id)   // 读当前行（含软删除）
es.Upsert(ToDoc(p), p.Version)
```

不管是哪个事件触发的，写进 ES 的都是**此刻 PG 里最新的样子**。重复处理同一事件 → 写同样的内容 → 天然幂等。顺带的好处：同一批里同一个帖子的多个事件只需处理一次（coalescing）。

但只有这一招还不够：A 读到 v2 之后很慢，B 读到 v3 写完了，A 才写 v2 → 还是覆盖了。所以需要 ②。

### ② 版本号 + ES external versioning

- `posts.version`：每次更新/删除时 `version = version + 1`，和数据变更在同一事务里。
- 写 ES 时：`PUT /posts/_doc/{id}?version=3&version_type=external`
- ES 的规则：**只有新版本号 > 已存版本号才接受**，否则返回 `409 Conflict`。
- 我们把 409 当成功处理："已经有更新的版本了，我这条可以丢掉"。

于是 A 迟到的 v2 写入会被 ES 拒绝。不管到达顺序如何，ES 最终存的一定是最大版本 = 最新状态。

这个思路叫**乐观并发控制（optimistic concurrency control）**，和数据库里的 `UPDATE ... WHERE version = ?`、HTTP 的 `ETag / If-Match` 是一个家族。

### ③ 删除用"墓碑"，不真删

如果删除时真的从 ES 删掉文档，版本号也就跟着没了。之后迟到的 v2 写入一看"没有这个文档" → 直接创建成功 → 复活。

（ES 自己会保留删除墓碑，但默认只保留 60 秒 `index.gc_deletes`，赌这个就是竞态。）

所以我们：
- PG 里软删除（`deleted_at`，并且 `version + 1`）
- ES 里写一个 `{"deleted": true}` 的文档，带着新版本号
- 所有搜索都加过滤 `deleted = false`

版本号永久留在 ES 里，任何旧写入都会被拒绝。

## 测试

`internal/e2e/e2e_test.go` 的 `TestOutOfOrderIndexWritesConverge`：先写 v3（删除），再写 v1、v2、再重复写 v3，断言只有第一次生效，且删除的帖子搜不出来。

动手：把 `search.go` 里的 `version_type=external` 改掉，跑测试，看四个 e2e 测试一起挂掉。

## 代价

- 每个事件多一次 PG 读。量大时可以把完整数据 + 版本号放进事件里，版本规则依然保证安全。
- 墓碑占空间，需要定期清理很老的墓碑（未实现，列在 README 的 Known limitations）。

## 迁移到别的场景

同样的思路在 QuickBite（点餐项目）里叫**幂等键（idempotency key）**：客户端给每次下单一个唯一 key，服务端用唯一约束拒绝重复。本质都是"让重复的请求变成无害的"。

## 自测

1. 如果 PG 里 `version` 不是在同一个事务里 +1，会出什么问题？
2. 为什么 409 要当作成功，而不是错误？如果当作错误会怎样？
3. 能不能用 `updated_at` 时间戳代替 version？（提示：时钟精度、时钟回拨、同一微秒内两次更新）
