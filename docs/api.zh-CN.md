# 命令行接口

[English](api.md) | 简体中文

`/api/v1/` 下的接口供 `plainmote` 命令行使用，也可以由脚本直接调用。调用者使用个人访问令牌认证，令牌通过设备授权获得：命令行或脚本申请验证码，账号所有者在已登录的浏览器中输入并允许。

接口可以列出、读取、保存和新建资源，以及创建快速分享，不能删除资源、管理分享链接或修改账号。

## 获取令牌

设备授权遵循 RFC 8628，分三步：

1. 客户端调用 `POST /api/v1/device/code`，取得设备码（`device_code`，只由客户端持有）与验证码（`user_code`，展示给用户）。
2. 用户在已登录的浏览器中打开 `verification_uri`（即 `/cli/device`），输入验证码，核对设备信息后点击“允许”。
3. 客户端按 `interval` 秒的间隔调用 `POST /api/v1/device/token`，直到取得令牌或被拒绝。

验证码为 8 个字符，取自不含 `0`、`O`、`1`、`I` 的 32 个字符，显示为 `XXXX-XXXX` 形式，输入时不区分大小写。验证码与设备码在 10 分钟后失效，设备码只能换取一次令牌。客户端不应将验证码放入打开的地址：手动输入验证码，是确认允许者正是看到验证码的人的那一步。

### 申请验证码

`POST /api/v1/device/code`

请求头 `Content-Type: application/json`，请求体：

| 字段 | 说明 |
| --- | --- |
| `scope` | `read`（只读）或 `write`（读写） |
| `device` | 设备名称，显示在确认页与账号页，最长 64 个字符 |
| `os` | 系统与架构，例如 `darwin/arm64`，最长 64 个字符 |
| `version` | 客户端版本，最长 64 个字符 |

不接受其他字段。响应：

```json
{
  "device_code": "…",
  "user_code": "KQWD-7XHM",
  "verification_uri": "https://plainmote.link/cli/device",
  "expires_in": 600,
  "interval": 5
}
```

### 换取令牌

`POST /api/v1/device/token`

请求头 `Content-Type: application/json`，请求体 `{"device_code": "…"}`。用户允许后返回：

```json
{
  "access_token": "pmt_…",
  "token_type": "bearer",
  "scope": "write",
  "expires_at": "2026-12-31T08:20:00Z",
  "login": "mira"
}
```

在此之前返回 `400` 与 `{"error": "<代码>", "interval": <秒>}`：

| 代码 | 含义 |
| --- | --- |
| `authorization_pending` | 用户尚未作出决定，按 `interval` 继续轮询 |
| `slow_down` | 轮询过快，间隔增加 5 秒（最多 60 秒），按新的 `interval` 继续 |
| `access_denied` | 用户拒绝了这次登录，或账号已被停用 |
| `expired_token` | 设备码已失效、已换取过令牌或不存在 |

## 使用令牌

- 令牌以 `pmt_` 开头，只能放在 `Authorization: Bearer <令牌>` 请求头中。地址中的令牌与 Cookie 一律不被接受。
- 令牌自登录起 90 天内有效，使用不会延长有效期；到期后需要重新获取。
- 只读令牌可以调用读取类接口，调用写入类接口时返回 `403`。
- 服务端只保存令牌的摘要。账号所有者可以在账号页查看每个令牌的设备、权限、登录时间和最后使用情况，并随时撤销；令牌也可以通过 `DELETE /api/v1/token` 撤销自身。账号被停用时，其全部令牌被删除。
- `plainmote` 命令行把令牌保存在配置目录下的 `credentials` 文件中（权限 600），也读取环境变量 `PLAINMOTE_TOKEN`。

## 通用约定

- 请求与响应为 JSON，资源内容的读取与保存除外。时间为 RFC 3339 格式的 UTC 时间，大小以字节为单位。
- 每个响应都带有 `X-PlainMote-Version` 响应头，值为服务端版本。
- 接口不响应跨域请求，不返回任何 CORS 响应头。
- 出错时返回 `{"error": "<代码>", "message": "<说明>"}`。

### 限流

