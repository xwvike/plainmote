<p align="center">
  <img src="docs/logo.png" alt="PlainMote" width="160" height="160">
</p>

# PlainMote

PlainMote 是一个使用 Go 编写的无状态资源分发服务。登录用户维护自己的资源，并为资源创建相互独立、可撤销的分享地址。

## 架构

- PostgreSQL 保存用户、会话、资源元数据、分享规则和访问记录。
- S3 兼容对象存储保存上传的资源正文，可直接使用 Cloudflare R2。
- 远程资源只保存 HTTP/HTTPS URL；预览和公开访问都会实时请求上游。
- GitHub OAuth 负责登录，只有白名单中的 GitHub 数字用户 ID 可以进入。
- Web 页面由 Go 模板和手写 CSS 构成，没有前端构建流程。
- 服务不读取配置文件，全部部署参数来自环境变量。

服务直接启动，不包含 CLI 子命令。容器本地没有持久化状态，可以让多个副本共享 PostgreSQL、对象存储和 Token 密钥。

## 资源与分享

上传正文先写入对象存储，再写入 PostgreSQL。替换正文会创建新的对象 Key，数据库切换成功后再删除旧对象。对象 Key 格式为：

```text
<resource-uuid>/<content-uuid>
```

公开地址格式为：

```text
https://config.example.com/d/<token>/<filename>
```

Token 是唯一的路由依据；文件名只用于下载名称和类型提示。每条分享独立维护 Token、备注、到期时间、最大使用次数、已用次数和撤销状态。

本地与远程资源的公开响应都支持单段 HTTP Byte Range。请求本地资源的某个区间时，应用把同一区间交给
S3/R2，只转发实际需要的字节；音视频播放、断点续传和普通大文件读取共用这套交付逻辑。首次请求原子扣除
一次使用次数，并签发一个一小时有效、绑定当前 Token 和分享的加密续传 Cookie。浏览器在同一次交付中发起的
后续请求仍逐条写访问记录，但不会重复扣除次数；分享到期或撤销后，续传也立即失效。

删除资源会一并删掉它的正文对象和全部分享，已发出的地址立即失效。访问记录不随之消失：每行访问记录
自带所属用户和当时的资源名，删除后仍可在日志里按这个名字查到。

## 首页与匿名分享

`/` 是任何人都能打开的页面：粘一段文本，选一个有效期，拿到一条链接。不需要账号。

这项功能默认关闭，要开需要 `PLAINMOTE_ANONYMOUS=true`。和这里其他放宽限制的开关一样，
它失败关闭：开放写入端点是一个决定，不是默认值——把本项目当内部工具部署的场景里，
外面的人本来就不该能往里放东西。

关闭时 `/paste` 根本不存在（不是存在但拒绝），`/` 也不再是页面：未登录跳 `/login`，
已登录跳资源列表。没有框子的首页对谁都没有可做的事，多一次点击不如直接送到该去的地方。
这时整个服务都不可索引，`robots.txt` 不再放行 `/$`。

匿名内容有三条硬限制，都在 store 层，构造请求绕不过去：链接必须过期，默认 1 分钟、最长 30 分钟；
正文最大 128 KiB；无论文件名叫什么，一律以 `text/plain; charset=utf-8` 交付。最后一条是滥用边界——
它让这里没法被用来在这个域名上挂页面。链接到期后，正文和记录由 `Prune` 一起回收。

登录用户看到的是同一个页面，只有顶栏和一句说明不同：首页这个框子始终是匿名、会过期的，
要保留得用「我的资源」。账号自己的资源列表在 `/resources/`。

匿名写入的入口是 `POST /paste`，单独一条路径，方便在边缘按一条规则限流。

## 配额

每个用户的资源数量和存储总量由套餐决定，`plans` 表里标记为默认的那条在注册时自动授予。配额只作用于
写入，不影响已经发出的分享：套餐过期不会让任何地址失效。仪表盘的工具栏显示当前用量和上限，超出时
的提示会写明是数量还是容量、差多少。

服务整体不接受索引：`robots.txt` 全量 Disallow，所有响应带 `X-Robots-Tag: noindex, nofollow, noarchive`。
分享地址本身就是凭据，被爬虫收录等同于泄露，所以这里要的是不被发现，而不是被发现。

## 访问记录

公开地址的每次请求都会留下记录，但只记可归属的事件。无人签发的 Token 和格式不对的地址不写数据库——
没有所属用户就没有人能读到它，而任何匿名请求都能无限制造这种行，因此改为按分钟折叠成一行 stderr 计数。

针对同一条分享、同一种结果、同一个来源 IP 的重复拒绝会在一分钟内合并成一行，记录重复次数和起始时间；
换一个来源就是新的一行，成功的交付永不合并。日志保留期由 `PLAINMOTE_LOG_RETENTION` 控制，默认 30 天，
设为 0 表示不清理访问记录。

Token 使用 256 位安全随机数生成。PostgreSQL 保存 SHA-256 查找索引，并使用 `PLAINMOTE_TOKEN_KEY` 提供的密钥通过 AES-GCM 加密保存原值。

