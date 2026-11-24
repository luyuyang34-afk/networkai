# 03 · Elasticsearch 实战要点

对应代码：`internal/search/search.go`（mapping、analyzer、查询、分页全在这一个文件里）。

## 1. 倒排索引：为什么搜索快

数据库按"文档 → 内容"存。搜索引擎反过来，存"词 → 出现在哪些文档"：

```
"run"      → [post1, post5]
"coffee"   → [post2, post6]
"mountain" → [post4]
```

搜 "coffee" 直接查表，不用扫描每篇帖子。这张表就是**倒排索引（inverted index）**。

## 2. Analyzer：文字怎么变成"词"

写入和查询时，文本都会过一遍 analyzer。我们自定义的 `post_text`：

```
"Running at the Café's!"
  standard tokenizer   → [Running, at, the, Café's]
  english_possessive   → [Running, at, the, Café]
  lowercase            → [running, at, the, café]
  asciifolding         → [running, at, the, cafe]
  english_stop         → [running, cafe]           去掉 at/the 这类停用词
  english_stemmer      → [run, cafe]               词干还原
```

效果（`make seed` 里能看到）：
- 搜 "runs" → 命中 "Running ..." 和 "run along ..."（都变成了 `run`）
- 搜 "cafe" → 命中 "Café review"

**关键：写入和查询必须用同一个 analyzer**，否则词对不上。

### 为什么还有个 `message.exact` 子字段？

词干还原提高召回（recall），但会损失精度：搜 "running" 时，"running" 原词的帖子应该比只有 "run" 的排在前面。所以同一段文本再存一份**不做词干**的版本，查询时两个字段都查，`exact` 权重 ×2（`"fields": ["message", "message.exact^2"]`）。这是很常见的 multi-field 技巧。

## 3. Mapping：字段类型决定能怎么查

| 字段 | 类型 | 为什么 |
|---|---|---|
| `message` | `text` | 要分词、全文搜索 |
| `author`、`media_type`、`id` | `keyword` | 整体精确匹配、过滤、排序 |
| `media_url` | `keyword`, `index: false` | 只存不搜，省空间 |
| `created_at` | `date` | 排序、范围查询 |
| `deleted` | `boolean` | 墓碑过滤 |

`"dynamic": "strict"`：文档里出现 mapping 没定义的字段就拒绝写入。默认（dynamic: true）会自动加字段，时间长了 mapping 会膨胀甚至出现类型冲突（mapping explosion）。

**Alias**：应用读写的是别名 `posts`，它指向物理索引 `posts_v1`。将来要改 mapping（mapping 大多不能原地修改），就建 `posts_v2` → reindex → 原子地切换 alias，零停机。

## 4. 查询：must vs filter

```json
{"bool": {
  "must":   [{"multi_match": {"query": "coffee", "fields": ["message","message.exact^2"], "operator": "and"}}],
  "filter": [{"term": {"deleted": false}}, {"term": {"author": "bob"}}]
}}
```

- `must`：参与**相关性打分**（BM25）。
- `filter`：只做是/否筛选，**不打分**，而且结果可以被缓存，更快。
- 原则：影响排序的放 must，纯条件放 filter。

`operator: and`：查询里的每个词都必须出现（"coffee review" 不会召回只含 "coffee" 的帖子）。这是精度和召回之间的取舍。

### BM25 一句话版

一个词对一篇文档的贡献 ≈ **这个词在文档里出现得多（TF，但有饱和上限）** × **这个词在全部文档里稀有（IDF）** ÷ **文档长度归一化**。"the" 到处都有，IDF 低，几乎不加分；"kayaking" 稀有，命中就加很多分。

## 5. 深分页：为什么不用 from/size

`from=10000&size=20` 意味着 ES 要在每个分片上排出前 10020 条，再丢掉前 10000 条，越往后越慢（默认最多 10000 条）。

我们用 **`search_after`**：记住上一页最后一条的排序值，下一页从它之后开始。代价和第 1 页一样。

关键细节：排序必须以**唯一字段**结尾（我们的 `id`），否则分数相同的结果在翻页时可能重复或丢失。cursor 就是把最后一条的 sort 值 base64 编码后交给客户端，对客户端是不透明的。

PG 那边的 `GET /posts` 用的是同样的思想，叫 **keyset pagination**：`WHERE (created_at, id) < ($1, $2) ORDER BY created_at DESC, id DESC`。测试 `TestListPostsKeysetPagination` 特意把很多帖子的时间戳改成一样，验证 id 作为决胜字段确实生效。

## 6. 近实时（near real-time）

写进 ES 的文档要等下一次 **refresh**（默认 1 秒）才能被搜到。所以测试和 demo 里会显式调 `_refresh` 或者等一下。生产环境不要每次写都 refresh，很伤性能。

## 自测

1. 如果把 `author` 定义成 `text` 类型，`author=bob_k` 的过滤会出什么问题？
2. 为什么 `deleted=false` 放在 filter 而不是 must？
3. 想支持中文搜索，analyzer 要怎么改？（提示：`smartcn` 或 `ik` 分词插件）
