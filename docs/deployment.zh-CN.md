# PlainMote 部署

[English](deployment.md) | 简体中文

生产容器只运行 PlainMote Web 服务。PostgreSQL、S3 兼容对象存储、TLS 和公网入口均在容器外提供；
容器本地没有需要保留的数据，也不读取配置文件。

## 1. 获取镜像

每个版本都以公开镜像发布到 GitHub Container Registry，包含 `linux/amd64` 与 `linux/arm64`：

| 标签 | 指向 |
| --- | --- |
| `ghcr.io/xwvike/plainmote:v1.2.3` | 一个发布版本 |
| `ghcr.io/xwvike/plainmote:<commit>` | 构建所用提交哈希的前 12 位 |
| `ghcr.io/xwvike/plainmote:latest` | 最新的发布版本，仅供试用 |

部署文件应始终使用版本标签、提交标签或镜像摘要，不能依赖 `latest`，否则无法确认当前运行的代码，也无法可靠回滚。
镜像摘要比标签更严格，标签被移动后摘要仍然指向原来的内容。查询摘要：

```bash
docker buildx imagetools inspect ghcr.io/xwvike/plainmote:v1.2.3
```

并在服务器 `.env` 中固定：

```dotenv
PLAINMOTE_IMAGE=ghcr.io/xwvike/plainmote@sha256:DIGEST
```

镜像由仓库的 [发布工作流](../.github/workflows/release.yml) 在 GitHub Actions 中构建，不在任何人的机器上构建，并附带经签名的构建来源证明。
核对镜像确由本仓库的该工作流构建：

```bash
gh attestation verify oci://ghcr.io/xwvike/plainmote@sha256:DIGEST --owner xwvike
```

镜像同时使用 OCI 标签记录源码仓库、版本和完整提交哈希：

```bash
docker image inspect ghcr.io/xwvike/plainmote:v1.2.3 \
  --format '{{json .Config.Labels}}'
```

构建过程同时为 macOS、Linux 和 Windows 的 amd64 与 arm64 编译 `plainmote` 命令行，放在镜像的 `/cli` 目录中，
由服务通过 `/cli` 和开发者页面 `/developers/cli` 提供下载；版本号同时写入服务端与命令行，二者版本始终一致。命令行文件使镜像增加约
35 MiB。

### 发布版本

在 `main` 上已推送的提交打标签并推送：

```bash
git tag v1.2.3
git push origin v1.2.3
```

工作流发布 `v1.2.3` 与提交标签；版本号不带 `-rc.1` 等后缀时，同时更新 `latest`。运行摘要中给出写入 `.env` 的镜像摘要。
手动运行工作流时只发布提交标签。工作流只使用仓库自身的 `GITHUB_TOKEN`，不持有任何服务器的凭据，镜像由服务器自行拉取。
首次发布后，应在 GitHub 上该包的设置中确认其为公开；私有包要求每台服务器在拉取前登录（见第 3 节）。

### 自行构建

在仓库根目录构建当前机器架构的本地镜像，例如修改过的版本：

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

在其他机器上为普通 x86 服务器构建时，加上 `--platform linux/amd64`。发布双架构镜像到自己的镜像仓库时，把 `--load`
换成 `--push --platform linux/amd64,linux/arm64`，并以该仓库的地址作为标签。不使用镜像仓库时，可以把单架构镜像导出后传到服务器：

```bash
docker save "plainmote:$VERSION" | gzip > "plainmote-$VERSION.tar.gz"
```

服务器导入后，把 `.env` 中的 `PLAINMOTE_IMAGE` 设置为 `plainmote:VERSION`：

```bash
gzip -dc "plainmote-$VERSION.tar.gz" | docker load
```

部署修改过的代码时，须把 `PLAINMOTE_SOURCE_URL` 指向修改后的源代码（AGPL-3.0）；发布到 GHCR 时，还应修改 `Dockerfile`
中的 `org.opencontainers.image.source` 标签，GHCR 据此把镜像包关联到仓库。

## 2. 准备外部服务

### PostgreSQL

创建独立数据库和账号，并生成带 TLS 参数的连接 URI：

```text
postgres://plainmote:PASSWORD@postgres.example.com:5432/plainmote?sslmode=require
```

