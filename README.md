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
| `GITHUB_ALLOWED_IDS` | 允许登录的 GitHub 数字用户 ID，逗号分隔 |

可选参数：

| 变量 | 默认值 | 作用 |
| --- | --- | --- |
| `PLAINMOTE_LISTEN` | `:8964` | HTTP 监听地址 |
| `PLAINMOTE_BLOB_REGION` | `auto` | S3 区域 |
| `PLAINMOTE_ALLOW_PRIVATE_UPSTREAM` | `false` | 是否允许引用私网地址 |
| `PLAINMOTE_TRUSTED_PROXIES` | 空 | 可信代理 IP/CIDR，逗号分隔 |
| `PLAINMOTE_SESSION_TTL` | `720h` | 登录会话有效期 |
| `PLAINMOTE_MAX_CONTENT_MIB` | `4` | 单份内容大小上限 |

配置在进程启动时完成类型转换和完整校验，缺少必填值会立即退出。

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

## 编辑器

可在线编辑的资源内容使用 CodeMirror 6，支持 YAML、JSON、TOML、XML、shell 和 ini/conf 语法高亮。
选择文件后，内容会立即载入编辑器。编辑器会自动识别 UTF-8、带 BOM 或无 BOM 的 UTF-16/UTF-32、
GB18030/GBK、Big5、日文和韩文编码，以及常用 Windows、ISO、KOI8 和 Mac 编码；识别不准时可以从
编码列表重新打开。未修改的上传和已有资源保留原始字节，修改后则按当前选定的编码写回，BOM 也会保留。
后续维护和 review 可按 [`docs/encoding-editor-learning.md`](docs/encoding-editor-learning.md) 中的学习与检查清单进行。

引用地址不能在线编辑，但同样用 CodeMirror 呈现：上游返回文本时挂一个只读编辑器，保留行号、高亮和搜索，
去掉编辑。上游是非文本或取不到时不加载编辑器。

资源页是双栏：编辑器占宽栏，名称、文件名和元信息在 320px 窄栏，保存横跨两栏。窄于 1180px 时塌成单列。

行尾按文件元数据处理，和编码一样：打开时从字节里检测，面板标题栏显示 LF 或 CRLF，保存时按它写回。
未修改的内容不重写，所以只看不改不会动任何字节。行尾不统一的文件，在第一次保存时统一成检测出的那一种，
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
