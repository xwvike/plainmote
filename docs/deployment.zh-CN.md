# PlainMote 部署

[English](deployment.md) | 简体中文

容器里只运行 PlainMote 服务本身。PostgreSQL、S3 兼容对象存储、TLS 和公网入口都在容器之外；容器内没有需要保留的数据，也不读取配置文件。

## 1. 获取镜像

每个版本都以公开镜像发布到 GitHub Container Registry，包含 `linux/amd64` 与 `linux/arm64`：

| 标签 | 指向 |
| --- | --- |
| `ghcr.io/xwvike/plainmote:v1.2.3` | 一个发布版本 |
| `ghcr.io/xwvike/plainmote:<commit>` | 构建所用提交哈希的前 12 位 |
| `ghcr.io/xwvike/plainmote:latest` | 最新的发布版本，只用于试用 |

部署时用版本标签、提交标签或镜像摘要，不要用 `latest`，否则无法确认正在运行的代码，也没法可靠回滚。摘要比标签更严格：标签可以被移动，摘要始终指向同一份内容。查询摘要：

```bash
docker buildx imagetools inspect ghcr.io/xwvike/plainmote:v1.2.3
```

在服务器的 `.env` 中固定：

```dotenv
PLAINMOTE_IMAGE=ghcr.io/xwvike/plainmote@sha256:DIGEST
```

镜像由仓库的 [发布工作流](../.github/workflows/release.yml) 在 GitHub Actions 中构建，附带签名的构建来源证明。核对镜像确实由该工作流构建：

```bash
gh attestation verify oci://ghcr.io/xwvike/plainmote@sha256:DIGEST --owner xwvike
```

镜像的 OCI 标签记录了源码仓库、版本和完整提交哈希：

```bash
docker image inspect ghcr.io/xwvike/plainmote:v1.2.3 --format '{{json .Config.Labels}}'
```

镜像的 `/cli` 目录里还有 macOS、Linux、Windows（amd64 与 arm64）的 `plainmote` 命令行，服务通过 `/cli` 和 `/developers/cli` 提供下载，约占 35 MiB。命令行与服务的版本号始终一致。

### 发布版本

给 `main` 上已推送的提交打标签并推送：

```bash
git tag v1.2.3
git push origin v1.2.3
```

工作流发布 `v1.2.3` 和提交标签；版本号不带 `-rc.1` 这类后缀时，同时更新 `latest`。运行摘要里给出可写入 `.env` 的镜像摘要。手动运行工作流只发布提交标签。工作流只使用仓库自己的 `GITHUB_TOKEN`，不持有任何服务器的凭据。首次发布后，到 GitHub 上该包的设置里确认它是公开的；私有包需要每台服务器先登录才能拉取（见第 3 节）。

### 自行构建

在仓库根目录为当前机器的架构构建镜像，例如修改过的版本：

```bash
test -z "$(git status --porcelain)" || { echo 'working tree is not clean'; exit 1; }
VERSION="$(git rev-parse --short=12 HEAD)"
docker buildx build \
  --load \
  --build-arg VERSION="$VERSION" \
  --build-arg REVISION="$(git rev-parse HEAD)" \
  --tag "plainmote:$VERSION" \
  .
```

- 在其他机器上为 x86 服务器构建：加 `--platform linux/amd64`。
- 推送双架构镜像到自己的镜像仓库：把 `--load` 换成 `--push --platform linux/amd64,linux/arm64`，标签用该仓库的地址。
- 不用镜像仓库：导出后传到服务器，导入后把 `PLAINMOTE_IMAGE` 设为 `plainmote:VERSION`。

```bash
docker save "plainmote:$VERSION" | gzip > "plainmote-$VERSION.tar.gz"
gzip -dc "plainmote-$VERSION.tar.gz" | docker load    # 在服务器上
```