密码中的 `@`、`:`、`/` 等字符必须进行 URL 编码。应用启动时会执行内嵌的 `schema.sql`，因此当前版本的
数据库账号需要连接、建表、建索引和读写权限。项目处于快速迭代期，不提供旧 schema 的自动迁移兼容层；
升级前应先检查 schema 变更并备份数据库。

### S3 或 Cloudflare R2

提前创建 Bucket，并为应用凭据授予该 Bucket 的读取、写入和删除对象权限。Cloudflare R2 的端点格式为：

```text
https://ACCOUNT_ID.r2.cloudflarestorage.com
```

R2 使用 `PLAINMOTE_BLOB_REGION=auto`。Bucket 不需要公开访问，所有正文都由 PlainMote 根据分享规则读取并转发。

### GitHub OAuth App

在 GitHub 创建独立的 OAuth App：

- Application name：`PlainMote`
- Homepage URL：生产环境的 `PLAINMOTE_PUBLIC_URL`
- Authorization callback URL：`PLAINMOTE_PUBLIC_URL/auth/github/callback`

授权页面展示的名称来自这个 OAuth App。应由项目或组织账号持有该 App；使用个人账号创建时，GitHub 仍可能
在授权信息中展示该账号的归属关系。

### Google OAuth 客户端

Google 登录为可选项，可与 GitHub 同时启用，也可单独使用。在 Google Cloud Console 的 Google Auth Platform 中：

1. **品牌（Branding）**：填写应用名称、支持邮箱、首页（`PLAINMOTE_PUBLIC_URL/about`）、隐私政策（`/privacy`）和服务条款（`/terms`），并把域名加入已授权网域。该域名须在同一 Google 账号的 Google Search Console 中完成验证。法律页面仅在设置 `PLAINMOTE_CONTACT_EMAIL` 后提供。
2. **受众（Audience）**：选择 External。应用处于测试状态时只有列出的测试用户可以登录，发布到正式环境后对所有人开放。
3. **数据访问（Data access）**：只添加 `openid` 与 `.../auth/userinfo.profile`。PlainMote 不请求其他权限，包括邮箱；这些范围无需应用验证。
4. **客户端（Clients）**：新建 Web 应用类型的客户端，已获授权的重定向 URI 为 `PLAINMOTE_PUBLIC_URL/auth/google/callback`。Client Secret 只在创建时显示，应当场保存。
5. 可选：提交品牌验证，通过后同意页面显示应用名称。

设置 `GOOGLE_CLIENT_ID` 与 `GOOGLE_CLIENT_SECRET`。账号以 Google 账号 ID（`sub`）识别；用 GitHub 和用 Google 创建的账号互相独立，所有者可以在设置中的“登录方式”页绑定另一种登录方式。

### 注册

`PLAINMOTE_REGISTRATION_MODE` 决定从未登录过的 GitHub 或 Google 账号能否创建账号：`open` 或 `closed`（默认）。已有账号不受影响，修改后重启即生效，可随时切换。新部署时先设为 `open`，登录创建自己的账号；如不希望他人注册，再改回 `closed`。

某种登录方式只有在 Client ID 与 Secret 都设置时才会提供。缺少其中一项时，不显示对应按钮，并在启动日志中说明；两种方式都不完整时，服务不会启动。

## 3. 配置部署目录

服务器只需要 `compose.yaml` 与 `.env`，不需要克隆源码：

```text
/opt/plainmote/
  compose.yaml
  .env
```

复制 [`.env.example`](../.env.example) 为 `.env`，填写全部必填项，并把镜像固定到某个版本或摘要（见第 1 节）：

```dotenv
PLAINMOTE_IMAGE=ghcr.io/xwvike/plainmote:v1.2.3
```

生成 Token 加密密钥：

```bash
openssl rand -hex 32
```

`PLAINMOTE_TOKEN_KEY` 必须长期备份，并在所有副本之间保持一致。它不是可以随时轮换的登录密码；丢失或替换后，
数据库里已有的分享 Token 将无法解密。

启用管理接口时设置 `PLAINMOTE_ADMIN_KEYS` 与 `PLAINMOTE_ADMIN_ORIGINS`，密钥的生成与管理页面的使用见
[管理](admin.zh-CN.md)。私钥不应放在服务器上。

