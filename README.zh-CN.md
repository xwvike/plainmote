<p align="center">
  <img src="docs/logo.png" alt="PlainMote" width="160" height="160">
</p>

# PlainMote

[English](README.md) | 简体中文

PlainMote 是一个用于分享配置文件、日志及其他文件的 Web 服务。内容按原始字节保存和交付；同一份内容可为不同的接收者或机器分别签发链接，每条链接独立设置有效期与使用次数、可单独撤销，并记录经由该链接的每次访问。

参考部署：[plainmote.link](https://plainmote.link)

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/screenshots/quick-share-dark.png">
  <img src="docs/screenshots/quick-share-light.png" alt="快速分享输入框：粘贴的日志中时间、级别和 key=value 均已高亮，下方为 10 分钟至 30 天的有效期刻度">
</picture>

## 功能

- **快速分享**：无需账号。在首页提交文本或文件（单份不超过 10 MiB）即生成一条分享链接，有效期可选 10 分钟、1 小时、1 天、7 天或 30 天，默认 1 小时。亦可通过 curl 从命令行提交。登录后创建的快速分享归入“我的资源”，带有标记，访问记录归创建者所有；在链接失效前只读，可转为普通资源长期保留，否则到期后内容被删除，访问记录保留。
- **资源与分享链接**：登录用户可保存文件（默认每个账号 100 MiB），并为同一资源签发多条链接。每条链接的有效期与使用次数独立设置，撤销其中一条不影响其余链接；资源内容更新后，各链接地址不变，并立即返回新内容。
- **访问记录**：每次经由链接的访问均记录时间、结果、来源 IP 及客户端信息，仅资源所有者可见；默认保留 30 天。
- **历史版本**：每次保存改动资源内容时，被替换的版本予以保留，每个资源最多 10 个，每个版本自被替换起保留 30 天。历史版本可查看、与任一版本对比、恢复为新版本、另存为新资源或单独删除。文本逐行对比；图片、音视频及其他文件并列显示，并标注大小与 SHA-256 摘要，图片另标尺寸。历史版本计入存储空间，但仅占用当前内容以外的剩余空间；保存需要该空间时，最早被替换的版本先被清除。访问记录注明每次获取的版本。若保存所依据的版本已不是最新版本，本次保存不会执行，页面将提示冲突，以免覆盖较新的内容。
- **远程资源**：资源可仅保存一个公网地址，每次访问时由服务端实时从源站读取。
- **条件请求**：分享链接支持 `HEAD` 请求，响应附带 `ETag`（存储内容另附 `Last-Modified`）。客户端携带 `If-None-Match` 或 `If-Modified-Since` 且内容未变化时，返回不含正文的 `304`。每次应答均计为一次使用并记入访问记录。
- **编辑器**：基于 CodeMirror 6，支持 YAML、JSON、TOML、XML、INI 与 `.env`、Shell、nginx、Dockerfile、SQL、diff 及日志的语法高亮，快速分享输入框使用同一编辑器。可识别 UTF-8、UTF-16、GB18030、Big5、Shift_JIS 等编码，保存时保留原编码与行尾。
- **命令行**：`plainmote` 在终端中查看、编辑、上传资源和创建快速分享，编辑时使用本机的编辑器，关闭后保存为新版本；登录在浏览器中确认。
- **端到端加密（可选）**：登录用户可设置主密码，为单个资源开启端到端加密：内容、名称、文件名和历史版本在浏览器中加密后上传，服务端仅保存密文；分享链接附带单独的链接密钥，接收方在浏览器中解密。快速分享也可在账号设置中开启加密。设计与格式见 [docs/encryption.zh-CN.md](docs/encryption.zh-CN.md)。
- **媒体预览**：在浏览器中打开分享链接时，图片直接显示，音视频直接播放；其他客户端获取原始字节。
- **界面**：提供英语、简体中文、繁体中文、日语、法语和德语，依浏览器语言显示；首页与法律页面另设各语言的独立地址（如 `/zh-cn/`、`/ja/`），供搜索引擎分别收录。支持浅色与深色主题。

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/screenshots/share-links-dark.png">
  <img src="docs/screenshots/share-links-light.png" alt="一份配置及其分享链接：每台机器、每个人各一条，各自的剩余时间与使用次数，已撤销和已用完的链接单独列出">
</picture>

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/screenshots/access-history-dark.png">
  <img src="docs/screenshots/access-history-light.png" alt="访问记录：每次访问的时间、结果、分享链接和来源 IP，机器的轮询与人的打开并列可见">
</picture>

## 命令行

`plainmote` 用于在终端中查看、编辑和上传资源。编辑时使用本机的编辑器（Zed、VS Code、Vim 等），关闭编辑器后内容保存为新版本。

### 安装

```bash
curl -fsSL https://plainmote.link/cli/zh | sh        # macOS、Linux
irm https://plainmote.link/cli/zh.ps1 | iex          # Windows PowerShell
```

安装脚本识别系统与处理器架构，下载所部署服务提供的对应文件，校验 SHA-256 后安装到当前用户目录（macOS 与 Linux 为 `~/.local/bin`），不需要管理员权限。同一地址可在执行前查看脚本内容。提供 macOS、Linux 和 Windows 的 amd64 与 arm64 版本，版本号与提供它的服务端一致；服务升级后重新执行安装命令即可更新。

`/cli/zh` 安装的命令行使用中文提示，`/cli/en` 使用英文提示，`/cli` 跟随系统语言。`/cli` 页面按访问者的系统给出安装命令，并列出各平台的文件与校验和。

### 使用

```bash
plainmote login                               # 在浏览器中确认登录
plainmote ls nginx
plainmote edit nginx.conf
plainmote cat nginx.conf | grep listen
tail -n 200 app.log | plainmote push - --name app.log
plainmote push ./nginx.conf --to nginx.conf
plainmote share ./app.log --ttl 1d              # 创建快速分享，只输出链接
plainmote push ./prod.env --encrypt             # 新建端到端加密资源
plainmote ls --decrypt prod                     # 列出资源，并解密加密资源的名称
```

打开加密资源的命令会询问主密码，也可从环境变量 `PLAINMOTE_MASTER_PASSWORD` 读取。

资源可以用 ID、至少 6 位的 ID 前缀，或完整的名称、文件名指定；匹配到多个资源时列出候选，不作选择。

### 登录

登录采用设备授权（RFC 8628）：命令行先显示验证码与确认地址，按 Enter 后打开浏览器，也可以在任意设备上手动打开该地址；用户在已登录的浏览器中输入验证码、核对设备信息并允许。验证码不会出现在所打开的地址中。`--read-only` 申请只读权限；`--no-browser` 不打开浏览器。

登录按服务器分别保存，可以同时登录多个部署。所用的服务器依次取自 `--server`、环境变量 `PLAINMOTE_SERVER` 和 `plainmote server <地址>` 保存的设置；`edit`、`push` 等写入操作在输出中注明目标服务器。

### 编辑与设置

保存时以下载时的版本为基准。如资源在此期间已在别处保存，命令行显示差异，并提供覆盖保存、基于新版本重新编辑或保留文件三种处理方式；无法保存的编辑内容不会被丢弃。

编辑器依次取自环境变量 `PLAINMOTE_EDITOR`、`plainmote config editor <命令>` 保存的设置、`$VISUAL` 和 `$EDITOR`；均未设置时，第一次编辑会列出本机已安装的编辑器供选择并记住。提示语言依次取自环境变量 `PLAINMOTE_LANG`、`plainmote config language zh|en|auto` 保存的设置和系统语言。`plainmote config` 显示当前全部设置。

命令行所用的接口见 [命令行接口](docs/api.zh-CN.md)。

### 使用 curl 分享

```bash
tail -n 200 app.log | curl -F 'content=<-' https://plainmote.link/paste
curl -F content=@app.log https://plainmote.link/paste
curl --data-binary @app.log 'https://plainmote.link/paste?ttl=1d&filename=app.log'
curl https://plainmote.link/paste    # 输出用法说明
```

`ttl` 可取 `10m`、`1h`、`1d`、`7d` 或 `30d`，纯数字按分钟计。成功时响应正文仅为一行分享地址，可直接用于管道或 `$(...)`。`/llms.txt` 以 llmstxt.org 格式向语言模型提供相同的说明。

## 安全与隐私

- **链接即凭证**：Token 为 256 位随机值。数据库仅保存其 SHA-256 摘要（用于检索）及经 AES-GCM 加密的原值。
- **禁止跨站嵌入**：其他站点将分享地址作为图片、视频或脚本嵌入时，请求在读取 Token 之前即被拒绝。
- **禁止收录**：分享地址、资源及账号页面均禁止搜索引擎收录，仅介绍服务的页面可被索引。
- **不提供网页托管**：可能被浏览器执行的类型（HTML、脚本、SVG 等）不会按原类型交付。免登录分享中，文本以纯文本交付，经文件头确认的图片与音视频按原类型交付，其余文件仅以附件形式提供下载；加密分享一律交付密文。
- **仅执行本站脚本**：页面通过内容安全策略只执行本站提供的脚本文件，不执行内联脚本，也不能被其他站点以框架嵌入。
- **命令行令牌**：通过设备授权签发，服务端仅保存其摘要。令牌自登录起 90 天内有效，分为只读和读写两种，均不能删除资源、管理分享链接或修改账号。所有已登录的设备及其最后使用情况列于“账号”页面，可随时撤销。接口仅从 `Authorization` 请求头读取令牌，不接受 Cookie 或地址中的令牌，也不响应跨站请求。
- **端到端加密**：仅使用浏览器内置的 WebCrypto，算法为 AES-256-GCM。默认情况下，密钥位于链接 `#` 之后的部分，浏览器不会将其发送至服务器。创建者也可改用 4 位口令，此时密钥由接收者的浏览器通过 PBKDF2-SHA-256（600,000 次迭代）从口令推导。
- **加密的局限**：解密依赖 JavaScript；链接或口令遗失后内容无法恢复；内容大小与访问记录不在加密范围内。口令取自 31 个字符，共约 92 万种组合，仅能防止偶然看到链接的人查看内容，不能抵御取得链接后逐一尝试的攻击者，敏感内容应使用附带密钥的完整链接。加解密代码由服务端提供，其可信程度取决于所部署代码的完整性；公开的源代码使之可供核查。

## 自部署

PlainMote 为单个无状态的 Go 服务，依赖以下组件：

- PostgreSQL
- S3 兼容对象存储（例如 Cloudflare R2、MinIO）
- GitHub OAuth App（用于登录）

```bash
cp .env.example .env    # 填写数据库、对象存储、OAuth 和密钥
docker compose up -d
```

镜像构建、外部服务准备、公网入口配置以及升级与回滚，见 [部署文档](docs/deployment.zh-CN.md)。

### 配置

全部参数通过环境变量提供，并在启动时校验；缺失或取值非法时，服务立即退出。

必须提供：

| 变量 | 作用 |
| --- | --- |
| `DATABASE_URL` | PostgreSQL 连接地址 |
| `PLAINMOTE_PUBLIC_URL` | 对外的 HTTP(S) origin，用于生成分享地址和 OAuth 回调地址 |
| `PLAINMOTE_BLOB_ENDPOINT` | S3 兼容服务端点 |
| `PLAINMOTE_BLOB_BUCKET` | Bucket 名称 |
| `PLAINMOTE_BLOB_ACCESS_KEY` | Access Key ID |
| `PLAINMOTE_BLOB_SECRET_KEY` | Secret Access Key |
| `PLAINMOTE_TOKEN_KEY` | 32 字节的 Token 加密密钥（64 位十六进制或 Base64）；更换后已有分享地址无法解密 |
| `GITHUB_CLIENT_ID` | GitHub OAuth Client ID |
| `GITHUB_CLIENT_SECRET` | GitHub OAuth Client Secret |

可选：

| 变量 | 默认值 | 作用 |
| --- | --- | --- |
| `PLAINMOTE_LISTEN` | `:8964` | HTTP 监听地址 |
| `PLAINMOTE_BLOB_REGION` | `auto` | S3 区域 |
| `PLAINMOTE_TRUSTED_PROXIES` | 空 | 可信代理的 IP 或 CIDR，逗号分隔 |
| `PLAINMOTE_SESSION_TTL` | `720h` | 登录会话有效期 |
| `PLAINMOTE_MAX_CONTENT_MIB` | `10` | 单份内容大小上限 |
| `PLAINMOTE_LOG_RETENTION` | `720h` | 访问记录保留时长，`0` 表示永久保留 |
| `PLAINMOTE_REGISTRATION_MODE` | `allowlist` | 新用户注册策略：`open`、`allowlist` 或 `closed` |
| `GITHUB_ALLOWED_IDS` | 空 | `allowlist` 模式下允许注册的 GitHub 数字用户 ID，逗号分隔 |
| `PLAINMOTE_ANONYMOUS` | `false` | 是否开放首页的免登录快速分享 |
| `PLAINMOTE_OPERATOR` | 空 | 运营者名称，显示在关于、隐私政策和服务条款中 |
| `PLAINMOTE_CONTACT_EMAIL` | 空 | 联系邮箱；设置后才提供关于、隐私政策、服务条款和联系页面 |
| `PLAINMOTE_SOURCE_URL` | 本仓库地址 | 页面底部“源代码”链接指向的地址；部署修改过的版本时，依 AGPL 须指向修改后的源代码 |
| `PLAINMOTE_ADMIN_KEYS` | 空 | 允许调用管理接口的 Ed25519 公钥，Base64 编码，逗号分隔；为空时管理接口不存在 |
| `PLAINMOTE_ADMIN_ORIGINS` | 空 | 允许在浏览器中跨域调用管理接口的管理页面来源，逗号分隔 |
| `PLAINMOTE_CLI_DIR` | `/cli` | `/cli` 所提供的命令行文件所在目录；容器镜像已包含这些文件 |

### 管理

本服务不包含管理页面，而是提供一组以 Ed25519 签名保护的 JSON 接口，涵盖部署概况、账号停用与恢复、套餐、资源下架与删除、链接撤销及审计记录，供另行部署或在本机运行的管理页面调用。服务端仅保存公钥；接口不返回资源内容、分享地址及访问记录中的访问者信息。[PlainMote Admin](https://github.com/xwvike/plainmote-admin) 是基于该接口的管理页面。功能、启用方法与完整的接口参考见 [管理文档](docs/admin.zh-CN.md)。

### 关于法律文本

设置 `PLAINMOTE_CONTACT_EMAIL` 后，服务提供关于、隐私政策、服务条款及联系页面（简体中文与英文）。上述文本依据参考部署的实际行为编写，其中的时长、大小等数值取自运行配置。**自行部署时，部署者应审阅这些文本，并依所在地区的法律及自身运营情况进行调整**（文件位于 `internal/web/templates/legal_*.html`）；部署者对其站点所作的承诺承担责任。

## 文档

- [部署](docs/deployment.zh-CN.md)：镜像构建、外部服务、公网入口、升级与回滚、运行约束及备份
- [管理](docs/admin.zh-CN.md)：管理功能、启用方法与管理接口参考
- [命令行接口](docs/api.zh-CN.md)：设备授权、令牌与 `/api/v1/` 接口参考

## 开发

开发环境包含 PostgreSQL、MinIO 及支持热更新的应用：

```bash
docker compose -f compose.dev.yaml up --build
```

应用地址为 `http://localhost:8964`，MinIO 控制台地址为 `http://localhost:9001`。登录功能需在 `.env` 中配置 GitHub OAuth 参数。

运行测试：

```bash
docker compose -f compose.dev.yaml up -d postgres
PLAINMOTE_TEST_DATABASE_URL='postgres://plainmote:plainmote@127.0.0.1:54329/plainmote?sslmode=disable' go test ./...
go vet ./...
node tools/e2ee/e2ee_test.mjs
npm --prefix tools/codemirror test
```

编辑器及编码识别所需的依赖以 bundle 形式随仓库发布，运行时不从第三方 CDN 加载。如需重建，执行 `tools/codemirror/build.sh`；常规构建不需要 Node.js。

```text
cmd/plainmote/       服务入口
cmd/plainmote-admin/ 管理密钥生成与请求签名工具
cmd/plainmote-cli/   plainmote 命令行
internal/app/        服务组装与 HTTP 生命周期
internal/auth/       GitHub OAuth
internal/blob/       S3 兼容对象存储
internal/config/     环境变量解析与校验
internal/linediff/   版本页面与命令行共用的行级差异比较
internal/store/      PostgreSQL schema 与业务约束
internal/upstream/   远程地址校验与读取
internal/web/        路由、页面、静态资源与公开分发
tools/               编辑器 bundle 与加密测试
docs/                部署、管理与接口文档，以及图片
```

## 许可证

Copyright (C) 2026 xwvike

本项目以 [GNU Affero General Public License v3.0](LICENSE) 发布。依据该许可证，修改本项目并通过网络向他人提供服务的，须向这些用户提供修改后的源代码。

第三方组件（CodeMirror 等，MIT；jschardet，LGPL-2.1）的许可证见 [`tools/codemirror/NOTICE.md`](tools/codemirror/NOTICE.md)。