## 环境变量

必须提供：

| 变量 | 作用 |
| --- | --- |
| `DATABASE_URL` | PostgreSQL 连接地址 |
| `PLAINMOTE_PUBLIC_URL` | 生成分享地址和 OAuth 回调地址的 HTTP(S) origin |
| `PLAINMOTE_BLOB_ENDPOINT` | S3 兼容服务端点 |
| `PLAINMOTE_BLOB_BUCKET` | S3 Bucket |
| `PLAINMOTE_BLOB_ACCESS_KEY` | S3 Access Key ID |
| `PLAINMOTE_BLOB_SECRET_KEY` | S3 Secret Access Key |
| `PLAINMOTE_TOKEN_KEY` | 32 字节、64 位十六进制或 Base64 Token 加密密钥 |
| `GITHUB_CLIENT_ID` | GitHub OAuth Client ID |
| `GITHUB_CLIENT_SECRET` | GitHub OAuth Client Secret |

可选参数：

| 变量 | 默认值 | 作用 |
| --- | --- | --- |
| `PLAINMOTE_LISTEN` | `:8964` | HTTP 监听地址 |
| `PLAINMOTE_BLOB_REGION` | `auto` | S3 区域 |
| `PLAINMOTE_ALLOW_PRIVATE_UPSTREAM` | `false` | 是否允许引用私网地址 |
| `PLAINMOTE_TRUSTED_PROXIES` | 空 | 可信代理 IP/CIDR，逗号分隔 |
| `PLAINMOTE_SESSION_TTL` | `720h` | 登录会话有效期 |
| `PLAINMOTE_MAX_CONTENT_MIB` | `4` | 单份内容大小上限 |
| `PLAINMOTE_LOG_RETENTION` | `720h` | 访问记录保留时长，`0` 表示永久保留 |
| `PLAINMOTE_REGISTRATION_MODE` | `allowlist` | 新用户注册策略：`open`、`allowlist` 或 `closed` |
| `PLAINMOTE_ANONYMOUS` | `false` | 是否开放首页的匿名分享 |
| `GITHUB_ALLOWED_IDS` | 空 | `allowlist` 模式下允许注册的 GitHub 数字用户 ID，逗号分隔 |

OAuth 只负责确认 GitHub 身份。注册策略只应用于数据库中尚不存在的新用户：`open` 允许注册，
`allowlist` 要求 GitHub ID 位于 `GITHUB_ALLOWED_IDS`，`closed` 不接受新用户。已经注册的用户仍可登录；
后续若需要封禁，应使用独立的账号状态，而不是从注册名单中删除 ID。

配置在进程启动时完成类型转换和完整校验。缺少必填值、注册模式非法、白名单包含非数字 ID，或者
`allowlist` 模式没有任何 ID，都会立即退出。

进程内每小时清理一次过期数据，启动时先跑一遍：删除超过保留期的访问记录，以及已过期的会话。
删除分批进行，避免积压多时的库在首次清理时产生一个长事务。多副本共享同一个数据库时，由 PostgreSQL
advisory lock 保证同一时刻只有一个副本在清理，抢不到的直接跳过。这不需要宿主机 crontab、额外容器
或数据库扩展——部署仍然只是 compose 加环境变量。

## 本地开发

开发 Compose 会启动 PostgreSQL、MinIO 和使用 Air 热更新的应用：

```bash
docker compose -f compose.dev.yaml up --build
```

应用地址为 `http://localhost:8964`，MinIO 控制台为 `http://localhost:9001`。真实 GitHub 登录仍需要在 `.env` 中设置 OAuth 参数。

当前快速迭代阶段不维护旧 schema 的升级路径。schema 不兼容时可以重建开发数据库：

```bash
docker compose -f compose.dev.yaml down -v
```

## 生产部署

生产 Compose 只运行无状态应用，PostgreSQL 和 S3/R2 由外部提供：

```bash
docker compose up --build -d
```

镜像入口就是 Web 服务本身，不需要 `command`、配置文件或持久化目录挂载。健康检查入口为 `/healthz`。

容器默认只发布到 `127.0.0.1`。这个服务假定前面有一层入口，发布到所有接口等于给了一条绕过它的路。
需要换绑定地址时用 `PLAINMOTE_BIND`。

### 放在 Cloudflare Tunnel 后面

应用本身不做限流，这件事交给边缘。因此下面三项不是可选项，少一项就有一处实际后果：

**一、`PLAINMOTE_TRUSTED_PROXIES` 必须指向隧道的来源地址。**

不设置时应用只认直连对端，也就是 cloudflared 自己，访问记录里每一行的来源 IP 都会变成隧道地址。
应用不会去猜：私有网段本身不构成信任，同一网络里的另一个容器同样能伪造转发头。

取值看实际拓扑，不要照抄。cloudflared 跑在宿主机时，容器看到的是 Docker 网桥网关而不是 `127.0.0.1`，
因为端口发布经过了 NAT。最可靠的办法是发一个真实请求，然后在访问记录里读 `remote_addr` 那一项——
它记的就是直连对端，把它填进去即可（越窄越好，优先 `/32`）。

