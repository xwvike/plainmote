# 管理接口

PlainMote 不提供管理界面，只提供一组签名保护的 HTTP 接口。管理界面是独立的网页，部署在任何地方（包括本机）均可，持有私钥即可调用。

本文档是接口约定，管理端据此实现。

## 设计原则

- **默认关闭**：未设置 `PLAINMOTE_ADMIN_KEYS` 时，下列路由不注册，访问返回 `404`。
- **服务端只持有公钥**：请求以 Ed25519 私钥签名，服务端只做验签。服务器的环境变量泄露不足以伪造管理请求。
- **不接触内容**：任何接口都不返回资源正文、历史版本正文、分享链接地址或访问记录中的 IP 与客户端信息。管理端只能看到元数据与汇总数字。
- **有副作用的操作全部留痕**：写入只追加的审计记录，记录时间、密钥、操作、对象与原因。
- **不暴露自身**：签名缺失或无效时返回与不存在的路径相同的 `404`，并按来源 IP 限流。

## 配置

| 变量 | 默认值 | 作用 |
| --- | --- | --- |
| `PLAINMOTE_ADMIN_KEYS` | 空 | 允许调用管理接口的 Ed25519 公钥，Base64（32 字节原始公钥），多个以逗号分隔。更换密钥时可新旧并存。为空则管理接口不存在 |
| `PLAINMOTE_ADMIN_ORIGINS` | 空 | 允许以浏览器跨域调用管理接口的来源，逗号分隔，例如 `http://localhost:5173,https://admin.example.com`。为空则不响应跨域请求 |

密钥对由仓库内的工具生成：

```bash
go run ./cmd/plainmote-admin keygen
# key id:  <公钥标识>
# public:  <Base64 公钥>   → 填入 PLAINMOTE_ADMIN_KEYS
# private: <JWK 私钥>      → 由管理端导入，或存为文件供 call 使用；不要放到服务器上
```

同一工具可为命令行请求签名，便于不使用管理端时直接调用：

```bash
go run ./cmd/plainmote-admin call -key admin.jwk GET https://plainmote.link/_admin/v1/overview
go run ./cmd/plainmote-admin call -key admin.jwk POST https://plainmote.link/_admin/v1/lookup '{"link":"https://plainmote.link/d/…"}'
```

使用 Docker Compose 部署时，`compose.yaml` 已将这两个变量传入容器，在 `.env` 中设置即可。

## 签名

每个请求携带以下请求头：

| 请求头 | 内容 |
| --- | --- |
| `X-PlainMote-Key` | 公钥标识：公钥 SHA-256 的前 16 个十六进制字符 |
| `X-PlainMote-Timestamp` | Unix 时间，秒 |
| `X-PlainMote-Nonce` | 至少 16 字节随机数，Base64url |
| `X-PlainMote-Signature` | 对下述签名串的 Ed25519 签名，Base64url |

签名串为以下各行以 `\n` 连接（末尾无换行）：

```
PLAINMOTE-ADMIN-V1
<请求方法，大写>
<请求路径与查询串，与实际发送的完全一致>
<X-PlainMote-Timestamp>
<X-PlainMote-Nonce>
<请求体 SHA-256，小写十六进制；无请求体时为空字符串的 SHA-256>
```

服务端拒绝以下请求：时间戳与服务器时间相差超过 300 秒；同一 nonce 在 600 秒内重复出现；公钥标识不在 `PLAINMOTE_ADMIN_KEYS` 中；签名无效。

浏览器端使用 WebCrypto 的 Ed25519（Chrome 137+、Firefox 129+、Safari 17+）。建议管理端将私钥以 `extractable: false` 导入，仅保存在当前会话，或以 `CryptoKey` 对象存入 IndexedDB，不以明文形式保存。

## 跨域

仅当请求的 `Origin` 在 `PLAINMOTE_ADMIN_ORIGINS` 中时，响应 `OPTIONS` 预检并返回 `Access-Control-Allow-Origin`、`Access-Control-Allow-Headers`（上述四个请求头与 `Content-Type`）和 `Access-Control-Allow-Methods`。不使用 Cookie，不返回 `Access-Control-Allow-Credentials`。

## 通用约定

