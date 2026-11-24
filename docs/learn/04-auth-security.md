# 04 · 认证、授权与几个真实的安全坑

对应代码：`internal/auth/auth.go`、`internal/httpapi/users.go`、`internal/httpapi/middleware.go`。

## 认证 vs 授权（面试必问）

- **认证（Authentication）**：你是谁？→ 登录、验证 JWT。失败返回 **401**。
- **授权（Authorization）**：你能不能做这件事？→ 这个帖子是不是你的。失败返回 **403**。

本项目里：`requireAuth` 中间件做认证；`store.mutateOwnedPost` 里 `SELECT author_id ... FOR UPDATE` 再比较，做授权。`FOR UPDATE` 是为了让"检查归属"和"执行修改"之间不会被别的事务插队（check-then-act 竞态）。

## 密码：bcrypt

原版代码把**明文密码**存进 ES，登录时用 term 查询比对明文。数据库一泄露，所有用户密码直接暴露（而且很多人多个网站用同一个密码）。

现在用 bcrypt：
- **单向哈希**：存的是哈希值，不能反推。
- **自带盐（salt）**：同样的密码每次哈希结果都不同，彩虹表无效。
- **故意很慢**（cost=12，约几百毫秒）：攻击者暴力破解的成本跟着暴涨。cost 每 +1，时间翻倍。
- **72 字节上限**：bcrypt 只看前 72 字节。我们直接拒绝更长的密码，而不是悄悄截断（否则两个前 72 字节相同的密码都能登录）。

### 时序攻击：用户名枚举

如果"用户不存在"时立刻返回、"密码错误"时要算 bcrypt（几百毫秒），攻击者测一下响应时间就知道哪些用户名存在。所以用户不存在时，我们也拿一个假哈希跑一遍 bcrypt（`CheckPassword` 里的 `dummyHash`），并且两种情况返回完全相同的错误码和信息。测试：`TestLoginDoesNotRevealWhetherUserExists`。

## JWT

结构：`header.payload.signature`，三段 base64。payload 里是 claims：

```json
{"sub": "42", "username": "alice", "iss": "networkai", "iat": ..., "exp": ...}
```

服务端用密钥对前两段签名。验签通过就信任 payload，**不用查数据库、不用存 session** → 无状态，好横向扩展。

### 我们做了哪些防护

| 防护 | 防的是什么 |
|---|---|
| `WithValidMethods(["HS256"])` | **alg 混淆攻击**：攻击者把 header 改成 `"alg":"none"`，有的库就不验签了。我们固定算法，绝不让 token 自己说用什么算法。测试里专门造了一个 `alg: none` 的 token。 |
| `WithExpirationRequired()` | 没有 exp 的 token = 永久有效，拒绝 |
| `WithIssuer` | 防止别的服务签发的 token 被拿来用 |
| `JWT_SECRET` ≥ 32 字节，未设置拒绝启动 | 弱密钥可以被离线暴力破解 |
| 换掉 `form3tech-oss/jwt-go`、`auth0/go-jwt-middleware v1` | 旧库已停止维护 |

### JWT 的代价（面试会问 JWT vs Session）

- **无法主动吊销**：token 在过期前一直有效。用户改密码、被封号，旧 token 还能用。常见方案：短有效期 access token + refresh token；或维护一个吊销黑名单（但这又引入了状态）。
- Session（QuickBite 用的）正好相反：服务端存状态，可以随时吊销，但要共享存储（如 Redis）才能多实例。

## 其他修掉的坑

| 原问题 | 后果 | 现在 |
|---|---|---|
| 注册正则 `^[a-z0-9]$` 且"匹配则拒绝" | 只拦截单字符用户名，`"Bad Name!<script>"` 能注册 | `^[a-z0-9_]{3,20}$` 且"不匹配则拒绝"，数据库也有同样的 CHECK 约束（纵深防御） |
| 按扩展名判断文件类型 | 把 HTML 改名成 `.jpg` 上传 → 存储型 XSS 风险 | 读前 512 字节嗅探真实类型（`media.Kind`） |
| GCS 对象逐个设成 `AllUsers` 可读 | 新 bucket 默认禁止这种 ACL；权限散落在每个对象上 | bucket 级权限（uniform bucket-level access） |
| CORS `AllowedOrigins: *` | 任意网站都能调 API | 白名单，可配置 |
| 内部错误原样返回给客户端 | 泄露 SQL、主机名等内部信息 | 统一返回 `internal`，真实错误只进日志（带 request_id 可以对上） |
| 上传无大小限制 | 一个大文件就能拖垮服务 | `MaxBytesReader`，默认 50MB |

## 自测

1. 为什么 PATCH 别人的帖子返回 403，而 PATCH 不存在的帖子返回 404？有没有理由两种都返回 404？（提示：防止探测资源是否存在）
2. JWT 放 localStorage 和放 httpOnly cookie 各有什么风险？
3. bcrypt 的 cost 设成 20 会怎样？