| 对象 | 上限 | 超出时 |
| --- | --- | --- |
| 申请验证码 | 每个 IP 每 10 分钟 10 次 | `429`，`rate_limited` |
| 换取令牌的轮询 | 每个 IP 每分钟 120 次 | `429`，`slow_down` |
| 认证失败 | 每个 IP 每分钟 30 次 | 该分钟内的请求返回 `429`，`rate_limited` |
| 已认证的请求 | 每个令牌每分钟 300 次 | `429`，`rate_limited` |

### 错误代码

| 状态码 | 代码 | 含义 |
| --- | --- | --- |
| `400` | `bad_request` | 请求格式不正确 |
| `400` | `invalid_scope` | `scope` 不是 `read` 或 `write` |
| `400` | `invalid_ttl` | `ttl` 不是快速分享可选的有效期 |
| `400` | `refused` | 请求被拒绝，原因见 `message`，例如文件名不合法或保存到快速分享 |
| `401` | `unauthorized` | 缺少令牌，或令牌无效、已过期、已撤销；响应带有 `WWW-Authenticate: Bearer` |
| `403` | `read_only` | 只读令牌不能进行写入 |
| `404` | `not_found` | 资源或接口不存在，包括属于其他账号的资源 |
| `409` | `reference` | 远程资源没有保存在本服务的内容 |
| `412` | `conflict` | 保存所基于的版本已不是当前版本，`current_version` 为当前版本号 |
| `413` | `too_large` | 内容超过大小上限 |
| `413` | `quota` | 超出账号的资源数量或容量上限 |
| `415` | `bad_request` | 请求体类型不正确 |
| `428` | `precondition_required` | 保存时缺少 `If-Match` |
| `429` | `rate_limited`、`slow_down` | 超出限流 |
| `500` | `internal` | 服务端故障，详情只写入服务端日志 |

## 接口一览

| 方法 | 路径 | 权限 | 说明 |
| --- | --- | --- | --- |
| `POST` | `/api/v1/device/code` | 无需令牌 | 申请验证码 |
| `POST` | `/api/v1/device/token` | 无需令牌 | 换取令牌 |
| `GET` | `/api/v1/me` | 只读 | 当前账号与令牌 |
| `DELETE` | `/api/v1/token` | 只读 | 撤销当前令牌 |
| `GET` | `/api/v1/resources` | 只读 | 资源列表，或解析资源引用 |
| `GET` | `/api/v1/resources/{id}` | 只读 | 资源详情 |
| `GET` | `/api/v1/resources/{id}/content` | 只读 | 读取内容 |
| `PUT` | `/api/v1/resources/{id}/content` | 读写 | 保存为新版本 |
| `POST` | `/api/v1/resources` | 读写 | 新建资源 |
| `POST` | `/api/v1/quick-shares` | 读写 | 创建快速分享 |

## 资源对象

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `id` | string | 资源 ID |
| `name` | string | 名称 |
| `filename` | string | 文件名 |
| `size` | integer | 当前内容的大小 |
| `type` | string | 内容类型 |
| `encoding` | string | 文本编码，例如 `utf-8`、`gb18030`；非文本内容省略 |
| `version` | integer | 当前版本号 |
| `updated_at` | time | 更新时间 |
| `remote` | boolean | 是否为远程资源 |
| `editable` | boolean | 是否为可以编辑的文本内容 |
| `encrypted` | boolean | 是否为端到端加密的内容；否时省略 |
| `taken_down` | boolean | 是否已被运营者下架；否时省略 |
| `expires_at` | time | 仅快速分享有此字段，为其删除时间。快速分享在资源页转为资源之前为只读（`editable` 为 false） |
| `url` | string | 资源页地址 |

## 接口

### 当前账号

`GET /api/v1/me`

```json
{ "login": "mira", "scope": "write", "expires_at": "2026-12-31T08:20:00Z", "device": "mira-mbp", "server_version": "adbcdce8a464" }
```

### 撤销当前令牌

`DELETE /api/v1/token`

成功时返回 `204`。

### 资源列表

`GET /api/v1/resources?q=`

返回 `{"resources": [<资源对象>...]}`，按更新时间倒序，最多 500 项。`q` 按名称或文件名进行部分匹配，不区分大小写。

`GET /api/v1/resources?ref=`

