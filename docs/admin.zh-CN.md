# 管理

[English](admin.md) | 简体中文

PlainMote 不包含管理页面。运营者通过一组以 Ed25519 签名保护的 HTTP 接口管理服务，管理页面独立于服务部署，可以运行在任何地方，包括本机。服务端只保存公钥，签名所需的私钥由运营者持有。

本文前半部分介绍管理功能及其对用户的影响，后半部分是完整的接口参考。

## 功能

| 范围 | 可进行的操作 |
| --- | --- |
| 部署概况 | 查看版本、主要配置、数据库与对象存储的状态、用户与资源的数量及容量、分享链接与访问的统计、最近一次定时清理的结果 |
| 账号 | 按登录名或 GitHub ID 查找；查看资源数量、容量与生效中的套餐；停用与恢复 |
| 套餐 | 新建与删除套餐；为账号授予或撤回套餐，可设置到期时间 |
| 资源 | 按所有者、名称或状态查找；查看元数据与分享链接列表；下架与撤销下架；删除 |
| 分享链接 | 由举报中的分享地址找到对应的资源与链接；撤销单条链接 |
| 审计记录 | 查看所有经管理接口进行的变更 |

管理接口有以下限制：

- **不接触内容**：任何接口都不返回资源正文、历史版本正文、分享链接地址或访问记录中的访问者信息（IP 地址与客户端信息）。运营者只能看到元数据与汇总数字；远程资源只给出源地址的主机名。
- **变更必须说明原因并留痕**：每一项变更都须附带原因，并写入只追加的审计记录，记录时间、所用密钥、操作、对象、原因及来源 IP。审计记录不能修改或删除，也不随访问记录的保留期清理。
- **注册白名单不在其中**：`allowlist` 模式下允许注册的账号由 `GITHUB_ALLOWED_IDS` 在启动时决定。

### 对用户的影响

| 操作 | 用户看到的效果 |
| --- | --- |
| 停用账号 | 该账号的全部登录会话与命令行登录立即失效，再次登录时登录页显示停用原因；其全部分享链接停止交付。数据不删除 |
| 恢复账号 | 账号可以再次登录；分享链接按各自原有的有效期与次数继续生效；命令行需要重新登录 |
| 下架资源 | 该资源的全部分享链接停止交付；所有者在资源页看到下架原因，资源列表中显示为“已下架”，不能为其新建分享链接，也不能将其历史版本另存为新资源，但可以编辑或删除该资源。未登录创建的快速分享没有可通知的所有者，下架即删除；登录用户的快速分享与资源相同 |
| 撤销下架 | 分享链接按各自原有的有效期与次数继续生效 |
| 删除资源 | 与所有者自行删除效果相同：正文、历史版本与分享链接一并删除，访问记录保留 |
| 撤销链接 | 与所有者撤销效果相同 |

对已停用账号或已下架资源的分享链接，公开访问一律返回 `401`，与链接失效时相同，不说明原因；所有者的访问记录中分别记为“账号已停用”与“已下架”。

## 启用

### 配置

| 变量 | 默认值 | 作用 |
| --- | --- | --- |
| `PLAINMOTE_ADMIN_KEYS` | 空 | 允许调用管理接口的 Ed25519 公钥，Base64 编码的 32 字节原始公钥，多个以逗号分隔。更换密钥期间可新旧并存。为空时管理接口不存在，所有路径返回 `404` |
| `PLAINMOTE_ADMIN_ORIGINS` | 空 | 允许在浏览器中跨域调用管理接口的页面来源，逗号分隔，例如 `http://localhost:5173,https://admin.example.com`。为空时不响应跨域请求 |

使用 Docker Compose 部署时，`compose.yaml` 已将这两个变量传入容器，在 `.env` 中设置即可。

### 密钥

仓库内的 `plainmote-admin` 工具生成密钥对：

```bash
go run ./cmd/plainmote-admin keygen
# key id:  <公钥标识>
# public:  <Base64 公钥>   填入 PLAINMOTE_ADMIN_KEYS
# private: <JWK 私钥>      由管理页面导入，或保存为文件供 call 使用；不要放在服务器上
```

私钥应在运营者自己的设备上生成和保存。服务器环境变量泄露只会暴露公钥，不足以伪造管理请求。

### 管理页面

