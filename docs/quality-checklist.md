# 项目质量清单

每一项打勾都要附上证据在哪里。没有证据的勾不算数。

## 打开 repo 的前 2 分钟

- [x] 一条命令跑起来：`make up` → `make seed`；CI 里 `docker compose up --build` 已经跑通
- [x] README 有架构图、截图、关键数字表、文档索引
- [x] CI 全绿：Go 的格式检查、vet、race 检测、单元 + 集成 + e2e 测试；Docker 镜像构建；前端 lint、类型检查、构建；全栈 Playwright 测试
- [x] 代码通过 gofmt 和 eslint
- [ ] demo 视频或 GIF（录下 `make demo-outage` 的终端，以及前端点赞、For you 的操作过程）
- [ ] 线上可访问的 demo（被云账号卡住，见 bottlenecks.md 第 4 条）

## 证明"真懂"

- [x] 故障注入：ES 宕机 150 秒零丢失、告警完整走过 pending → firing → resolved
- [x] 关键性质都有测试，并做过变异测试（6 处）
- [x] ADR 0001–0004 + ranking.md
- [x] Known limitations 写在 README 里
- [x] 有方法论的压测：开放模型、阶梯加压、环境说明、优化前后对比、对照实验（benchmark.md）
- [x] Prometheus 指标 + 4 条告警规则 + runbook
- [x] 离线评估：按时间切分、消融实验、理论上限、冷启动切片（ranking-eval.md）
- [ ] 真实数据集上的排序评估（被数据集卡住，见 bottlenecks.md 第 1 条）
- [ ] LLM 的真实延迟和成本数据（被 API key 卡住，见 bottlenecks.md 第 3 条）
- [ ] 压测机和服务分开部署，拿到可以对外引用的容量数字

## 面试时能讲出来的东西

- [x] Git 历史：每个提交都说明了"为什么"，原型代码保留在前两个提交里
- [x] 真实的 debug 故事，engineering-log 里有 8 条，最好的是"Feed 拖垮搜索"那一整条排查链
- [x] 追问清单 + 6 篇学习笔记
- [ ] 至少 2 轮模拟面试

## 去教程痕迹

- [x] 原型结构已经全部替换，README 也如实说明了项目历史

## 简历对齐

见 [resume-alignment.md](resume-alignment.md)。
