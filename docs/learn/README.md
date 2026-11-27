# 学习指南：NetworkAI 是怎么工作的

这组笔记写给"要能在面试里把这个项目讲透"的人。建议按顺序读，每读完一篇就去跑对应的测试、改一行代码看它失败。

| 顺序 | 文档 | 核心问题 |
|---|---|---|
| 0 | 本页 | 代码地图：一个请求从进来到落库到能被搜到，经过哪些文件 |
| 1 | [01-outbox.md](01-outbox.md) | 为什么不能"写完 PG 再写 ES"？outbox 怎么解决？ |
| 2 | [02-idempotency.md](02-idempotency.md) | 消息会重复、会乱序，怎么保证结果还是对的？ |
| 3 | [03-elasticsearch.md](03-elasticsearch.md) | 倒排索引、analyzer、mapping、相关性打分、深分页 |
| 4 | [04-auth-security.md](04-auth-security.md) | bcrypt、JWT、认证 vs 授权、几个真实的安全坑 |
| 5 | [05-ranking.md](05-ranking.md) | 推荐系统：两阶段召回+排序、离线评估怎么不作弊、合成数据怎么讲 |
| 6 | [06-performance.md](06-performance.md) | 压测方法、找瓶颈的套路、bulkhead/批量化/缓存这些模式 |
| 7 | [07-scale-out.md](07-scale-out.md) | 规模变大以后：扩散、CDC、读副本、热点、分片、跨区域、SLO |
| 8 | [../system-design.md](../system-design.md) | 整个项目作为"设计一个 news feed"的系统设计题答案（英文） |
| 9 | [interview-questions.md](interview-questions.md) | 考点总览 + 面试追问清单（每题追三层） |
| 10 | [../interview-prep.md](../interview-prep.md) | 英文版：最重要的 5 个设计题 + 最难的 5 个问题（面试直接用） |

还有一份英文的 [engineering log](../engineering-log.md)，记录了做这个项目时真实踩到的坑。"讲一个你遇到的最难的 bug"可以直接从这里讲。

---

## 先跑起来（10 分钟）

```bash
make up             # 起 postgres / elasticsearch / api / indexer
make seed           # 灌示例数据，并演示两次搜索
make demo-outage    # 故障注入：关掉 ES，继续发帖，恢复后一条不丢
make test-integration
```

跑完 `make demo-outage` 之后，再打开 `scripts/demo-es-outage.sh` 对照着看每一步在验证什么。

---

## 代码地图：跟着一次"发帖"走一遍

```
POST /posts  (multipart: message + media_file)
│
├─ internal/httpapi/middleware.go
│    requestID → recoverer → accessLog → cors   每个请求都过这几层
│    requireAuth: 解析 Bearer token → 把 Principal{UserID, Username} 放进 context
│
├─ internal/httpapi/posts.go  createPost()
│    1. MaxBytesReader 限制上传大小
│    2. 读文件前 512 字节 → media.Kind() 嗅探真实类型（不信扩展名）
│    3. 先把文件存进 media.Store（本地磁盘或 GCS）
│    4. 再调 store.CreatePost()；如果这一步失败 → 删掉刚上传的文件（补偿操作）
│
├─ internal/store/store.go  CreatePost()
│    BEGIN
│      INSERT INTO posts ...
│      INSERT INTO outbox (aggregate_id = post.id, event_type = 'post.changed')
│    COMMIT                       ← 两行要么都在，要么都不在
│
│   ……HTTP 201 已经返回给用户了。下面是异步的：
│
├─ internal/outbox/worker.go  ProcessBatch()           （indexer 进程里，每 500ms 一次）
│    SELECT ... FROM outbox WHERE 未处理 AND 到期
│      ORDER BY id LIMIT 100 FOR UPDATE SKIP LOCKED
│    对每个事件调 handler；成功 → processed_at = now()
│                         失败 → attempts+1，按指数退避安排下次重试
│                         ES 整体挂了 → 整个 worker 暂停，不扣重试次数
│
├─ internal/indexer/indexer.go  IndexPost()
│    从 PG 重新读这条帖子的"当前状态"（包括已软删除的）
│    → search.Upsert(doc, version = post.version)
│
└─ internal/search/search.go  Upsert()
     PUT /posts/_doc/{id}?version=N&version_type=external
     ES 只接受比已存版本更大的 N；否则 409 → 当作成功（已经有更新的了）
```

搜索的路径就短得多：`GET /posts/search` → `httpapi.searchPosts` → `search.Client.Search` → ES。浏览（`GET /posts`）则直接查 PG，所以 ES 挂了也能刷。

---

## 分层：为什么这么拆

| 层 | 目录 | 只关心 | 不关心 |
|---|---|---|---|
| HTTP | `internal/httpapi` | 参数校验、鉴权、把错误映射成状态码 | SQL 怎么写 |
| 数据 | `internal/store` | SQL、事务、约束 | HTTP |
| 同步 | `internal/outbox`、`internal/indexer` | 可靠投递、重试 | 用户请求 |
| 外部系统 | `internal/search`、`internal/media` | 和 ES / 存储通信 | 业务规则 |

好处体现在测试上：`httpapi` 的单元测试用假的 store（`httpapi_test.go` 里的 `fakeStore`），不需要数据库，毫秒级跑完；`store` 和 `e2e` 的集成测试才连真库。

## 建议的动手练习

1. 在 `store.CreatePost` 里删掉 `enqueuePostChanged` 那一行，跑 `make test-integration`，看哪些测试挂了、报错怎么说。（这就是"变异测试"：确认测试真的在保护你以为它保护的东西。）
2. 把 `search.go` 里的 `version_type=external` 去掉，再跑一次。
3. 读 `TestConcurrentWorkersClaimDisjointEvents` 的注释，然后把 `SKIP LOCKED` 去掉跑一次。想想为什么去掉之后"结果依然正确，但测试会挂"。
4. 手动复现旧 bug：用 `curl` 注册一个用户名叫 `"Bad Name!"` 的用户，看现在返回什么。再看 `users.go` 里关于原来正则的注释。