[PlainMote Admin](https://github.com/xwvike/plainmote-admin)（MIT）是基于本接口的管理页面，完全在浏览器中运行，私钥以不可导出的形式保存，可同时管理多个部署。该页面可在本机运行、部署为静态站点，或使用已发布的地址 [xwvike.github.io/plainmote-admin](https://xwvike.github.io/plainmote-admin/)。页面所在的来源须列入 `PLAINMOTE_ADMIN_ORIGINS`。浏览器按来源保存私钥，同一来源下的任何脚本都可以使用它，因此该页面应部署在不承载其他内容的来源上。

### 命令行调用

不使用管理页面时，`plainmote-admin call` 可为单个请求签名并发送：

```bash
go run ./cmd/plainmote-admin call -key admin.jwk GET https://plainmote.link/_admin/v1/overview
go run ./cmd/plainmote-admin call -key admin.jwk POST https://plainmote.link/_admin/v1/lookup '{"link":"https://plainmote.link/d/…"}'
```

## 接口参考

### 通用约定

- 路径前缀为 `/_admin/v1/`。请求体与响应体均为 JSON，请求体不超过 64 KiB。
- 时间为 RFC 3339 格式的 UTC 时间，大小以字节为单位，ID 为 UUID。
- 列表接口接受查询参数 `page`（从 1 开始，默认 1）与 `size`（1–100，默认 20），返回 `{"items": [...], "total": <总数>}`。
- 进行变更的接口要求请求体包含 `reason` 字段，长度为 1–500 个字符，写入审计记录；停用账号与下架资源的原因会显示给用户。

### 认证

每个请求须携带以下请求头：

| 请求头 | 内容 |
| --- | --- |
| `X-PlainMote-Key` | 公钥标识：公钥 SHA-256 摘要的前 16 个十六进制字符 |
| `X-PlainMote-Timestamp` | Unix 时间，单位为秒 |
| `X-PlainMote-Nonce` | 至少 16 字节的随机数，Base64url 编码（不含填充），不超过 128 个字符 |
| `X-PlainMote-Signature` | 对签名串的 Ed25519 签名，Base64url 编码（不含填充） |

签名串由以下六行以 `\n` 连接而成，末尾没有换行：

```
PLAINMOTE-ADMIN-V1
<请求方法，大写>
<请求路径与查询串，与实际发送的完全一致>
<X-PlainMote-Timestamp 的值>
<X-PlainMote-Nonce 的值>
<请求体的 SHA-256 摘要，小写十六进制；无请求体时为空字符串的摘要>
```

以下请求被拒绝：时间戳与服务器时间相差超过 300 秒；同一 nonce 在 660 秒内再次出现；公钥标识不在 `PLAINMOTE_ADMIN_KEYS` 中；签名无效；请求体超过 64 KiB。只有通过验证的请求才会消耗 nonce；已使用的 nonce 保存在服务进程内存中，进程重启后清空。

认证失败时返回与不存在的路径相同的 `404`（纯文本）。同一来源 IP 每分钟认证失败超过 30 次后，在该分钟内的所有请求都返回 `404`。

浏览器中可使用 WebCrypto 的 Ed25519（Chrome 137+、Firefox 129+、Safari 17+）。私钥应以 `extractable: false` 导入，保存在当前会话中，或以 `CryptoKey` 对象存入 IndexedDB，不应以明文形式保存。

### 跨域

仅当请求的 `Origin` 列于 `PLAINMOTE_ADMIN_ORIGINS` 时，服务端响应 `OPTIONS` 预检（`204`），并在响应中返回：

- `Access-Control-Allow-Origin`：请求的来源；
- `Access-Control-Allow-Methods`：`GET, POST, DELETE`；
- `Access-Control-Allow-Headers`：`Content-Type` 与上述四个认证请求头；
- `Access-Control-Max-Age`：`600`。

管理接口不使用 Cookie，不返回 `Access-Control-Allow-Credentials`。

### 错误

通过认证的请求出错时返回 `{"error": "<代码>", "message": "<说明>"}`：

| 状态码 | 代码 | 含义 |
| --- | --- | --- |
| `400` | `bad_request` | 请求体或查询参数不合法 |
| `400` | `bad_reason` | 缺少 `reason`，或其长度不在 1–500 个字符之间 |
| `404` | `not_found` | 对象或接口不存在 |
| `409` | `already_suspended`、`not_suspended` | 账号已处于该状态 |
| `409` | `already_taken_down`、`not_taken_down` | 资源已处于该状态 |
| `409` | `already_revoked` | 链接已撤销 |
| `409` | `plan_exists`、`invalid_plan` | 套餐名称重复，或名称、上限不合法 |
| `409` | `plan_protected`、`plan_in_use` | 默认套餐不能删除、授予或撤回；仍有账号持有的套餐不能删除 |
| `500` | `internal` | 服务端故障，详情只写入服务端日志 |

### 接口一览

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `GET` | `/_admin/v1/overview` | 部署概况 |
| `GET` | `/_admin/v1/users` | 账号列表 |
| `GET` | `/_admin/v1/users/{id}` | 账号详情 |
| `POST` | `/_admin/v1/users/{id}/suspend` | 停用账号 |
| `POST` | `/_admin/v1/users/{id}/unsuspend` | 恢复账号 |
| `POST` | `/_admin/v1/users/{id}/plans` | 授予套餐 |
| `DELETE` | `/_admin/v1/users/{id}/plans/{plan_id}` | 撤回套餐 |
| `GET` | `/_admin/v1/plans` | 套餐列表 |
| `POST` | `/_admin/v1/plans` | 新建套餐 |
| `DELETE` | `/_admin/v1/plans/{id}` | 删除套餐 |
| `GET` | `/_admin/v1/resources` | 资源列表 |
| `GET` | `/_admin/v1/resources/{id}` | 资源详情 |
| `GET` | `/_admin/v1/resources/{id}/links` | 资源的分享链接 |
| `POST` | `/_admin/v1/resources/{id}/takedown` | 下架资源 |
| `POST` | `/_admin/v1/resources/{id}/restore` | 撤销下架 |
| `DELETE` | `/_admin/v1/resources/{id}` | 删除资源 |
| `POST` | `/_admin/v1/links/{id}/revoke` | 撤销链接 |
| `POST` | `/_admin/v1/lookup` | 由分享地址查找资源与链接 |
| `GET` | `/_admin/v1/audit` | 审计记录 |

### 数据对象

#### 账号

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `id` | string | 账号 ID |
| `github_id` | string | GitHub 数字用户 ID |
| `login` | string | GitHub 登录名 |
| `name` | string | GitHub 显示名称 |
| `created_at` | time | 注册时间 |
| `last_signed_in_at` | time \| null | 最近一次登录时间 |
| `status` | string | `active` 或 `suspended` |
| `suspended_reason` | string | 停用原因，未停用时为空 |
| `resources` | integer | 资源数量 |
| `resources_limit` | integer | 资源数量上限 |
| `live_links` | integer | 有效分享链接数量 |
| `storage.current_bytes` | integer | 当前内容占用的容量 |
| `storage.history_bytes` | integer | 历史版本占用的容量 |
| `storage.limit_bytes` | integer | 容量上限 |
| `plans` | array | 仅账号详情返回：生效中的套餐，见下表 |

`resources_limit` 与 `storage.limit_bytes` 为当前生效的全部套餐之和，已计入授予时间与到期时间。`plans` 的每一项：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `plan_id` | string | 套餐 ID |
| `name` | string | 套餐名称 |
| `default` | boolean | 是否为默认套餐 |
| `granted_at` | time | 授予时间 |
| `expires_at` | time \| null | 到期时间，`null` 表示不到期 |

#### 套餐

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `id` | string | 套餐 ID |
| `name` | string | 名称 |
| `max_resources` | integer | 资源数量上限 |
| `max_storage` | integer | 容量上限 |
| `default` | boolean | 是否为默认套餐；每个账号都持有默认套餐 |
| `users` | integer | 持有该套餐的账号数量 |

#### 资源

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `id` | string | 资源 ID |
| `owner.id`、`owner.login` | string | 所有者；未登录创建的快速分享 `owner.login` 为 `anonymous` |
| `name` | string | 名称 |
| `filename` | string | 文件名 |
| `kind` | string | `stored`（保存在本服务的内容）或 `remote`（远程资源） |
| `content_type` | string | 内容类型；端到端加密的资源为 `application/vnd.plainmote.sealed`，其 `name` 与 `filename` 为空 |
| `size` | integer | 当前内容的大小 |
| `origin_host` | string | 远程资源源地址的主机名；不返回完整地址 |
| `version` | integer | 当前版本号 |
| `history_versions` | integer | 历史版本数量 |
| `history_bytes` | integer | 历史版本占用的容量 |
| `live_links` | integer | 有效分享链接数量 |
| `created_at`、`updated_at` | time | 创建与更新时间 |
| `status` | string | `active` 或 `taken_down` |
| `takedown_reason` | string | 下架原因，未下架时为空 |

#### 分享链接

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `id` | string | 链接 ID |
| `name` | string | 备注 |
| `created_at` | time | 创建时间 |
| `expires_at` | time \| null | 到期时间，`null` 表示不到期 |
| `max_uses` | integer | 使用次数上限，`0` 表示不限 |
| `used_count` | integer | 已使用次数 |
| `revoked_at` | time \| null | 撤销时间 |
| `live` | boolean | 当前是否有效 |

分享链接的 token 与地址在任何接口中都不返回。

#### 审计记录

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `id` | string | 记录 ID |
| `at` | time | 操作时间 |
| `key` | string | 所用公钥的标识 |
| `action` | string | 操作，见下表 |
| `target_type` | string | `user`、`plan`、`resource` 或 `link` |
| `target_id` | string | 对象 ID |
| `target_label` | string | 操作时对象的名称快照，对象删除后仍保留：账号为登录名，资源为名称（无名称时为文件名），套餐为名称，链接为备注（无备注时为所属资源的名称） |
| `reason` | string | 填写的原因 |
| `detail` | object \| null | 操作的补充信息，见下表 |
| `remote_ip` | string | 请求的来源 IP |

| `action` | `detail` |
| --- | --- |
| `user.suspend`、`user.unsuspend` | `null` |
| `user.plan.grant` | `{"plan_id", "plan_name", "expires_at"}` |
| `user.plan.revoke` | `{"plan_id", "plan_name"}` |
| `plan.create`、`plan.delete` | `{"max_resources", "max_storage"}` |
| `resource.takedown` | `null`；未登录创建的快速分享因下架而被删除时为 `{"deleted": true}` |
| `resource.restore`、`resource.delete` | `null` |
| `link.revoke` | `{"resource_id"}` |

### 部署概况

`GET /_admin/v1/overview`

```json
{
  "version": "adbcdce8a464",
  "revision": "adbcdce8a464…",
  "started_at": "2026-10-03T03:42:30Z",
  "config": {
    "public_url": "https://plainmote.link",
    "registration_mode": "allowlist",
    "anonymous": true,
    "max_content_bytes": 10485760,
    "log_retention_hours": 720,
    "history_keep": 10,
    "history_retention_days": 30
  },
  "health": { "database": "ok", "object_storage": "ok" },
  "users": { "total": 12, "suspended": 0, "signed_in_last_30d": 7 },
  "resources": { "total": 84, "remote": 3, "taken_down": 0, "current_bytes": 5996544, "history_bytes": 8540160, "history_versions": 41 },
  "links": { "live": 51, "ended_last_7d": 9 },
  "access": {
    "last_24h": { "success": 120, "expired": 3, "exhausted": 1, "revoked": 0, "upstream_error": 0, "taken_down": 0, "suspended": 0 },
    "last_7d": { "success": 902, "expired": 17, "exhausted": 4, "revoked": 2, "upstream_error": 0, "taken_down": 0, "suspended": 0 }
  },
  "anonymous": { "live_pastes": 6, "bytes": 81920, "limit_bytes": 21474836480 },
  "prune": { "last_run_at": "2026-10-03T04:00:00Z", "access_logs": 0, "sessions": 2, "pastes": 1, "versions": 0 }
}
```

- `health`：数据库与对象存储的状态，取值为 `ok` 或 `error`；对象存储不支持检测时为 `unknown`。对象存储通过对存储桶的一次元数据请求判断，不读取任何对象。
- `access`：最近 24 小时与 7 天内经分享链接的访问次数，按结果分类。合并记录的重复拒绝按实际次数计。
- `anonymous`：未登录创建的有效快速分享数量、其占用的容量与匿名额度上限。登录用户的快速分享计入各自账号。
- `prune`：本进程启动后最近一次定时清理的结果，尚未清理时为 `null`。

### 账号

`GET /_admin/v1/users?q=&status=&page=&size=`

`q` 部分匹配登录名（不区分大小写），或完全匹配 GitHub ID；`status` 为 `active` 或 `suspended`。返回账号对象的列表（不含 `plans`）。

`GET /_admin/v1/users/{id}`

返回账号对象，含 `plans`。

`POST /_admin/v1/users/{id}/suspend`

请求体：`{"reason": "…"}`。停用账号，返回更新后的账号对象。错误代码：`already_suspended`。

`POST /_admin/v1/users/{id}/unsuspend`

请求体：`{"reason": "…"}`。恢复账号，返回更新后的账号对象。错误代码：`not_suspended`。

### 套餐

`GET /_admin/v1/plans`

返回全部可授予的套餐（列表格式，`total` 为套餐总数）。快速分享使用的匿名额度不是套餐，不在其中。

`POST /_admin/v1/plans`

```json
{ "name": "friends", "max_resources": 100, "max_storage": 1073741824, "reason": "…" }
```

新建套餐，返回 `201` 与套餐对象。名称为 1–100 个字符，上限不能为负数。错误代码：`plan_exists`、`invalid_plan`。

`DELETE /_admin/v1/plans/{id}`

请求体：`{"reason": "…"}`。删除套餐，返回 `{"id": "…", "deleted": true}`。默认套餐不能删除（`plan_protected`）；仍有账号持有该套餐时不能删除（`plan_in_use`），包括已到期但尚未撤回的授予，须先逐个撤回。

`POST /_admin/v1/users/{id}/plans`

```json
{ "plan_id": "…", "expires_at": "2027-01-01T00:00:00Z", "reason": "…" }
```

为账号授予套餐，其上限与已有套餐相加；`expires_at` 为 `null` 时不到期。对已持有的套餐再次授予时只更新到期时间。默认套餐不能授予（`plan_protected`）。返回更新后的账号对象。

`DELETE /_admin/v1/users/{id}/plans/{plan_id}`

请求体：`{"reason": "…"}`。撤回授予的套餐，返回更新后的账号对象。默认套餐不能撤回（`plan_protected`）。

### 资源

`GET /_admin/v1/resources?owner=&q=&status=&page=&size=`

`owner` 为账号 ID；`q` 部分匹配名称或文件名（不区分大小写）；`status` 为 `active` 或 `taken_down`。按创建时间倒序返回资源对象的列表。

`GET /_admin/v1/resources/{id}`

返回资源对象。

`GET /_admin/v1/resources/{id}/links?page=&size=`

返回该资源的全部分享链接（含已失效的），按创建时间倒序。

`POST /_admin/v1/resources/{id}/takedown`

请求体：`{"reason": "…"}`。下架资源，返回更新后的资源对象；未登录创建的快速分享因下架而被删除时返回 `{"id": "…", "deleted": true}`。错误代码：`already_taken_down`。

`POST /_admin/v1/resources/{id}/restore`

请求体：`{"reason": "…"}`。撤销下架，返回更新后的资源对象。错误代码：`not_taken_down`。

`DELETE /_admin/v1/resources/{id}`

请求体：`{"reason": "…"}`。删除资源及其历史版本与分享链接，访问记录保留。返回 `{"id": "…", "deleted": true}`。

### 分享链接

`POST /_admin/v1/links/{id}/revoke`

请求体：`{"reason": "…"}`。撤销链接，返回 `{"id": "…", "revoked": true}`。错误代码：`already_revoked`。

`POST /_admin/v1/lookup`

```json
{ "link": "https://plainmote.link/d/…" }
```

`link` 可以是完整的分享地址、地址中的路径或 token 本身。返回该链接所属的资源与链接自身的状态：

```json
{ "resource": { "id": "…", "name": "checkout-api", "…": "…" }, "link": { "id": "…", "name": "web-01", "live": true, "…": "…" } }
```

`resource` 为资源对象，`link` 为分享链接对象。以 POST 提交，使分享地址不出现在任何 URL 与日志中；响应不回显 token。

### 审计记录

`GET /_admin/v1/audit?target=&page=&size=`

`target` 为对象 ID，指定时只返回针对该对象的记录。按时间倒序返回审计记录对象的列表。
