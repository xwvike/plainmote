<p align="center">
  <img src="docs/logo.png" alt="PlainMote" width="160" height="160">
</p>

# PlainMote

[English](README.md) | 简体中文

PlainMote 用来保存和分享配置文件、日志、笔记这类文件。每次保存都留有版本；每个接收者或机器各用一条分享链接，有效期和次数分别设置，可以单独撤销，每次访问都有记录。除了网页，也可以在终端、API 或 AI 应用中使用。

参考部署：[plainmote.link](https://plainmote.link)

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/screenshots/quick-share-dark.png">
  <img src="docs/screenshots/quick-share-light.png" alt="快速分享输入框：粘贴的日志中时间、级别和 key=value 均已高亮，下方为 10 分钟至 30 天的有效期刻度">
</picture>

## 功能

- **快速分享**：不用注册。在首页粘贴文本或上传文件，得到一条有效期 10 分钟到 30 天的链接；也可以用 curl 提交。登录后创建的快速分享出现在“我的资源”中，到期前可以转为普通资源保留。
- **分享链接**：同一份资源可以发出多条链接，每条有自己的有效期和次数上限，可以单独撤销。内容更新后链接地址不变。链接支持 `ETag` 与 `304`，适合机器定时拉取；在浏览器中打开时，图片和音视频直接预览。
- **版本历史**：每次改动内容都保留被替换的版本（每个资源最多 10 个，保留 30 天），可以对比、恢复或另存为新资源。保存时若别处已有更新，提示冲突而不覆盖。
- **访问记录**：每次经由链接的访问都记下时间、结果、来源 IP 和客户端，仅所有者可见，默认保留 30 天。
- **标签**：给资源打上 `tmp`、`surge` 这样的文本标签，在列表中按标签筛选。
- **编辑器**：基于 CodeMirror 6，常见配置格式与日志有语法高亮；识别 UTF-8、GB18030、Shift_JIS 等编码，保存时保留原编码和换行符。
- **端到端加密（可选）**：设置主密码后，单个资源或快速分享可以在浏览器中加密后再上传，服务端只保存密文。
- **命令行与 MCP**：`plainmote` 命令行查看、编辑和上传资源；Claude、ChatGPT、Cursor 等 AI 应用经授权后通过 `/mcp` 读写资源。
- **界面**：英语、简体中文、繁体中文、日语、法语和德语，浅色与深色主题。

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/screenshots/share-links-dark.png">
  <img src="docs/screenshots/share-links-light.png" alt="一份配置及其分享链接：每台机器、每个人各一条，各自的剩余时间与使用次数，已撤销和已用完的链接单独列出">
</picture>

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/screenshots/access-history-dark.png">
  <img src="docs/screenshots/access-history-light.png" alt="访问记录：每次访问的时间、结果、分享链接和来源 IP，机器的轮询与人的打开并列可见">
</picture>

## 命令行

```bash
curl -fsSL https://plainmote.link/cli/zh | sh        # macOS、Linux
irm https://plainmote.link/cli/zh.ps1 | iex          # Windows PowerShell
```

安装脚本校验 SHA-256 后安装到当前用户目录，不需要管理员权限；服务升级后重新执行即可更新。`/cli/zh` 安装中文提示的版本，`/cli/en` 安装英文提示的版本。

```bash
plainmote login                               # 在浏览器中确认登录
plainmote ls nginx
plainmote edit nginx.conf                     # 用本机编辑器打开，关闭后保存为新版本
plainmote cat nginx.conf | grep listen
tail -n 200 app.log | plainmote push - --name app.log
plainmote push ./nginx.conf --to nginx.conf
plainmote push ./note.md --tag tmp            # 新建资源并打上标签
plainmote ls --tag tmp
plainmote share ./app.log --ttl 1d            # 创建快速分享，只输出链接
plainmote push ./prod.env --encrypt           # 新建端到端加密资源
```

- 资源可以用 ID、至少 6 位的 ID 前缀、名称或文件名指定；匹配到多个时列出候选。
- 编辑期间若资源在别处被保存，命令行显示差异，由你选择覆盖、基于新版本重新编辑或保留文件。
- 打开加密资源时询问主密码，也可以从环境变量 `PLAINMOTE_MASTER_PASSWORD` 读取。
- `plainmote help` 列出全部命令与设置。命令行所用的接口见 [命令行接口](docs/api.zh-CN.md)。

### 用 curl 分享

```bash
tail -n 200 app.log | curl -F 'content=<-' https://plainmote.link/paste
curl --data-binary @app.log 'https://plainmote.link/paste?ttl=1d&filename=app.log'
curl https://plainmote.link/paste    # 输出用法说明
```

`ttl` 可取 `10m`、`1h`、`1d`、`7d` 或 `30d`。响应正文只有一行分享地址，可以直接用于管道或 `$(...)`。

## 安全与隐私

- **链接即凭证**：Token 为 256 位随机值，数据库只保存其摘要和加密后的原值。分享地址不被搜索引擎收录，也不能被其他站点嵌入。
- **不托管网页**：HTML、脚本、SVG 等可能被浏览器执行的类型不会按原类型交付；页面只执行本站提供的脚本文件。
- **令牌与授权**：命令行令牌和 MCP 授权在服务端只保存摘要，都不能删除资源，可以随时在设置的“已授权应用”中撤销。
- **端到端加密**：在浏览器中使用 WebCrypto（命令行使用 Go 标准库），AES-256-GCM；主密码不离开设备。解密依赖 JavaScript；主密码和恢复密钥都遗失后内容无法恢复；内容大小和访问记录不加密。4 位口令只能防止偶然看到链接的人，敏感内容请用带密钥的完整链接。设计与威胁模型见 [端到端加密](docs/encryption.zh-CN.md)。

## 自部署

PlainMote 是单个无状态的 Go 服务，需要 PostgreSQL、S3 兼容对象存储（如 Cloudflare R2、RustFS），以及用于登录的 GitHub 或 Google OAuth。镜像 `ghcr.io/xwvike/plainmote` 提供 amd64 与 arm64 版本，服务器上只需要 `compose.yaml` 和 `.env`：

```bash
cp .env.example .env    # 填写镜像版本、数据库、对象存储、OAuth 和密钥
docker compose up -d
```

环境变量、外部服务、公网入口、升级与备份见 [部署文档](docs/deployment.zh-CN.md)。

- **管理**：服务不含管理页面，而是提供以 Ed25519 签名保护的管理接口；[PlainMote Admin](https://github.com/xwvike/plainmote-admin) 是基于它的管理页面。见 [管理文档](docs/admin.zh-CN.md)。
- **法律文本**：设置 `PLAINMOTE_CONTACT_EMAIL` 后，服务提供关于、隐私政策、服务条款和联系页面。这些文本按参考部署的实际情况编写，**自部署时请按所在地区的法律和自己的运营情况修改**（`internal/web/templates/legal_*.html`）。

## 文档

- [部署](docs/deployment.zh-CN.md)：镜像、外部服务、环境变量、公网入口、升级与备份
- [管理](docs/admin.zh-CN.md)：管理功能、启用方法与管理接口
- [命令行接口](docs/api.zh-CN.md)：设备授权、令牌、`/api/v1/` 接口与读取分享链接
- [MCP 服务器](docs/mcp.zh-CN.md)：连接 AI 应用、授权、工具与限制
- [端到端加密](docs/encryption.zh-CN.md)：密钥、格式、流程与威胁模型

## 开发

```bash
docker compose -f compose.dev.yaml up --build
```

应用地址为 `http://localhost:8964`，RustFS 控制台为 `http://localhost:9001/rustfs/console/`（用户名 `plainmote`，密码 `plainmote-secret`）。登录需要在 `.env` 中配置 GitHub 或 Google 的 OAuth 参数。

运行测试：

```bash
docker compose -f compose.dev.yaml up -d postgres
PLAINMOTE_TEST_DATABASE_URL='postgres://plainmote:plainmote@127.0.0.1:54329/plainmote?sslmode=disable' go test ./...
go vet ./...
node tools/e2ee/e2ee_test.mjs
npm --prefix tools/codemirror test
```

装有 Node.js 时，`go test` 还会运行浏览器端的加密、对比与导出模块，并校验命令行与浏览器能互相解密；没有则跳过这些测试。编辑器的依赖以 bundle 形式随仓库提交，运行时不从 CDN 加载；重建执行 `tools/codemirror/build.sh`，常规构建不需要 Node.js。

```text
cmd/plainmote/       服务入口
cmd/plainmote-admin/ 管理密钥生成与请求签名工具
cmd/plainmote-cli/   plainmote 命令行
internal/app/        服务组装与 HTTP 生命周期
internal/auth/       GitHub OAuth 与 Google OpenID Connect
internal/blob/       S3 兼容对象存储
internal/config/     环境变量解析与校验
internal/linediff/   版本页面与命令行共用的行级差异比较
internal/store/      PostgreSQL schema 与业务约束
internal/upstream/   从公网地址读取 MCP 客户端元数据文档
internal/web/        路由、页面、静态资源与公开分发
tools/               编辑器 bundle 与加密测试
docs/                部署、管理与接口文档，以及图片
```

## 许可证

Copyright (C) 2026 xwvike

本项目以 [GNU Affero General Public License v3.0](LICENSE) 发布。修改本项目并通过网络向他人提供服务的，须向这些用户提供修改后的源代码。

第三方组件（CodeMirror 等，MIT；jschardet，LGPL-2.1）的许可证见 [`tools/codemirror/NOTICE.md`](tools/codemirror/NOTICE.md)。
