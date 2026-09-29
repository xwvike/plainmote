<p align="center">
  <img src="docs/logo.png" alt="PlainMote" width="160" height="160">
</p>

# PlainMote

[English](README.md) | 简体中文

把想要分享的文本和文件生成链接：内容逐字节保留，每条链接可以单独设置有效期和使用次数，随时撤销，每次访问都有记录。

参考部署：[plainmote.link](https://plainmote.link)

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/screenshots/share-links-dark.png">
  <img src="docs/screenshots/share-links-light.png" alt="一份资源及其分享链接：每位接收者一条链接，各自的剩余时间与使用次数，已撤销和已用完的链接单独列出">
</picture>

## 功能

- **快速分享**：无需登录，在首页粘贴文本或上传文件（最大 4 MiB）即可得到一条最长 30 分钟后失效的链接；也可以直接在终端里用 curl 分享。
- **资源与分享链接**：登录后保存文本和文件，为每位接收者创建独立的链接，分别设置有效期和使用次数，撤销其中一条不影响其他链接。
- **访问记录**：每次通过链接的访问都记录时间、结果、来源 IP 和客户端信息，仅资源所有者可见。
- **远程资源**：可以只保存一个公网地址，每次访问时实时从源站获取内容。
- **在线编辑**：基于 CodeMirror 6，支持 YAML、JSON、TOML、XML、INI 与 `.env`、Shell、nginx、Dockerfile、SQL、diff 和日志的语法高亮，快速分享的输入框使用同一编辑器；自动识别 UTF-8、UTF-16、GB18030、Big5、Shift_JIS 等编码，保存时按原编码和行尾写回。
- **端到端加密（可选）**：登录用户可在账号设置中开启，开启后快速分享在浏览器中加密，服务端只保存密文。
- **音视频与图片预览**：分享地址在浏览器中直接展示图片、播放音视频，其他客户端获取原始字节。
- **不依赖 JavaScript**：核心功能在关闭脚本时仍可使用；脚本只做增强。端到端加密是例外，加解密必须在浏览器中执行。
- **界面**：英语、简体中文、繁体中文、日语、法语和德语，按浏览器语言显示；首页和法律页面另有各语言的独立地址（如 `/zh-cn/`、`/ja/`），供搜索引擎分别收录。浅色与深色主题。

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/screenshots/access-history-dark.png">
  <img src="docs/screenshots/access-history-light.png" alt="访问记录：每次访问的时间、结果、分享链接和来源 IP">
</picture>

## 终端分享

```bash
tail -n 200 app.log | curl -F 'content=<-' https://plainmote.link/paste
curl -F content=@app.log https://plainmote.link/paste
curl --data-binary @app.log 'https://plainmote.link/paste?ttl=30&filename=app.log'
curl https://plainmote.link/paste    # 打印用法
```

响应只有一行分享地址，便于管道传递或 `$(...)` 捕获。`/llms.txt` 以 llmstxt.org 的格式向语言模型说明同样的用法。

## 安全与隐私

- **分享链接即凭证**：Token 为 256 位随机数，数据库只保存 SHA-256 索引和经 AES-GCM 加密的原值。
- **防止外链滥用**：其他网站无法把分享地址当作图片、视频或脚本嵌入，这类请求在读取 Token 之前即被拒绝。
- **禁止收录**：分享地址、资源和账号页面一律禁止搜索引擎收录；只有介绍服务的页面可被索引。
- **不做网页托管**：可能被浏览器执行的类型（HTML、脚本、SVG 等）一律不会按原类型交付；免登录分享的文本以纯文本交付，经文件头确认的图片和音视频按原类型交付，其他文件一律作为附件下载（加密的则为密文）。
- **端到端加密**：只使用浏览器自带的 WebCrypto（AES-256-GCM；以 4 位口令代替链接中的密钥时，用 PBKDF2-SHA-256 从口令推导密钥）。
  密钥位于链接 `#` 之后，浏览器不会将其发送给服务器。
  其局限同样明确：解密需要 JavaScript；链接或口令遗失后无法恢复；口令只有 4 位，只能防住偶然看到链接的人，挡不住拿到链接后逐一尝试的人；内容大小与访问记录不加密；
  加解密代码由服务端下发，其可信度取决于所部署代码的完整性，这也是本项目开源的原因之一。

## 自部署

PlainMote 是单个无状态的 Go 服务，依赖：

- PostgreSQL
- S3 兼容对象存储（例如 Cloudflare R2、MinIO）
- 一个 GitHub OAuth App，用于登录

```bash
cp .env.example .env    # 填写数据库、对象存储、OAuth 和密钥
docker compose up -d
```

镜像构建、外部服务准备、公网入口、升级与回滚见 [`docs/deployment.md`](docs/deployment.md)。

### 配置

全部参数来自环境变量，启动时完成校验，缺失或非法时立即退出。

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
| `PLAINMOTE_MAX_CONTENT_MIB` | `20` | 单份内容大小上限 |
| `PLAINMOTE_LOG_RETENTION` | `720h` | 访问记录保留时长，`0` 表示永久保留 |
| `PLAINMOTE_REGISTRATION_MODE` | `allowlist` | 新用户注册策略：`open`、`allowlist` 或 `closed` |
| `GITHUB_ALLOWED_IDS` | 空 | `allowlist` 模式下允许注册的 GitHub 数字用户 ID，逗号分隔 |
| `PLAINMOTE_ANONYMOUS` | `false` | 是否开放首页的免登录快速分享 |
| `PLAINMOTE_OPERATOR` | 空 | 运营者名称，显示在关于、隐私政策和服务条款中 |
| `PLAINMOTE_CONTACT_EMAIL` | 空 | 联系邮箱；设置后才提供关于、隐私政策、服务条款和联系页面 |
| `PLAINMOTE_SOURCE_URL` | 本仓库地址 | 页面底部“源代码”链接指向的地址；部署修改过的版本时，依 AGPL 须指向修改后的源代码 |

### 关于法律文本

设置 `PLAINMOTE_CONTACT_EMAIL` 后，服务会提供关于、隐私政策、服务条款和联系页面（简体中文与英文）。
这些文本按参考部署的实际行为编写，其中的时长、大小等数值取自当前配置。
**自行部署时，请由部署者审阅并按所在地区的法律和自身运营情况调整**
（位于 `internal/web/templates/legal_*.html`），部署者对其站点作出的承诺负责。

## 开发

开发环境会启动 PostgreSQL、MinIO 和支持热更新的应用：

```bash
docker compose -f compose.dev.yaml up --build
```

应用地址为 `http://localhost:8964`，MinIO 控制台为 `http://localhost:9001`。登录需要在 `.env` 中配置 GitHub OAuth 参数。

测试：

```bash
docker compose -f compose.dev.yaml up -d postgres
PLAINMOTE_TEST_DATABASE_URL='postgres://plainmote:plainmote@127.0.0.1:54329/plainmote?sslmode=disable' go test ./...
go vet ./...
node tools/e2ee/e2ee_test.mjs
npm --prefix tools/codemirror test
```

编辑器及编码识别的依赖以 bundle 形式随仓库发布，运行时不从第三方 CDN 加载；
重建方式为 `tools/codemirror/build.sh`，日常构建无需 Node.js。

```text
cmd/plainmote/       服务入口
internal/app/        服务组装与 HTTP 生命周期
internal/auth/       GitHub OAuth
internal/blob/       S3 兼容对象存储
internal/config/     环境变量解析与校验
internal/store/      PostgreSQL schema 与业务约束
internal/upstream/   远程地址校验与读取
internal/web/        路由、页面、静态资源与公开分发
tools/               编辑器 bundle 与加密测试
docs/                部署文档与图片
```

## 许可证

Copyright (C) 2026 xwvike

本项目以 [GNU Affero General Public License v3.0](LICENSE) 发布。依据该许可证，若您修改本项目并通过网络向他人提供服务，须向这些用户提供修改后的源代码。

第三方组件（CodeMirror 等，MIT；jschardet，LGPL-2.1）的许可证见 [`tools/codemirror/NOTICE.md`](tools/codemirror/NOTICE.md)。