把命令行中输入的资源引用解析为资源：`ref` 可以是完整的 ID、至少 6 个字符的 ID 前缀，或完整的名称、文件名。返回所有匹配项，最多 20 项；匹配多项时由调用者决定，服务端不作选择。

### 资源详情

`GET /api/v1/resources/{id}`

返回资源对象。

### 读取内容

`GET /api/v1/resources/{id}/content`

以原始字节返回当前内容，保持保存时的编码与换行符。响应头：

| 响应头 | 内容 |
| --- | --- |
| `Content-Type` | 内容类型，文本附带 `charset` |
| `ETag` | 版本标签，例如 `"v5"` |
| `X-PlainMote-Resource-Version` | 版本号，例如 `5` |
| `X-PlainMote-Encoding` | 文本编码；非文本内容不返回 |

远程资源返回 `409`（`reference`）。

### 保存为新版本

`PUT /api/v1/resources/{id}/content`

请求体为新内容的原始字节。请求头：

| 请求头 | 内容 |
| --- | --- |
| `If-Match` | 必填。编辑所基于的版本标签，例如 `"v5"`；`*` 表示不论当前版本、直接保存 |
| `X-PlainMote-Encoding` | 可选。内容的文本编码，通常沿用读取时的 `X-PlainMote-Encoding`；省略时由服务端识别 |

成功时返回：

```json
{ "version": 6, "new_version": true, "trimmed": 0 }
```

- `new_version`：内容与当前版本相同时为 `false`，此时不产生新版本。
- `trimmed`：为腾出容量而清除的历史版本数量。

资源在此期间已被保存为更新的版本时，返回 `412`：

```json
{ "error": "conflict", "message": "…", "current_version": 7 }
```

内容大小上限由部署的 `PLAINMOTE_MAX_CONTENT_MIB` 决定。名称与文件名不变；历史版本、容量与冲突检查规则与网页保存相同。

### 新建资源

`POST /api/v1/resources`

请求体为 `multipart/form-data`：

| 字段 | 说明 |
| --- | --- |
| `content` | 必填，文件形式的内容 |
| `name` | 名称，可选；省略时使用文件名 |
| `filename` | 文件名，可选，决定内容类型的识别 |
| `encoding` | 文本编码，可选；省略时由服务端识别 |

成功时返回 `201` 与资源对象。

### 创建快速分享

`POST /api/v1/quick-shares`

与登录用户在首页输入框中的操作相同：保存内容并生成一条随其失效的分享链接，失效前保存在账号的资源中。仅在部署开放快速分享时可用，否则返回 `404`。

请求体为 `multipart/form-data`：

| 字段 | 说明 |
| --- | --- |
| `content` | 必填，文件形式的内容，最大 10 MiB |
| `filename` | 可选，链接末尾的文件名 |
| `ttl` | 可选，`10m`、`1h`、`1d`、`7d` 或 `30d`，默认 `1h` |

成功时返回 `201`、资源对象与分享链接 `share_url`。文本以纯文本交付，图片和音视频按其类型交付，其他内容作为下载，与首页创建的快速分享相同。快速分享计入账号的存储容量，到期后内容被删除，访问记录保留。

## 示例

```bash
TOKEN=pmt_…
API=https://plainmote.link/api/v1

curl -fsS -H "Authorization: Bearer $TOKEN" "$API/resources?q=nginx"

curl -fsS -D headers.txt -o nginx.conf -H "Authorization: Bearer $TOKEN" "$API/resources/$ID/content"

curl -fsS -X PUT -H "Authorization: Bearer $TOKEN" -H 'If-Match: "v5"' \
  --data-binary @nginx.conf "$API/resources/$ID/content"

curl -fsS -H "Authorization: Bearer $TOKEN" \
  -F name=app.log -F filename=app.log -F content=@app.log "$API/resources"

curl -fsS -H "Authorization: Bearer $TOKEN" \
  -F filename=app.log -F ttl=1d -F content=@app.log "$API/quick-shares"
```

在命令行参数中写入令牌会让同一台机器上的其他用户通过进程列表看到它。脚本中应从文件读取令牌，例如使用 curl 的 `-H @文件`，或直接使用 `plainmote` 命令行。