需要对外公开的实例应设置 `PLAINMOTE_CONTACT_EMAIL`（可同时设置 `PLAINMOTE_OPERATOR`），以提供隐私政策、
服务条款、关于和联系页面。接入 Google 等第三方 OAuth 时，审核要求这些页面位于已验证的域名下；
`https://<域名>/about` 可以作为应用首页，`/privacy` 和 `/terms` 分别填入隐私政策和服务条款地址。

限制 `.env` 的读取权限：

```bash
chmod 600 .env
```

如果镜像为私有包，先在部署机器登录其镜像仓库；GHCR 使用具有 `read:packages` 权限的 Token：

```bash
printf '%s' "$GHCR_TOKEN" | docker login ghcr.io -u USER --password-stdin
```

## 4. 启动与核验

先检查 Compose 展开后的配置。该命令只检查，不要把包含 Secret 的展开结果保存到日志或发给其他人：

```bash
docker compose config --quiet
```

拉取并启动固定版本镜像：

```bash
docker compose pull
docker compose up -d --remove-orphans
docker compose ps
```

应用默认只发布到宿主机 `127.0.0.1:8964`。从宿主机检查进程：

```bash
curl --fail --silent --show-error http://127.0.0.1:8964/healthz
docker compose logs --tail=100 plainmote
```

`/healthz` 返回 `ok` 只表示 HTTP 进程正在响应。应用在启动时已经连接 PostgreSQL 并执行 schema；S3/R2
凭据则应通过创建、读取和删除一份测试资源完成部署验收。

## 5. 公网入口

PlainMote 自身提供 HTTP，不终止 TLS。使用 Nginx、Caddy 或 Cloudflare Tunnel 将公网 HTTPS origin 转发到
`127.0.0.1:8964`，并保留 `Host`、`X-Forwarded-Proto` 和真实客户端 IP 头。

只有确实与应用直接连接的代理地址才能写入 `PLAINMOTE_TRUSTED_PROXIES`。不要因为某个网段是私网就整体信任它。
留空时不读取任何转发头，访问记录中的来源 IP 全部是代理自身的地址。
Docker 或 Tunnel 场景下，应用看到的直连地址可能是网桥网关；部署后访问一个分享地址，访问记录中出现的来源 IP 就是应填写的值，填写后再访问一次，确认记录变为真实客户端 IP。

使用 Cloudflare 时还需要：

1. 对以 `/paste` 开头的路径（包括 `/paste/encrypted`）设置写入限流；开放匿名分享时这是必须项。
2. 对 `/api/v1/device/` 设置限流。应用自身已按 IP 限制申请验证码与轮询的频率，入口层限流可以在流量到达应用前拦截。
3. 根据需要对 `/d/*` 设置访问限流，但绝不能启用 `Cache Everything`。
4. 保留应用发出的 `Cache-Control: no-store`，确保撤销和次数限制不会被边缘缓存绕过。
5. 不要把容器端口同时发布到公网，否则会形成绕过 Tunnel 和限流规则的入口。
6. 如需在访问记录中显示访问者的大致位置，可在 Cloudflare 控制台（Rules → Settings）开启托管转换“Add visitor location headers”，并设置 `PLAINMOTE_VISITOR_LOCATION=cloudflare`。开启后，应用记录 Cloudflare 根据客户端 IP 估算的国家或地区、省级行政区和城市，隐私政策同步说明这一点。应用只读取来自 `PLAINMOTE_TRUSTED_PROXIES` 所列地址的位置请求头；该列表为空时设置此项，应用将拒绝启动。不要在其他代理后设置此项：会原样转发客户端请求头的代理，会让访问者自行决定记录的位置。开启前产生的访问记录不含位置。

## 6. 升级与回滚

每个版本都有各自的标签与摘要（见第 1 节）。升级时修改 `.env` 中的 `PLAINMOTE_IMAGE`，然后执行：

```bash
docker compose pull
docker compose up -d --remove-orphans
curl --fail --silent --show-error http://127.0.0.1:8964/healthz
```

查看启动日志并完成一次登录、资源读取和分享访问。回滚时把 `PLAINMOTE_IMAGE` 改回上一个版本或摘要，再执行相同命令。