部署修改过的代码时，按 AGPL-3.0 要求把 `PLAINMOTE_SOURCE_URL` 指向修改后的源代码；发布到 GHCR 时，还要修改 `Dockerfile` 中的 `org.opencontainers.image.source` 标签，GHCR 据此把镜像关联到仓库。

## 2. 准备外部服务

### PostgreSQL

创建单独的数据库和账号，连接地址带上 TLS 参数：

```text
postgres://plainmote:PASSWORD@postgres.example.com:5432/plainmote?sslmode=require
```

密码中的 `@`、`:`、`/` 等字符要做 URL 编码。服务启动时执行内嵌的 `schema.sql`，所以数据库账号需要连接、建表、建索引和读写权限。项目仍在快速迭代，不提供旧 schema 的自动迁移；升级前先查看 schema 变更并备份数据库。

### S3 或 Cloudflare R2

提前创建 Bucket，给服务的凭据授予该 Bucket 的读、写、删对象权限。Cloudflare R2 的端点是 `https://ACCOUNT_ID.r2.cloudflarestorage.com`，区域为 `auto`。Bucket 不需要公开访问，所有内容都由 PlainMote 按分享规则读取后转发。

### GitHub OAuth App

在 GitHub 创建一个单独的 OAuth App：

- Application name：`PlainMote`
- Homepage URL：`PLAINMOTE_PUBLIC_URL`
- Authorization callback URL：`PLAINMOTE_PUBLIC_URL/auth/github/callback`

GitHub 授权页显示的名称来自这个 App。最好由项目或组织账号持有；用个人账号创建时，GitHub 可能在授权信息中显示该账号。

### Google OAuth 客户端

Google 登录是可选的，可以和 GitHub 同时用，也可以单独用。在 Google Cloud Console 的 Google Auth Platform 中：

1. **品牌（Branding）**：填写应用名称、支持邮箱、首页（`PLAINMOTE_PUBLIC_URL/about`）、隐私政策（`/privacy`）和服务条款（`/terms`），并把域名加入已授权网域。该域名要在同一 Google 账号的 Search Console 中完成验证。这几个页面只在设置了 `PLAINMOTE_CONTACT_EMAIL` 后存在。
2. **受众（Audience）**：选 External。测试状态下只有列出的测试用户能登录，发布到正式环境后对所有人开放。
3. **数据访问（Data access）**：只添加 `openid` 和 `.../auth/userinfo.profile`。PlainMote 不请求邮箱或其他权限，这两个范围不需要应用验证。
4. **客户端（Clients）**：新建 Web 应用类型的客户端，重定向 URI 为 `PLAINMOTE_PUBLIC_URL/auth/google/callback`。Client Secret 只在创建时显示一次。
5. 可选：提交品牌验证，通过后同意页显示应用名称。

账号按 Google 账号 ID（`sub`）识别。用 GitHub 和用 Google 创建的账号相互独立，所有者可以在设置的“登录方式”中绑定另一种。

### 注册

`PLAINMOTE_REGISTRATION_MODE` 决定没登录过的 GitHub 或 Google 账号能否创建账号，默认 `closed`。已有账号不受影响，改完重启即可。新部署时先设为 `open`，登录创建自己的账号，不需要别人注册的话再改回 `closed`。

一种登录方式只有在 Client ID 和 Secret 都设置时才提供；缺一项时不显示对应按钮，启动日志里会说明。两种方式都不完整时，服务不启动。

## 3. 配置部署目录

服务器只需要 `compose.yaml` 和 `.env`，不需要源码：

```text
/opt/plainmote/
  compose.yaml
  .env
```

复制 [`.env.example`](../.env.example) 为 `.env`，填好必填项，并把镜像固定到某个版本或摘要（见第 1 节）。

### 环境变量

所有配置都来自环境变量，启动时校验；缺失或不合法时服务直接退出。

必填：

