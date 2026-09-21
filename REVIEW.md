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
- `/` 是首页，对登录与否呈现同一个页面；账号的资源列表在 `/resources/`。首页按登录状态变身
  会让爬虫和用户看到两个东西，这是有意避开的。
- 首页不加载 `editor.js`，也不加载 CodeMirror bundle。那个框就是一个 `textarea`。
- `PLAINMOTE_ANONYMOUS` 默认 `false`，注册模式缺省为 `allowlist`，两项都失败关闭。
  关闭时 `/paste` 不注册进 mux——端点不存在，而不是存在但拒绝；`/` 也不渲染页面，
  未登录跳 `/login`，已登录跳 `/resources/`。首页的可索引例外和 `robots.txt` 的 `Allow: /$`
  都跟着这个开关走：`/` 只有在真是一个页面时才值得被找到。
- `POST /paste` 没有 CSRF token（没有会话可挂），改用 `Origin` 同源校验；缺少 `Origin` 的请求
  （curl、老浏览器）放行，因为那不是被驱动的浏览器该有的样子。
- 服务整体不可索引，**首页除外**。响应头的例外和 `htmlhead` 里 meta 的例外必须同时成立，
  只改一边等于首页自己把自己摘出索引。
- `robots.txt` 只放行 `/$`，其余 Disallow，`X-Robots-Tag: noindex, nofollow, noarchive`
  由中间件加在每个响应上。两者分工不同：robots.txt 只对主动来问的爬虫有效，响应头覆盖的是
  别人把分享地址贴到公开位置的情况。`noarchive` 和 `noindex` 同等重要——缓存副本会比撤销的链接活得久。
- 交付不依赖记录：`RecordAccess` 失败只写 stderr，资源照发。Token 在此之前已经消耗，
  此时拒绝交付既送不出资源，也补不回那行记录。
- 进入 `access_logs` 的事件必须可归属。无人签发的 Token 和格式不对的地址不写库——
  没有 owner 就没有读者，而匿名者可以无限制造。它们由 `web.probeLog` 计数，每分钟最多一行 stderr。
- 折叠不等于丢弃。同一 link、同一 outcome、同一来源 IP 在一分钟内重复的拒绝合并进同一行，
  `hits` 记次数、`first_at` 记起点。换一个来源就是另一行；`success` 永不折叠。
- `access_logs` 独立于它描述的资源：行自带 `owner_id` 和资源名快照，删除资源不带走历史。
- 匿名内容属于 `AnonymousUserID` 这个哨兵用户，不是空 owner。`owner_id` 因此保持 `NOT NULL`，
  配额、访问记录和删除路径都不需要为它开特例；没有人能登录成它。
- 匿名内容一律以 `text/plain` 交付，与文件名无关。这条是滥用边界，不是显示偏好——
  它把「在别人域名上挂一个页面」整类问题排除掉了。
- 匿名链接必须有 TTL，`ttl = 0` 表示取默认值而不是永不过期。上限由 `AnonymousMaxTTL` 在 store 层兜底，
  构造请求绕不过去。短生命周期是开放端点的主要约束，不是配置项。
- 匿名内容由 `Prune` 回收：最后一条可达链接死掉之后，行和对象一起删。创建到一半没写成链接的资源
  也会被同一条规则扫掉。
- 匿名套餐是保险丝不是配额。真正的控制是 TTL 和限流；它只在那两样都失效时才动作。
- 限流不在应用里，在边缘。这是有意的边界，代价是它必须真的被配置：见 README 的部署一节。
  应用这一侧的责任是不给出绕过边缘的路——容器默认只发布到回环地址。
- `clientIP()` 不猜。`PLAINMOTE_TRUSTED_PROXIES` 为空时它只认直连对端，所以隧道部署下
  这个变量决定了访问记录里的来源 IP 是访客还是隧道自己。
- 配额只管写入，不碰交付。套餐过期不会让已发出的分享失效。
- 配额检查在事务里、用户行锁之下；对象存储的读写在事务之外。
- 每个配额天花板都有对应的释放路径：`DeleteResource` 是 `max_resources` 的唯一出口。

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
- 进程内有且只有一个后台循环：`app.prune`。它绑在 `signal.NotifyContext` 上，关停时随之退出，失败只记日志
  不终止进程——表大一点不影响分发资源。加第二个后台循环前，先确认它是否真的不能在请求时求解。