- 路径前缀 `/_admin/v1/`；请求与响应均为 JSON，时间为 RFC 3339（UTC），大小以字节为单位。
- 列表接口接受 `page`（从 1 开始）与 `size`（最大 100），返回 `{"items": [...], "total": n}`。
- 错误返回 `{"error": "<代码>", "message": "<说明>"}`，代码见各接口。
- 有副作用的接口要求请求体中的 `reason` 字段（1–500 字），写入审计记录；面向用户的操作会向用户显示该原因。
- 签名有效但对象不存在时返回 `404` 与 `{"error": "not_found"}`；状态冲突返回 `409`，错误代码见各接口；请求体不合法返回 `400`（`bad_request`、`bad_reason`）；服务端故障返回 `500`（`internal`），详情只写入服务端日志。

## 接口

### 部署概况

`GET /_admin/v1/overview`

```json
{
  "version": "06f9cf403721",
  "revision": "06f9cf4037…",
  "started_at": "…",
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
  "resources": { "total": 84, "remote": 3, "taken_down": 0, "current_bytes": 0, "history_bytes": 0, "history_versions": 0 },
  "links": { "live": 51, "ended_last_7d": 9 },
  "access": { "last_24h": { "success": 0, "expired": 0, "exhausted": 0, "revoked": 0, "upstream_error": 0, "taken_down": 0, "suspended": 0 }, "last_7d": { } },
  "anonymous": { "live_pastes": 0, "bytes": 0, "limit_bytes": 21474836480 },
  "prune": { "last_run_at": "…", "access_logs": 0, "sessions": 0, "pastes": 0, "versions": 0 }
}
```

`object_storage` 通过对存储桶的一次元数据请求判断，不读取任何对象。`prune` 为本进程启动后最近一次定时清理的结果，启动后尚未清理时为 `null`。访问次数按结果分类，合并记录的重复拒绝按实际次数计。

### 用户

`GET /_admin/v1/users?q=&status=&page=&size=`

`q` 匹配登录名或 GitHub ID；`status` 为 `active` 或 `suspended`。每项：

```json
{
  "id": "…", "github_id": "123", "login": "mira", "name": "Mira",
  "created_at": "…", "last_signed_in_at": "…",
  "status": "active", "suspended_reason": "",
  "resources": 4, "resources_limit": 1000, "live_links": 7,
  "storage": { "current_bytes": 0, "history_bytes": 0, "limit_bytes": 104857600 }
}
```

`resources_limit` 与 `storage.limit_bytes` 为当前生效的全部套餐之和，已计入授予时间、到期时间及套餐的有效期。

`GET /_admin/v1/users/{id}`：同上，另含 `plans`（生效中的套餐及到期时间）。

`POST /_admin/v1/users/{id}/suspend`，请求体 `{"reason": "…"}`：停用账号。立即注销其全部登录会话，拒绝再次登录（登录页显示原因）；其全部分享链接停止交付，访问记录以 `suspended` 结果记录。数据不删除。

`POST /_admin/v1/users/{id}/unsuspend`，请求体 `{"reason": "…"}`：恢复账号。链接按各自原有的有效期与次数继续生效。

错误代码：`not_found`、`already_suspended`、`not_suspended`。

### 套餐

`GET /_admin/v1/plans`：全部套餐及其上限、是否默认、使用人数。快速分享使用的匿名额度不是可授予的套餐，不在其中。

`POST /_admin/v1/plans`，请求体 `{"name": "…", "max_resources": 100, "max_storage": 1073741824, "reason": "…"}`：新建一个可授予的套餐。错误代码：`plan_exists`、`invalid_plan`。

`DELETE /_admin/v1/plans/{id}`，请求体 `{"reason": "…"}`：删除套餐，成功返回 `{"id": "…", "deleted": true}`。默认套餐不能删除（`plan_protected`）；仍有用户持有（含已到期但尚未撤回的授予）时不能删除（`plan_in_use`），须先逐个撤回，删除不会降低任何人的额度。

`POST /_admin/v1/users/{id}/plans`，请求体 `{"plan_id": "…", "expires_at": null, "reason": "…"}`：为用户追加套餐，上限与已有套餐相加。

`DELETE /_admin/v1/users/{id}/plans/{plan_id}`，请求体 `{"reason": "…"}`：撤回追加的套餐。默认套餐不能撤回（`plan_protected`）。

两者成功时均返回更新后的用户详情。

### 资源

`GET /_admin/v1/resources?owner=&q=&status=&page=&size=`

`owner` 为用户 ID；`q` 匹配名称或文件名；`status` 为 `active` 或 `taken_down`。每项只含元数据：

