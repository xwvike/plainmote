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
