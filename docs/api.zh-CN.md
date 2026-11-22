# HTTP API

[English](api.md) | 简体中文

`/api/v1/` 下的接口供脚本、CI 流水线与自定义 HTTP 客户端调用。调用者使用个人访问令牌（`pmt_…`）认证，令牌可在网页端的 **设置 › 已授权应用**（`/account/devices`）中一键创建。

接口支持列出、读取、新建、更新、打标签、查看历史版本、管理分享链接、移入回收站以及创建快速分享。访问记录、回收站恢复/永久删除以及账号设置仅限网页端操作。

## 认证

1. 打开 **设置 › 已授权应用**（`/account/devices`），点击 **新建令牌**。
2. 填写令牌名称，选择 **只读**（`read`）或 **读写**（`write`）权限，以及有效期（`30 天`、`90 天`、`1 年` 或 `永不过期`）。
3. 复制创建时一次性展示的 `pmt_…` 令牌，并通过 `Authorization` 请求头携带：

```bash
Authorization: Bearer pmt_…
```

- 不接受放在 URL 查询参数或 Cookie 中的令牌。
- 只读令牌调用写入接口时返回 `403 read_only`。
- 服务端只保存令牌摘要。可随时在 **设置 › 已授权应用** 中撤销，或通过 `DELETE /api/v1/token` 撤销当前令牌。

## 通用约定

- 请求与响应均为 JSON，读取和保存资源内容（`GET`/`PUT /api/v1/resources/{id}/content`）除外，后者使用原始字节。时间为 RFC 3339 格式的 UTC 时间，大小以字节为单位。
- 每个响应带有 `X-PlainMote-Version` 响应头，不返回 CORS 响应头。
- 出错时返回 `{"error": "<代码>", "message": "<说明>"}`。

### 限流

| 对象 | 上限 | 超出时 |
| --- | --- | --- |
| 认证失败 | 每个 IP 每分钟 30 次 | `429 rate_limited` |
| 已认证请求 | 每个令牌每分钟 300 次 | `429 rate_limited` |
| 创建分享链接 | 每个令牌每小时 60 次 | `429 rate_limited` |
| 移入回收站 | 每个令牌每小时 60 次 | `429 rate_limited` |

## 接口一览

| 方法 | 路径 | 权限 | 说明 |
| --- | --- | --- | --- |
| `GET` | `/api/v1/resources` | `read` | 列出资源（支持 `?q=`、`?tag=`、`?kind=resource\|quick_share`、`?ref=`） |
| `POST` | `/api/v1/resources` | `write` | 新建资源（支持 JSON 或 `multipart/form-data`） |
| `GET` | `/api/v1/resources/{id}` | `read` | 资源详情 |
| `PATCH` | `/api/v1/resources/{id}` | `write` | 修改 `name`、`filename` 或 `tags` |
| `DELETE` | `/api/v1/resources/{id}` | `write` | 移入回收站并撤销全部分享链接 |
| `GET` | `/api/v1/resources/{id}/content` | `read` | 读取原始字节内容（支持 `?version=N` 读取历史版本） |
| `PUT` | `/api/v1/resources/{id}/content` | `write` | 提交原始字节保存为新版本（可选 `If-Match` 防冲突） |
| `GET` | `/api/v1/resources/{id}/versions` | `read` | 查看当前版本与历史版本列表 |
| `GET` | `/api/v1/resources/{id}/links` | `read` | 列出资源的有效分享链接 |
| `POST` | `/api/v1/resources/{id}/links` | `write` | 创建分享链接 |
| `DELETE` | `/api/v1/resources/{id}/links/{link_id}` | `write` | 撤销指定分享链接 |
| `PATCH` | `/api/v1/resources/{id}/tags` | `write` | 增量添加或移除标签 |
| `GET` | `/api/v1/tags` | `read` | 列出账号的全部标签及数量 |
| `POST` | `/api/v1/quick-shares` | `write` | 创建快速分享（支持 JSON 或 `multipart/form-data`） |
| `GET` | `/api/v1/me` | `read` | 当前账号与令牌信息 |
| `DELETE` | `/api/v1/token` | `read` | 撤销当前令牌 |

## 示例

```bash
TOKEN=pmt_…
API=https://plainmote.link/api/v1

# 列出资源
curl -fsS -H "Authorization: Bearer $TOKEN" "$API/resources?q=nginx"

# 用 JSON 新建资源
curl -fsS -X POST "$API/resources" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"filename":"nginx.conf","content":"listen 80;\n","tags":["prod"]}'

# 或使用 multipart/form-data 上传二进制文件
curl -fsS -H "Authorization: Bearer $TOKEN" \
  -F name=app.log -F filename=app.log -F content=@app.log "$API/resources"

# 读取原始内容（加 ?version=1 可读取历史版本）
curl -fsS -o nginx.conf -H "Authorization: Bearer $TOKEN" "$API/resources/$ID/content"

# 保存为新版本（If-Match 可选；传 "v1" 可启用乐观锁冲突校验）
curl -fsS -X PUT -H "Authorization: Bearer $TOKEN" \
  --data-binary @nginx.conf "$API/resources/$ID/content"

# 修改名称或替换标签
curl -fsS -X PATCH "$API/resources/$ID" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"生产 Nginx","tags":["prod","edge"]}'

# 创建分享链接（省略 expires_in 即为永不过期）
curl -fsS -X POST "$API/resources/$ID/links" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"edge-node-1","expires_in":"30d","max_uses":0}'

# 撤销分享链接
curl -fsS -X DELETE -H "Authorization: Bearer $TOKEN" "$API/resources/$ID/links/$LINK_ID"

# 移入回收站（保留 30 天）
curl -fsS -X DELETE -H "Authorization: Bearer $TOKEN" "$API/resources/$ID"
```

## 读取分享链接

读取分享链接不需要令牌，地址本身就是凭证。`GET` 返回原始字节，`HEAD` 只返回响应头，支持 `ETag` 与 `Last-Modified`（`304 Not Modified`）。资源更新后，所有分享链接立即生效。