```json
{
  "id": "…", "owner": { "id": "…", "login": "mira" },
  "name": "checkout-api", "filename": "checkout.yaml",
  "kind": "stored", "content_type": "application/yaml", "size": 292,
  "origin_host": "",
  "version": 7, "history_versions": 6, "history_bytes": 1710,
  "live_links": 3,
  "created_at": "…", "updated_at": "…",
  "status": "active", "takedown_reason": ""
}
```

远程资源的 `kind` 为 `remote`，只给出 `origin_host`（主机名），不给出完整地址。快速分享的 `owner.login` 为 `anonymous`。

`GET /_admin/v1/resources/{id}/links?page=&size=`：该资源的全部链接（含已失效的），按创建时间倒序，每项：

```json
{
  "id": "…", "name": "web-01", "created_at": "…",
  "expires_at": "…", "max_uses": 0, "used_count": 12,
  "revoked_at": null, "live": true
}
```

不返回 token。资源不存在时返回 `404`。

`POST /_admin/v1/lookup`，请求体 `{"link": "<举报中的分享地址或 token>"}`：由分享地址找到资源，返回上述资源元数据及该链接自身的状态（名称、有效期、次数、是否撤销）。以 POST 提交，使分享地址不出现在任何 URL 与日志中；响应不回显 token。

`POST /_admin/v1/resources/{id}/takedown`，请求体 `{"reason": "…"}`：下架。该资源的全部链接停止交付，访问记录以 `taken_down` 结果记录；所有者在资源页看到下架原因，不能为其创建新链接，可以删除该资源。返回更新后的资源元数据。快速分享没有所有者可以通知，下架即删除，此时返回 `{"id": "…", "deleted": true}`。

`POST /_admin/v1/resources/{id}/restore`，请求体 `{"reason": "…"}`：撤销下架。

`DELETE /_admin/v1/resources/{id}`，请求体 `{"reason": "…"}`：删除资源及其历史版本与链接，与所有者自行删除效果相同，访问记录保留。

`POST /_admin/v1/links/{id}/revoke`，请求体 `{"reason": "…"}`：撤销单条链接。

错误代码：`not_found`、`already_taken_down`、`not_taken_down`、`already_revoked`。

### 审计记录

`GET /_admin/v1/audit?target=&page=&size=`

```json
{
  "id": "…", "at": "…", "key": "3f9a1c0e7b2d4a61",
  "action": "user.plan.grant", "target_type": "user", "target_id": "…",
  "target_label": "mira",
  "reason": "…",
  "detail": { "plan_id": "…", "plan_name": "friends", "expires_at": null },
  "remote_ip": "…"
}
```

- `target_label` 为操作时对象的名称快照，对象删除后仍保留：用户为登录名，资源为名称（无名称时为文件名），套餐为名称，链接为链接名称（无名称时为所属资源的名称）。早于该字段的记录为空字符串。
- `reason` 为管理员填写的原文。
- `detail` 为操作的结构化补充信息，无则为 `null`：

| `action` | `detail` |
| --- | --- |
| `user.suspend`、`user.unsuspend`、`resource.restore`、`resource.delete` | `null` |
| `resource.takedown` | `null`；快速分享因下架而被删除时为 `{"deleted": true}` |
| `user.plan.grant` | `{"plan_id", "plan_name", "expires_at"}` |
| `user.plan.revoke` | `{"plan_id", "plan_name"}` |
| `plan.create`、`plan.delete` | `{"max_resources", "max_storage"}` |
| `link.revoke` | `{"resource_id"}` |

审计记录只追加，不提供修改或删除接口，也不随访问记录的保留期清理。

## 不在本期范围

- **注册白名单**：目前由 `GITHUB_ALLOWED_IDS` 在启动时决定，改为可在线管理需要把白名单移入数据库，另行设计。
- **查看内容**：包括资源正文、历史版本、分享地址、访问记录明细。按原则不提供。

## 对用户的影响

- 停用账号的用户登录时，登录页显示"此账号已被停用"及停用原因。
- 下架资源的所有者在资源页顶部看到下架原因，"新建分享"按钮不再显示；资源列表中该资源的状态显示为"已下架"。
- 访问记录新增"已下架""账号已停用"两种结果。
- 公开链接对下架或停用的内容一律返回 `401`，与链接失效时相同，不说明原因。
