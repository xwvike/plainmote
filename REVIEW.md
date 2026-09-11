# 人工 Review 入口

代码和测试是实际行为的依据。这份文档记录当前架构的阅读顺序和 Review 检查点，部署参数以 `README.md` 为准。

## 系统地图

第一遍阅读：

1. `cmd/plainmote/main.go`：进程直接启动 Web 服务。
2. `internal/app/app.go`：读取环境、连接 PostgreSQL 与 S3、组装依赖和管理 HTTP 生命周期。
3. `internal/web/routes.go`：HTTP 路由和 handler 依赖边界。

```text
main
  -> app
       -> environment config
       -> pgxpool / PostgreSQL
       -> S3-compatible object storage
       -> upstream HTTP client
       -> GitHub OAuth client
       -> web handlers
```

## 数据边界

按顺序阅读：

1. `internal/store/schema.sql`：当前 PostgreSQL schema。
2. `internal/store/store.go`：连接池、事务和 Store 生命周期。
3. `internal/store/models.go`：Web 与 Store 之间的数据模型。
4. `internal/blob/blob.go`：对象存储接口。
5. `internal/config/config.go`：环境变量和启动校验。

需要确认的不变量：

- PostgreSQL 是用户、会话、资源元数据、分享规则和访问记录的事实来源。
- 上传正文只保存在 S3/R2，数据库保存对象 Key 和大小。
- 本地资源保存对象 Key，远程资源保存上游 URL。
- 分享属于资源，资源属于 GitHub 用户。
- 分享 Token 以 AES-GCM 密文保存，同时保存 SHA-256 哈希用于查找。
- Web 容器本地没有需要保留的数据。

## 部署和启动边界

按顺序阅读：

1. `compose.yaml`：生产容器边界和环境变量注入。
2. `internal/config/config.go`：环境变量解析、默认值和启动校验。
3. `internal/store/store.go`：`DATABASE_URL` 解析、连接池和启动探活。
4. `internal/store/schema.go`：启动时执行 PostgreSQL schema。

生产环境通过一个连接 URI 同时配置 PostgreSQL 地址、端口、数据库、用户名和密码，例如：

```text
postgres://plainmote:password@postgres.example.com:5432/plainmote?sslmode=require
```

需要确认的运行约束：

- 进程不接收 CLI 子命令或配置文件路径，`app.Run()` 只读取环境变量。
- `DATABASE_URL` 由 `pgxpool` 原生解析；生产 URI 应使用数据库服务商要求的 TLS 模式，并作为 Secret 注入，不能写入镜像、仓库或日志。
- 启动会连接并 `Ping` PostgreSQL，然后执行内嵌 `schema.sql`；任一步失败都会终止进程。
- 当前 schema 使用 `CREATE ... IF NOT EXISTS` 且没有迁移历史，因此应用数据库角色需要首次建表权限。多副本同时冷启动和生产环境是否允许应用持有 DDL 权限，需要在部署时明确决定。
- 所有副本必须共享相同的 `PLAINMOTE_TOKEN_KEY`，否则已有分享 Token 无法解密。
- `/healthz` 只表示 HTTP 进程存活，不检查 PostgreSQL 或 S3/R2；编排平台若需要依赖就绪语义，应单独配置启动或就绪策略。

## 关键链路

### 登录

```text
GET /auth/github
GET /auth/github/callback
  -> internal/auth/github.go
  -> internal/store/users.go
  -> internal/web/session.go
```

重点检查 OAuth state、GitHub ID 白名单、Cookie 安全属性、Session 哈希和 CSRF。

### 资源写入

```text
POST /resources/new
POST /resources/{id}
  -> internal/web/resources.go
  -> internal/store/resources.go
  -> S3/R2
  -> PostgreSQL resources
```

重点检查对象写入与数据库写入的顺序、失败补偿、正文替换、本地/远程资源切换，以及补偿删除失败时的孤儿对象处理策略。

### 分享创建与消费

```text
POST /resources/{id}/share
  -> internal/store/shares.go

GET /d/{token}/{filename}
  -> ConsumeToken transaction
  -> S3 Open or upstream Fetch
  -> access_logs
```

分享次数通过 PostgreSQL 条件 `UPDATE ... RETURNING` 原子扣除。需要明确接受或调整“正文读取前扣除次数”的产品语义。

### 远程资源

```text
resource.origin_url
  -> internal/upstream/client.go
  -> redirect validation
  -> DNS/address validation
  -> response size limit
```

重点检查私网地址策略、每次重定向、DNS 解析后的最终拨号地址、禁用环境 HTTP 代理，以及响应 `Content-Type` 的安全降级和 `nosniff`。

请求头必须保持透明：`Accept` 是 `*/*`，不能写成对文本格式的偏好。那是内容协商，会让上游返回与该地址默认
返回不同的东西——GitHub API 在收到排序过的 Accept 时会把缩进 JSON 压成单行。这条同时影响预览和公开地址，
因为两者共用 `upstream.Fetch`；本服务转发地址提供的内容，不替使用者表达偏好。