**二、匿名端点的限流要在 Cloudflare 侧配。**

应用里唯一的兜底是匿名套餐这个保险丝，它的额度远高于正常用量，只在限流已经失效时才动作。
换句话说：边缘不配限流，匿名写入就是不设防的。

**三、`/d/*` 不能被缓存。**

交付响应带 `Cache-Control: no-store`，但只要在 Cloudflare 上给这个路径开了 Cache Everything，
缓存副本就会比撤销的链接活得久——撤销将不再生效。这条路径必须始终回源。

## 测试

数据库和 Web 集成测试会为每个测试创建独立 PostgreSQL schema。先启动测试数据库：

```bash
docker compose -f compose.dev.yaml up -d postgres
```

然后执行：

```bash
PLAINMOTE_TEST_DATABASE_URL='postgres://plainmote:plainmote@127.0.0.1:54329/plainmote?sslmode=disable' go test ./...
PLAINMOTE_TEST_DATABASE_URL='postgres://plainmote:plainmote@127.0.0.1:54329/plainmote?sslmode=disable' go test -race ./...
go vet ./...
```

没有设置 `PLAINMOTE_TEST_DATABASE_URL` 时，需要 PostgreSQL 的测试会跳过。

## 编辑器与文件预览

可在线编辑的资源内容使用 CodeMirror 6，支持 YAML、JSON、TOML、XML、shell 和 ini/conf 语法高亮。
选择文本文件后，内容会立即载入编辑器。编辑器会自动识别 UTF-8、带 BOM 或无 BOM 的 UTF-16/UTF-32、
GB18030/GBK、Big5、日文和韩文编码，以及常用 Windows、ISO、KOI8 和 Mac 编码。文件编码列表决定保存格式；
识别不准时，选择正确的文件编码后点「通过编码重新打开」读取原始字节。未修改的上传和已有资源保留原始字节；修改正文、
文件编码或行尾序列后则按当前选择重新写入，BOM 也会保留。
后续维护和 review 可按 [`docs/encoding-editor-learning.md`](docs/encoding-editor-learning.md) 中的学习与检查清单进行。

选择图片、音频或视频后，内容区直接切换到相应预览，文件编码和行尾序列控件同时隐藏。浏览器用文件的 Object URL
完成预览，不把媒体复制进 JavaScript 内存。其他二进制文件显示文件名、类型和大小；已保存的普通文件可以
从资源页打开。音视频进入页面后会读取元数据；视频额外请求首个可解码帧，因此无需播放即可看到首帧和时长，
也不会为此预载整个文件。媒体预览只负责展示原文件，不进行缩略图生成、转码或编解码兼容处理。

引用地址不能在线编辑，但同样用 CodeMirror 呈现：上游返回文本时挂一个只读编辑器，保留行号、高亮和搜索，
去掉编辑。上游是非文本或取不到时不加载编辑器。

资源页使用单栏内容面板，保存操作和名称、文件名等字段位于其下方。

行尾序列按文件元数据处理，和文件编码一样：打开时从字节里检测，面板标题栏显示 LF 或 CRLF，保存时按它写回。
未修改的内容不重写，所以只看不改不会动任何字节；手动切换行尾序列会立即成为待保存的格式变更。
行尾序列不统一的文件，在第一次保存时统一成检测出的那一种，
孤立 CR 也归到这一类——这与 VS Code、Sublime 和 JetBrains 的行为一致，区别是这里把结果显示出来了。

这是渐进增强，不是替换：页面发出的仍然是 `<textarea name="content">`，编辑器挂在它旁边，
提交时把内容写回去。模块没加载、加载失败或被拦截时，页面就是一个表单里的 textarea，保存照常。

编辑器依赖 bundle 提交在仓库里并作为静态资源随应用镜像发布，浏览器运行时不会从第三方 CDN
加载编辑器或编码检测代码。`package.json` 使用精确版本，`package-lock.json` 锁定完整依赖图和 integrity；
`node_modules` 不进入仓库或应用镜像。重建脚本在临时目录执行 `npm ci`，结束后自动清理：

```bash
tools/codemirror/build.sh
npm --prefix tools/codemirror test
```

它只写 `internal/web/static/vendor/codemirror.js`。日常 Go 构建直接使用已提交的 bundle，不需要
安装 Node.js。

## 代码结构

```text
cmd/plainmote/       Web 服务入口
internal/app/        服务组装与 HTTP 生命周期
internal/auth/       GitHub OAuth 客户端
internal/blob/       S3 兼容对象存储
internal/config/     环境变量解析与校验
internal/store/      pgxpool、PostgreSQL schema 和业务约束
internal/upstream/   远程地址校验与读取
internal/web/        路由、会话、页面和公开分发
design/              设计留档与 logo 母版
docs/                README 用图
tools/codemirror/    编辑器 bundle 的重建脚本
```
