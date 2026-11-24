# 面试追问清单

每题按"第一层 → 追问 → 再追问"组织。练习方式：合上文档自己讲，讲不出来的那一层就是要补的。答案要点都能在 01–04 和 ADR 里找到。

## A. 一句话介绍（30 秒版本，用英文练）

> "NetworkAI is a Go backend for a social posting app. PostgreSQL is the source of truth, and Elasticsearch is a search index kept in sync through a transactional outbox, so a search outage never blocks or loses a write. I made the indexing idempotent and order-independent with external versioning, and I verified the guarantees with failure-injection tests — including one that found a bug where a long outage would have dead-lettered every event."

## B. 数据一致性

1. **为什么不直接写完 PG 再写 ES？**
   - 追问：失败了重试不就行了？→ 进程崩溃时重试丢失；需要持久化的"待办"
   - 再追问：那 outbox 和直接发 Kafka 有什么区别？→ 发 Kafka 仍是双写
2. **你的投递语义是什么？**（at-least-once）
   - 追问：怎么处理重复？→ 版本号 + external versioning，409 视为成功
   - 再追问：为什么不做 exactly-once？
3. **两个 worker 同时处理同一帖子的不同事件，会不会写乱？**
   - 追问：删除之后迟到的更新会不会让帖子复活？→ 墓碑 + 版本
   - 再追问：为什么不依赖 ES 自己的删除墓碑？→ `gc_deletes` 默认 60 秒
4. **CDC（Debezium）和 outbox 怎么选？**

## C. 并发与数据库

5. **`FOR UPDATE SKIP LOCKED` 是什么？**
   - 追问：去掉 `SKIP LOCKED` 结果还正确吗？→ 正确，但会串行化
   - 再追问：你怎么用测试证明它确实并行了？
6. **为什么用 keyset 分页不用 OFFSET？**
   - 追问：created_at 相同的两条怎么办？→ (created_at, id) 复合排序
   - 再追问：对应的索引怎么建？为什么是部分索引？
7. **ownership 检查为什么要 `SELECT ... FOR UPDATE`？**（check-then-act 竞态）

## D. 可靠性

8. **ES 挂了，你的系统表现如何？**
   - 发帖、浏览正常；搜索 503；outbox 积压；恢复后自动追平
   - 追问：`/readyz` 为什么不检查 ES？→ 否则局部故障变成全局故障
9. **讲一个你遇到的 bug。**（→ [engineering log](../engineering-log.md) 第一条：长时间宕机会把所有事件打进死信）
   - 追问：你怎么发现的？→ 跑故障演示时看日志
   - 再追问：怎么修的？为什么这样修？有什么风险？→ 错误分类器误判会导致卡住；用 `oldest_pending_seconds` 监控
10. **重试为什么要加抖动（jitter）？**
11. **怎么知道积压了？线上该报什么警？**

## E. 搜索

12. **"runs" 为什么能搜到 "Running"？**（analyzer、stemming）
    - 追问：那精确匹配的结果怎么排得更前？→ multi-field，`message.exact^2`
13. **must 和 filter 的区别？**
14. **深分页怎么做？**（search_after + 唯一决胜字段）
15. **mapping 要改字段类型怎么办？**（alias + 新索引 + reindex + 切换）
16. **BM25 大概怎么算分？**

## F. 安全

17. **密码怎么存？** → bcrypt；追问 72 字节、cost、盐
18. **登录接口怎么防用户名枚举？** → 相同错误 + 相同耗时
19. **JWT 有什么风险？怎么防 `alg: none`？怎么吊销？**
20. **上传文件怎么防伪装？**

## G. 工程实践

21. **你怎么确认测试真的有效？** → 变异测试：故意删掉关键代码，确认测试会挂
22. **集成测试怎么隔离？** → 每个测试独立数据库 + 独立 ES 索引，可并行
23. **如果流量涨 10 倍，先改哪里？**（读副本、CDC、批量写 ES 的 `_bulk`、outbox 分区……说出瓶颈在哪，比说出方案更重要）
24. **这个项目最大的不足是什么？**（README 的 Known limitations；能主动说出边界是加分项）

## 红线：不要说的话

- 不要说任何没有测过的数字（"P95 20ms"、"支持 1 万 QPS"）。目前**没有做压测**，就说"还没有做压测，下一步计划用 k6……"。
- 不要说"exactly-once"，说"at-least-once + 幂等"。
- 被问到不会的，说"我没做过这个，但我会这样考虑……"，比编造好得多。
