# PlainMote 部署

生产容器只运行 PlainMote Web 服务。PostgreSQL、S3 兼容对象存储、TLS 和公网入口均在容器外提供；
容器本地没有需要保留的数据，也不读取配置文件。

## 1. 构建镜像

在仓库根目录构建当前机器架构的本地镜像：

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

部署到普通 x86 Linux 服务器时，也可以明确构建 amd64 镜像：

```bash
test -z "$(git status --porcelain)" || { echo 'working tree is not clean'; exit 1; }
VERSION="$(git rev-parse --short=12 HEAD)"
docker buildx build \
  --load \
  --platform linux/amd64 \
  --build-arg VERSION="$VERSION" \
  --build-arg REVISION="$(git rev-parse HEAD)" \
  --tag "plainmote:$VERSION" \
  .
```

发布到 GHCR，并同时生成 amd64 与 arm64 镜像：

```bash
export IMAGE=ghcr.io/xwvike/plainmote
export VERSION="$(git rev-parse --short=12 HEAD)"

test -z "$(git status --porcelain)" || { echo 'working tree is not clean'; exit 1; }
printf '%s' "$GHCR_TOKEN" | docker login ghcr.io -u xwvike --password-stdin
docker buildx build \
  --push \
  --platform linux/amd64,linux/arm64 \
  --build-arg VERSION="$VERSION" \
  --build-arg REVISION="$(git rev-parse HEAD)" \
  --tag "$IMAGE:$VERSION" \
  .
```

确认这个版本可以部署后，再单独增加 `latest` 标签。部署文件应始终使用提交版本标签或镜像摘要，不能依赖
`latest`，否则无法确认当前运行的代码，也无法可靠回滚。

```bash
docker buildx imagetools create \
  --tag "$IMAGE:latest" \
  "$IMAGE:$VERSION"
```

镜像摘要比标签更严格，标签被重新推送后摘要仍然指向原来的内容。发布后可查询摘要：

```bash
docker buildx imagetools inspect "$IMAGE:$VERSION"
```

需要完全固定构建产物时，在服务器 `.env` 中使用输出的 manifest list 摘要：

```dotenv
PLAINMOTE_IMAGE=ghcr.io/xwvike/plainmote@sha256:DIGEST
```

GHCR Token 至少需要 `write:packages`；私有镜像的部署机器需要 `read:packages`。镜像使用
OCI 标签记录源码仓库、版本和完整提交哈希，可以通过以下命令核对：

```bash
docker image inspect "plainmote:$VERSION" \
  --format '{{json .Config.Labels}}'
```

不使用镜像仓库时，可以把单架构镜像导出后传到服务器：

```bash
docker save "plainmote:$VERSION" | gzip > "plainmote-$VERSION.tar.gz"
```

服务器导入后，把 `.env` 中的 `PLAINMOTE_IMAGE` 设置为 `plainmote:VERSION`：

```bash
gzip -dc "plainmote-$VERSION.tar.gz" | docker load
```

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

## 3. 配置部署目录

服务器只需要 `compose.yaml` 与 `.env`，不需要克隆源码：

```text
/opt/plainmote/
  compose.yaml
  .env
```

复制 [`.env.example`](../.env.example) 为 `.env`，填写全部必填项，并把镜像固定到刚刚发布的版本或摘要：

```dotenv
PLAINMOTE_IMAGE=ghcr.io/xwvike/plainmote:COMMIT_TAG
```

生成 Token 加密密钥：

```bash
openssl rand -hex 32
```

`PLAINMOTE_TOKEN_KEY` 必须长期备份，并在所有副本之间保持一致。它不是可以随时轮换的登录密码；丢失或替换后，
数据库里已有的分享 Token 将无法解密。

限制 `.env` 的读取权限：

```bash
chmod 600 .env
```

如果镜像为私有包，先在部署机器登录 GHCR：

```bash
printf '%s' "$GHCR_TOKEN" | docker login ghcr.io -u xwvike --password-stdin
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
Docker 或 Tunnel 场景下，应用看到的直连地址可能是网桥网关；部署后访问一个分享地址，再从访问记录核对来源 IP。

使用 Cloudflare 时还需要：

1. 对 `/paste` 设置写入限流；开放匿名分享时这是必须项。
2. 根据需要对 `/d/*` 设置访问限流，但绝不能启用 `Cache Everything`。
3. 保留应用发出的 `Cache-Control: no-store`，确保撤销和次数限制不会被边缘缓存绕过。
4. 不要把容器端口同时发布到公网，否则会形成绕过 Tunnel 和限流规则的入口。

## 6. 升级与回滚

每次发布都构建新的提交标签。升级时修改 `.env` 中的 `PLAINMOTE_IMAGE`，然后执行：

```bash
docker compose pull
docker compose up -d --remove-orphans
curl --fail --silent --show-error http://127.0.0.1:8964/healthz
```

查看启动日志并完成一次登录、资源读取和分享访问。回滚时把 `PLAINMOTE_IMAGE` 改回上一个提交标签，再执行相同命令。

数据库 schema 当前没有向后兼容保证。若新版本已经写入旧版本不能理解的数据，单纯切换旧镜像不构成完整回滚；
发布前必须同时保留 PostgreSQL 备份。对象正文存放在 S3/R2，也应配置服务商侧的版本控制或备份策略。

## 7. 运行约束

- 容器以 UID/GID `65532` 运行，根文件系统只读，仅挂载临时 `/tmp`。
- Compose 将容器日志限制为 3 个、每个 10 MiB，避免 Docker JSON 日志无限占用宿主机磁盘。
- 收到 SIGTERM 后，服务最多使用 10 秒完成 HTTP 关闭；Compose 给出 15 秒退出时间。
- 多副本必须共享 PostgreSQL、S3/R2 和同一份 `PLAINMOTE_TOKEN_KEY`。
- 应用不承担边缘限流；匿名写入和公开交付的抗攻击规则必须在入口层配置。
- `/d/*`、登录页面和资源页面都不应被 CDN 缓存。