| 变量 | 作用 |
| --- | --- |
| `DATABASE_URL` | PostgreSQL 连接地址 |
| `PLAINMOTE_PUBLIC_URL` | 对外的 HTTP(S) origin，用于生成分享地址和 OAuth 回调地址 |
| `PLAINMOTE_BLOB_ENDPOINT` | S3 兼容服务端点 |
| `PLAINMOTE_BLOB_BUCKET` | Bucket 名称 |
| `PLAINMOTE_BLOB_ACCESS_KEY` | Access Key ID |
| `PLAINMOTE_BLOB_SECRET_KEY` | Secret Access Key |
| `PLAINMOTE_TOKEN_KEY` | 32 字节的 Token 加密密钥（64 位十六进制或 Base64），见下文 |

可选：

| 变量 | 默认值 | 作用 |
| --- | --- | --- |
| `PLAINMOTE_LISTEN` | `:8964` | HTTP 监听地址 |
| `PLAINMOTE_BLOB_REGION` | `auto` | S3 区域 |
| `PLAINMOTE_TRUSTED_PROXIES` | 空 | 可信代理的 IP 或 CIDR，逗号分隔（见第 5 节） |
| `PLAINMOTE_VISITOR_LOCATION` | 空 | 设为 `cloudflare` 时，按 Cloudflare 的位置请求头在访问记录中记下访问者的大致位置（见第 5 节） |
| `PLAINMOTE_SESSION_TTL` | `720h` | 登录会话有效期 |
| `PLAINMOTE_MAX_CONTENT_MIB` | `10` | 单份内容的大小上限 |
| `PLAINMOTE_LOG_RETENTION` | `720h` | 访问记录保留时长，`0` 表示一直保留 |
| `PLAINMOTE_REGISTRATION_MODE` | `closed` | 是否开放新账号注册：`open` 或 `closed` |
| `GITHUB_CLIENT_ID`、`GITHUB_CLIENT_SECRET` | 空 | GitHub OAuth App；GitHub 与 Google 至少配置一个 |
| `GOOGLE_CLIENT_ID`、`GOOGLE_CLIENT_SECRET` | 空 | Google OAuth 客户端；只请求名称和头像，不获取邮箱 |
| `PLAINMOTE_ANONYMOUS` | `false` | 是否允许不登录直接在首页或 `/paste` 快速分享；每个地址（IPv6 按 /64 计）每小时最多 120 个，每天合计不超过 256 MiB |
| `PLAINMOTE_OPERATOR` | 空 | 运营者名称，显示在关于、隐私政策和服务条款中 |
| `PLAINMOTE_CONTACT_EMAIL` | 空 | 联系邮箱；设置后才有关于、隐私政策、服务条款和联系页面 |
| `PLAINMOTE_SOURCE_URL` | 本仓库地址 | 每页底部“源代码”链接的地址；部署修改过的版本时，按 AGPL 要求指向修改后的源代码 |
| `PLAINMOTE_ADMIN_KEYS` | 空 | 允许调用管理接口的 Ed25519 公钥，Base64，逗号分隔；不设置则没有管理接口 |
| `PLAINMOTE_ADMIN_ORIGINS` | 空 | 允许在浏览器中跨站调用管理接口的管理页面来源，逗号分隔 |
| `PLAINMOTE_CLI_DIR` | `/cli` | `/cli` 下提供的命令行文件所在目录；镜像已包含这些文件 |

### 密钥与页面

生成 Token 加密密钥：

```bash
openssl rand -hex 32
```

`PLAINMOTE_TOKEN_KEY` 要长期备份，所有副本使用同一个值。它不能随意轮换：丢失或更换后，数据库里已有的分享 Token 都无法解密。

管理接口需要设置 `PLAINMOTE_ADMIN_KEYS` 和 `PLAINMOTE_ADMIN_ORIGINS`，密钥生成和管理页面见 [管理](admin.zh-CN.md)。私钥不要放在服务器上。