- 清理必须先拿到 advisory lock `pruneLockKey`，且锁与删除要在同一个连接上：`pg_try_advisory_lock` 是会话级的，
  从连接池里另取一条连接执行删除等于没加锁。
- 删除按 `pruneBatch` 分批。不要改成一条不带 `LIMIT` 的 `DELETE`：积压数月的库首次清理会变成一个长事务。
- `PLAINMOTE_LOG_RETENTION=0` 只关闭访问记录的清理，会话照清。会话过期是关于会话本身的事实，不是存储策略。

## 关键链路

### 登录

```text
GET /auth/github
GET /auth/github/callback
  -> internal/auth/github.go
  -> internal/store/users.go
  -> internal/web/session.go
```

OAuth 只验证 GitHub 身份，`PLAINMOTE_REGISTRATION_MODE` 决定一个此前未出现的身份能否创建本地账号。
`open` 接受所有新用户，`allowlist` 检查数字 GitHub ID，`closed` 只允许已有用户登录。注册名单不是
持续访问控制：已有用户不能因为离开名单而被踢出；账号封禁应由未来独立的用户状态承担。

重点检查 OAuth state、已有用户与新用户的分支、注册策略默认拒绝、Cookie 安全属性、Session 哈希和 CSRF。

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
  -> browser HTML request for stored audio/video: empty player shell
  -> otherwise: ConsumeToken transaction -> S3 Open or upstream Fetch -> access_logs

Browser player
  -> GET same /d/ address with ?raw=1 (one full-body, counted request)
  -> response.blob() -> URL.createObjectURL() -> native audio/video controls
```

分享次数通过 PostgreSQL 条件 `UPDATE ... RETURNING` 原子扣除，正文读取失败仍会消耗本次额度。
所有公开正文请求都扣次，不能因为 Cookie、Range、User-Agent 或 Fetch Metadata 而免扣。
公开接口忽略 Range 并完整返回 200，使用 `Accept-Ranges: none`；所有正文请求统一通过 `ConsumeToken` 校验并计次。
资源编辑页的已登录原始内容接口仍可使用 S3 OpenRange，与公开分享计次无关。

媒体只使用原来的分享地址。Accept 的 HTML 协商用于选择播放器外壳，不能授予读取权限；外壳不能包含资源正文，
也不能提前扣次。JS 从同一地址的 `?raw=1` 完整读取一次，不自动重试，生成本地 Blob URL 给播放器。
播放器不能将 /d/ 网络地址直接作为 src。播放、拖动、重播不再访问正文接口；刷新重新获取正文，额度耗尽应显示失败。

播放器只载入本地静态脚本和样式，CSP 仅允许同源 fetch 与 blob: 媒体，禁止嵌入并设置 no-referrer/no-store。
文件字节响应保持 sandbox/default-src 'none'，不能将用户内容当作脚本执行。查询参数和 Referer 中的 Token、grant 等敏感值应脱敏。
已下载内容可在客户端继续播放，这与普通下载一致，不能承诺撤销后收回已交付字节。

回归验证应覆盖：所有文件类型携带旧 Cookie、Range 和伪造媒体头仍扣次；播放器外壳不扣次、正文只请求一次；
音频/视频首开、拖动、刷新、多标签页；撤销、到期、删除、资源更新后不能利用耗尽链接取新内容。
必须用真实浏览器确认播放、拖动过程中没有追加正文请求，不能只检查 HTTP 请求头。

### 远程资源

```text
resource.origin_url
  -> internal/upstream/client.go
  -> redirect validation
  -> DNS/address validation
  -> response size limit
