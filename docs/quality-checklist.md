# 项目质量清单

每完成一项就打勾，并写上证据在哪里。没有证据的勾不算。

## 打开 repo 的前 2 分钟

- [x] 一条命令跑起来：`make up` → `make seed`（compose 编排、seed、outage demo 均已实测跑通；Dockerfile 的编译阶段因开发环境限制网络，需要在 CI 或本机上确认一次）
- [x] README 顶部有架构图（mermaid，GitHub 会直接渲染）
- [x] CI：gofmt、vet、race 检测、单元 + 集成测试（真实 PG + ES service container）、docker build → `.github/workflows/ci.yml`
- [ ] CI 第一次在 GitHub 上实际跑绿（推送后确认 badge）
- [x] 代码 gofmt 干净
- [ ] demo 视频 / GIF（录 `make demo-outage` 的终端，约 40 秒）
- [ ] 线上可访问的 demo

## 证明"真懂"

- [x] 故障注入 demo：`make demo-outage`，另有 150 秒长宕机实测（engineering log）
- [x] 关键性质有测试，且做过变异测试确认测试有效（README "Testing" 一节）
- [x] ADR：0001 outbox、0002 幂等/乱序、0003 宕机暂停
- [x] Known limitations / 规模扩大时怎么办（README）
- [ ] 有方法论的 benchmark：k6 压测，写清硬件、数据量、并发数、P50/P95/P99，脚本可复现
- [ ] 指标监控：Prometheus 指标 + outbox 积压告警

## 面试时能讲出来的东西

- [x] Git 历史是一系列有意义的提交，提交信息说明"为什么"
- [x] 真实 debug 故事：`docs/engineering-log.md`（长宕机打进死信；SKIP LOCKED 变异测试）
- [x] 追问清单：`docs/learn/interview-questions.md`
- [ ] 模拟面试至少 2 轮（我可以扮演面试官）
- [x] 简历上每个数字在 repo 里都有出处 → 目前**没有任何性能数字**，压测前不要写

## 去教程痕迹

- [x] 原型的结构（handler/service/backend、olivere/elastic、form3tech jwt）已全部替换
- [x] README "Project history" 坦诚说明前两个提交是原型

## 简历对齐

见 [resume-alignment.md](resume-alignment.md)。