对外公开的实例应设置 `PLAINMOTE_CONTACT_EMAIL`（以及可选的 `PLAINMOTE_OPERATOR`），以提供隐私政策、服务条款、关于和联系页面。Google 等 OAuth 服务商审核时要求这些页面位于已验证的域名下：`/about` 可作应用首页，`/privacy` 和 `/terms` 分别作隐私政策和服务条款地址。

限制 `.env` 的读取权限：

```bash
chmod 600 .env
```

镜像如果是私有包，先在服务器上登录镜像仓库；GHCR 用带 `read:packages` 权限的 Token：

```bash
printf '%s' "$GHCR_TOKEN" | docker login ghcr.io -u USER --password-stdin
```

## 4. 启动与核验

先检查 Compose 展开后的配置。这条命令只做检查；展开结果含有密钥，不要保存到日志或发给别人：

```bash
docker compose config --quiet
```

拉取并启动：

```bash
docker compose pull
docker compose up -d --remove-orphans
docker compose ps
```

服务默认只发布在宿主机的 `127.0.0.1:8964`。在宿主机上检查：

```bash
curl --fail --silent --show-error http://127.0.0.1:8964/healthz
docker compose logs --tail=100 plainmote
```

`/healthz` 返回 `ok` 只说明 HTTP 进程在响应。PostgreSQL 在启动时已经连上并执行了 schema；对象存储的凭据要靠实际创建、读取、删除一份资源来验证。

## 5. 公网入口

PlainMote 只提供 HTTP，不处理 TLS。用 Cloudflare Tunnel 接入 Cloudflare，把公网 HTTPS 转发到 `127.0.0.1:8964`，并保留 `Host` 和 `X-Forwarded-Proto`。服务从 Cloudflare 设置的 `CF-Connecting-IP` 读取客户端 IP，所以前面不能再放其他代理：会原样转发客户端请求头的代理，等于让访问者自己决定被记录和被限流的 IP。

`PLAINMOTE_TRUSTED_PROXIES` 只填与服务直接相连的代理地址，不要因为是私网就信任整个网段。留空时不读取任何转发头，访问记录里的来源 IP 都是代理自身的地址。用 Docker 或 Tunnel 时，服务看到的直连地址可能是网桥网关：部署后打开一个分享链接，访问记录里出现的来源 IP 就是要填的值；填好后再打开一次，确认记录变成了真实客户端 IP。这个值填错时，代理后的所有客户端在按 IP 限流时都算作同一个地址，一个人的失败尝试会连累所有人；未列入的直连地址发来 `CF-Connecting-IP` 或 `X-Forwarded-For` 时，服务会在日志里提示一次。

在 Cloudflare 中还要：

1. 对 `/paste` 开头的路径（包括 `/paste/encrypted`）设置写入限流。开放匿名分享时必须设置。
2. 对 `/api/v1/device/`、`/oauth/register`、`/oauth/token` 和 `/mcp` 设置限流。服务自身已按 IP 限流（IPv6 按 /64 计），`/mcp` 还按授权限流；入口层限流可以在流量到达服务之前拦下。
3. 按需对 `/d/*` 设置访问限流，但不要启用 `Cache Everything`。
4. 保留服务发出的 `Cache-Control: no-store`，避免边缘缓存绕过撤销和次数限制。
5. 不要再把容器端口直接暴露到公网，否则会出现一个绕过 Tunnel 和限流的入口。
6. 可选：在 Cloudflare 控制台（Rules → Settings）开启托管转换“Add visitor location headers”，并设置 `PLAINMOTE_VISITOR_LOCATION=cloudflare`，访问记录会显示 Cloudflare 按 IP 估算的国家或地区、省级行政区和城市，隐私政策也会相应说明。位置请求头只从 `PLAINMOTE_TRUSTED_PROXIES` 所列地址读取，该列表为空时服务拒绝启动。开启前的记录没有位置。

## 6. 升级与回滚

修改 `.env` 中的 `PLAINMOTE_IMAGE`，然后执行：