命令行随镜像一同更新：新版本上线后，`/cli` 提供的就是新版本的命令行，用户重新执行安装命令即可升级，已有的登录不受影响。

数据库 schema 当前没有向后兼容保证。若新版本已经写入旧版本不能理解的数据，单纯切换旧镜像不构成完整回滚；
发布前必须同时保留 PostgreSQL 备份。对象正文存放在 S3/R2，也应配置服务商侧的版本控制或备份策略。

## 7. 运行约束

- 容器以 UID/GID `65532` 运行，根文件系统只读，仅挂载临时 `/tmp`。
- Compose 将容器日志限制为 3 个、每个 10 MiB，避免 Docker JSON 日志无限占用宿主机磁盘。
- 收到 SIGTERM 后，服务最多使用 10 秒完成 HTTP 关闭；Compose 给出 15 秒退出时间。
- 多副本必须共享 PostgreSQL、S3/R2 和同一份 `PLAINMOTE_TOKEN_KEY`。
- 以下状态只保存在单个进程的内存中：命令行接口与设备授权的限流计数、管理接口的认证失败计数，以及管理请求已使用的
  nonce。多副本部署时，限流按副本分别计算；同一个已签名的管理请求在有效期内可能被重放到另一个副本。需要多副本时，
  应在入口层补充限流，并将 `/_admin/` 的请求固定路由到同一个副本。
- 应用不承担边缘限流；匿名写入和公开交付的抗攻击规则必须在入口层配置。
- `/d/*`、登录页面和资源页面都不应被 CDN 缓存。

## 8. 时间与时区

数据库中的业务时间使用 `timestamptz` 保存，应用写入和比较的都是同一个绝对时间。生产 PostgreSQL 建议把
`timezone` 与 `log_timezone` 都设为 `UTC`，这样数据库日志、人工查询和跨地区排障不会混用服务器本地时区。
宿主机可以继续使用运维人员习惯的时区；它只影响 systemd 定时器和宿主机日志的显示。

页面输出包含 UTC 时间点的 `<time datetime="...">`，浏览器再使用自己的时区和当前界面语言完成格式化。因此日本用户
会看到日本时间，其他地区用户也不需要在账号中单独设置时区。浏览器禁用 JavaScript 时，页面会明确显示 UTC 作为
后备值。切换界面语言只改变日期的展示格式，不改变时间点或浏览器时区。

## 9. 磁盘、日志与备份

PostgreSQL 没有适合生产环境的“数据库最大 5 GiB”配置。给数据目录设置硬文件系统配额会让写入在配额耗尽时直接
遇到 `ENOSPC`，可能中断事务、检查点和 WAL 写入。应同时监控以下三个值，并在仍有处理空间时预警：

```sql
SELECT pg_size_pretty(pg_database_size(current_database()));
```

```bash
du -sh /path/to/postgres/data
df -h /
journalctl --disk-usage
```

数据库逻辑大小不包含 WAL、表膨胀、容器日志和备份，所以不能只看 SQL 查询结果。对于约 20 GiB 的根分区，可以从
下面的阈值开始，再根据增长速度调整：

- 数据库逻辑大小达到 3 GiB 时警告，4 GiB 时严重告警。
- PostgreSQL 数据目录达到 4 GiB 时警告，5 GiB 时严重告警。
- 根分区可用空间低于 3 GiB 时警告，低于 1.5 GiB 时严重告警。

`PLAINMOTE_LOG_RETENTION` 默认只保留 30 天访问记录。普通 `VACUUM` 会让删除后的空间供 PostgreSQL 重用，但不会立即
把文件缩小并归还给操作系统；不要把定期 `VACUUM FULL` 当作日常清理方案，因为它会锁表并额外占用临时磁盘空间。

Compose 已把每个容器的 JSON 日志限制为 3 个、每个 10 MiB。宿主机的 journald 也应设置 `SystemMaxUse` 和
`SystemKeepFree`，否则系统服务日志仍可能持续占用根分区。

数据库至少每天执行一次 `pg_dump -Fc`，并用 `pg_restore --list` 验证备份可读。保存在数据库同一块磁盘上的备份只能
应对误删或逻辑损坏，不能应对磁盘或 VPS 丢失；生产备份还应使用独立凭据复制到另一台机器或独立对象存储。