### 静态资源与编辑器

```text
GET /static/<name>
  -> internal/web/static.go  staticTypes 白名单
  -> ETag 重新验证
  -> 启动时预压缩，按 Accept-Encoding 分发

internal/web/static/editor.js
  -> internal/web/static/vendor/codemirror.js
  -> tools/codemirror/package.json + package-lock.json
  -> tools/codemirror/build.sh 通过 npm ci 重建
```

需要确认的不变量：

- `/static/` 只服务 `staticTypes` 里列出的文件；类型不靠扩展名推断，目录里多一个文件不等于对外可取。
- 静态资源使用 ETag 和 `Cache-Control: public, no-cache`，浏览器可以复用内容，但每次使用前必须确认
  当前部署仍是同一版本。
- 编辑器是渐进增强。可编辑资源服务端渲染的永远是 `<textarea name="content">`，隐藏它的 `cm-source` 只由
  `editor.js` 在编辑器起来之后添加，任何时候都不能写进模板或样式表。
- 远程资源的只读预览走 `data-preview` 和 `preview()`，不是 `data-editor` 和 `enhance()`。它挂的 textarea
  没有 `name`、带 `disabled`，不进表单；`preview()` 里不能出现任何写回表单或产生待提交字节的逻辑。
- 资源页是双栏：宽栏是内容，窄栏 `.side` 是名称、文件名和元信息。保存横跨两栏放在 `.savebar`，
  因为它提交的是整个 form。`.panel-ed > .pb` 的去内边距由 `:has(> .cm-host)` 把关，
  没跑起编辑器的浏览器必须仍然拿到有内边距的 textarea。
- 加载 `editor.js` 的只有两种页面：可在线编辑的资源，以及上游返回文本、`UpstreamTextPreview` 为真的
  远程资源。非文本资源和取不到上游内容的远程资源都不能下载 CodeMirror bundle。
- 表单提交前由 `editor.js` 把内容写回 textarea；CSRF、字段名和大小校验都不经过 JavaScript。
- 选择文本文件后，编辑器必须立即自动识别编码并显示内容；识别不准时可手动选择编码重新打开。未修改时
  上传和已有资源必须保留原始字节，一旦在编辑器中修改，文件输入会被清空，服务端按当前编码重新写入。
- 浏览器和服务端支持的编码集合必须一致；资源要单独持久化源编码，不能靠 HTTP Content-Type 或文件后缀
  猜测后直接覆盖。无法可靠解码或含二进制控制字符的内容不能进入文本编辑器。
- 行尾是文件属性，不是正文的一部分。`store.DetectEOL` 与 `editor.js` 的 `detectEOL` 必须给出相同结果，
  两边都是「CRLF 数量多于其余换行数量才算 CRLF」，且只产出 `lf` 或 `crlf`。改一边必须改另一边。
- 行尾不入库：每次渲染从字节重新检测。因此没有可迁移的列，也不存在与内容不符的陈旧值。
- 写入前由服务端 `store.ApplyEOL` 按表单的 `content_eol` 统一行尾。这一步不能省：浏览器提交 textarea 时
  一律把换行规范成 CRLF，无论有没有 JavaScript，省掉它会把 LF 文件写成 CRLF。
- 编辑器侧必须用 `view.state.sliceDoc()` 取内容，不能用 `view.state.doc.toString()`。后者硬编码 LF，
  会把整个文件的行尾改掉，包括用户没有碰过的行。行尾通过 `EditorState.lineSeparator` 放在 compartment 里。
- 编辑器语言识别只存在于浏览器层，Store 不依赖 CodeMirror 的语言名称。
- `codemirror.js` 是提交进仓库的编辑器和编码检测构建产物。依赖版本以 `package.json` 和 `package-lock.json` 为准，
  重建必须使用 `npm ci`；`npm --prefix tools/codemirror test` 覆盖浏览器侧的代表性自动识别和解码路径。

## PostgreSQL 检查点

- 所有时间字段使用 `TIMESTAMPTZ`，Go 侧使用 `time.Time`。
- 主键与外键使用 PostgreSQL `UUID`。
- Token 密文使用 `BYTEA`。
- Store 直接使用 `pgxpool`，Web 层只依赖领域错误。
- schema 在应用启动时直接执行，数据库角色权限必须与这一行为匹配。
- 每个测试使用独立 schema，支持包级并行执行。
- 当前没有历史迁移；schema 不兼容时重建快速迭代环境。

## 通用函数检查表

```text
入口：谁调用它？
输入：哪些值来自用户、环境、数据库或上游？
身份：对象归属在哪里校验？
读取：读取了哪些外部状态？
写入：按什么顺序写 PostgreSQL、S3 和响应？
不变量：返回后必须保证什么？
失败：每个失败点留下了什么状态？
并发：多个副本同时执行会发生什么？
日志：成功和失败是否留下足够证据？
测试：哪个真实 PostgreSQL 测试固定了行为？
```