```bash
docker compose pull
docker compose up -d --remove-orphans
curl --fail --silent --show-error http://127.0.0.1:8964/healthz
```

查看启动日志，再实际登录、读取一份资源、打开一个分享链接。回滚时把 `PLAINMOTE_IMAGE` 改回之前的版本或摘要，执行同样的命令。

新版本上线后，`/cli` 提供的就是新版本的命令行，用户重新执行安装命令即可升级，已有的登录不受影响。

schema 目前不保证向后兼容。新版本写入了旧版本不认识的数据时，只换回旧镜像并不算完整回滚，所以每次发布前都要备份 PostgreSQL。存放在 S3/R2 的内容也应开启服务商的版本控制或备份。

## 7. 运行约束

- 容器以 UID/GID `65532` 运行，根文件系统只读，只挂载临时的 `/tmp`。
- 收到 SIGTERM 后，服务最多用 10 秒关闭 HTTP；Compose 给出 15 秒退出时间。
- 每个进程最多保持 max(4, CPU 核数) 个 PostgreSQL 连接，可在 `DATABASE_URL` 中用 `pool_max_conns` 调整，例如 `?pool_max_conns=10`。
- 服务不做边缘防护；匿名写入和公开交付的防滥用规则要在入口层配置。`/d/*`、登录页和资源页都不能被 CDN 缓存。
- 多副本必须共享 PostgreSQL、S3/R2 和同一个 `PLAINMOTE_TOKEN_KEY`。以下状态只在单个进程的内存中：各接口和免登录快速分享的限流计数、管理接口的认证失败计数、管理请求已用过的 nonce。因此多副本时限流按副本分别计算，一个已签名的管理请求在有效期内可能被重放到另一个副本。需要多副本时，在入口层补充限流，并把 `/_admin/` 固定路由到同一个副本。

## 8. 时间

数据库用 `timestamptz` 保存时间。建议把 PostgreSQL 的 `timezone` 和 `log_timezone` 都设为 `UTC`，避免日志和手工查询混用本地时区；宿主机的时区不影响服务。页面输出 UTC 时间，由浏览器按自己的时区和界面语言显示；禁用 JavaScript 时直接显示 UTC。

## 9. 磁盘、日志与备份

PostgreSQL 没有适合生产使用的“数据库最大容量”设置。给数据目录加硬配额会让写入在配额耗尽时遇到 `ENOSPC`，可能中断事务、检查点和 WAL 写入。应该监控下面几项，在还有余地时告警：

```sql
SELECT pg_size_pretty(pg_database_size(current_database()));
```

```bash
du -sh /path/to/postgres/data
df -h /
journalctl --disk-usage
```

数据库的逻辑大小不含 WAL、表膨胀、容器日志和备份，只看 SQL 结果不够。以约 20 GiB 的根分区为例，可以从下面的阈值开始，再按增长速度调整：

- 数据库逻辑大小：3 GiB 警告，4 GiB 严重。
- PostgreSQL 数据目录：4 GiB 警告，5 GiB 严重。
- 根分区可用空间：低于 3 GiB 警告，低于 1.5 GiB 严重。

访问记录默认保留 30 天（`PLAINMOTE_LOG_RETENTION`）。普通 `VACUUM` 让删除后的空间可以复用，但不会立刻把文件缩小、把空间还给系统；不要把定期 `VACUUM FULL` 当作日常清理，它会锁表并临时占用额外磁盘。

Compose 已把每个容器的日志限制为 3 个文件、每个 10 MiB。宿主机的 journald 也要设置 `SystemMaxUse` 和 `SystemKeepFree`，否则系统日志仍可能占满根分区。

数据库至少每天执行一次 `pg_dump -Fc`，并用 `pg_restore --list` 确认备份可读。和数据库在同一块磁盘上的备份只能应对误删和逻辑损坏，应对不了磁盘或 VPS 丢失；生产备份还要用单独的凭据复制到另一台机器或独立的对象存储。