```

远程资源只允许公网 HTTP/HTTPS 地址，没有放行私网的配置或代码分支。重点检查原始地址、每次重定向、
DNS 解析后的最终拨号地址、禁用环境 HTTP 代理，以及响应 `Content-Type` 的安全降级和 `nosniff`。

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
- 预览开 `EditorView.lineWrapping`，编辑器不开。这是有意的不对称：预览回答「这个地址是什么」，
  对齐浏览器的软换行；编辑器沿用代码编辑器惯例。不要为了一致把两边统一。
- 资源页是单栏。内容面板下面的 `.savebar` 提交整个 form；名称、文件名等字段也放在这里。
  `.panel-ed > .pb` 的去内边距由当前可见的编辑器或媒体预览把关，没跑起编辑器的浏览器必须仍然拿到
  有内边距的 textarea。
- 加载 `editor.js` 的只有两种页面：可在线编辑的资源，以及上游返回文本、`UpstreamTextPreview` 为真的
  远程资源。非文本资源和取不到上游内容的远程资源都不能下载 CodeMirror bundle。
- 本地资源页加载轻量的 `upload.js`。它负责文件名建议、大小提示、文件分类和媒体预览；图片、音频和视频
  使用 Object URL，不能为了预览读取完整文件。`editor.js` 只订阅文本和未知类型的文件，识别为二进制后
  必须交还给普通文件预览。媒体与普通文件状态下必须隐藏文件编码和行尾序列控件。音视频使用 `preload="metadata"`；
  视频元数据就绪后只 seek 到 `0.001s` 触发首帧解码，不能改成自动播放或整段预载。
- 已有资源用 `.resource-detail` 单独缩短内容区；没有分享和仅有一条分享时分别用 `:has()` 校正高度，让整个页面
  留在首屏且不出现纵向滚动条，多条分享则保留更高的编辑区域并允许页面滚动。新建页没有分享区，继续使用
  较大的默认高度。`resource.js` 拦截“新建分享”，POST 成功后只替换 `data-share-status` 和 `data-share-panel`。
  失败恢复只能发 GET 刷新页面，不能自动重试 POST，否则响应丢失时可能重复创建分享。
- 表单提交前由 `editor.js` 把内容写回 textarea；CSRF、字段名和大小校验都不经过 JavaScript。
- 选择文本文件后，编辑器必须立即自动识别编码并显示内容。文件编码下拉框表示保存格式；识别不准时，用户选择
  文件编码后必须再点「通过编码重新打开」，才能按该编码解释原始字节。未修改时上传和已有资源必须保留原始字节；修改
  正文、文件编码或行尾序列后，编辑器必须覆盖仍处于选中状态的上传文件，服务端按当前选项重新写入。
- 浏览器和服务端支持的编码集合必须一致；资源要单独持久化源编码，不能靠 HTTP Content-Type 或文件后缀
  猜测后直接覆盖。无法可靠解码或含二进制控制字符的内容不能进入文本编辑器。
- 行尾序列是文件属性，不是正文的一部分。`store.DetectEOL` 与 `editor.js` 的 `detectEOL` 必须给出相同结果，
  两边都是「CRLF 数量多于其余换行数量才算 CRLF」，且只产出 `lf` 或 `crlf`。改一边必须改另一边。
- 行尾序列不入库：每次渲染从字节重新检测。因此没有可迁移的列，也不存在与内容不符的陈旧值。
- 写入前由服务端 `store.ApplyEOL` 按表单的 `content_eol` 统一行尾序列。这一步不能省：浏览器提交 textarea 时
  一律把换行规范成 CRLF，无论有没有 JavaScript，省掉它会把 LF 文件写成 CRLF。
- 编辑器侧必须用 `view.state.sliceDoc()` 取内容，不能用 `view.state.doc.toString()`。后者硬编码 LF，
  会把整个文件的行尾序列改掉，包括用户没有碰过的行。行尾序列通过 `EditorState.lineSeparator` 放在 compartment 里。
- 编辑器的焦点提示是 `.panel-ed` 的边框换成 `--accent`，和页面上每个 input、select 的做法一致。
  不要改回在编辑器内部画环：`.cm-gutters` 是 `position: sticky` 且带 `z-index`，
  inset 阴影和 inset outline 都会被它盖住，行号列那一段会整段缺失。
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
- `sessions` 只靠 `SessionUser` 删除「被出示且已过期」的那一行，换了设备或清掉 cookie 的会话不会再被查询到。
  兜底在 `Store.Prune`，不要因为看起来重复就把它去掉。
- 当前没有历史迁移；schema 不兼容时重建快速迭代环境。schema.sql 里不留兼容性 `ALTER`。
- `access_logs.owner_id` 是 `NOT NULL`：进入这张表的每一行都必须有主人，否则它没有读者。
  写入侧靠 `links.resource_id` 的 `NOT NULL` 和级联删除保证这一点，`recordRefusalTx` 因此用内连接。
- 数据库测试在没有 `PLAINMOTE_TEST_DATABASE_URL` 时整包跳过，`go test ./...` 依然打印 ok。
  改动 store 后必须带着这个变量跑一遍，否则等于没测。

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
