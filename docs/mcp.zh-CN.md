# MCP 服务器

[English](mcp.md) | 简体中文

`/mcp` 是一个远程 [Model Context Protocol](https://modelcontextprotocol.io) 服务器。Claude、ChatGPT、Cursor、Codex 等 AI 应用可以连接到该服务器，经账号所有者在浏览器中授权后，列出、读取、保存和新建资源；所有者允许的，还可以创建和撤销分享链接。

应用不能删除资源、查看访问记录、修改账号，也不能读取端到端加密的内容。应用读取的未加密内容会发送给该应用及其使用的模型服务商。

## 连接应用

服务器地址为部署地址加 `/mcp`，例如 `https://plainmote.link/mcp`。开发者页面 `/developers/mcp` 按应用列出以下步骤，并已填入本部署的地址。

| 应用 | 添加方法 |
| --- | --- |
| Claude Code | `claude mcp add --transport http --scope user plainmote <地址>`，然后在 Claude Code 中运行 `/mcp` 完成授权 |
| Codex | `codex mcp add plainmote --url <地址>`，该命令会打开浏览器进行授权；如未打开，运行 `codex mcp login plainmote` |
| Zed | 在 `settings.json` 中加入 `"context_servers": {"plainmote": {"url": "<地址>"}}` |
| VS Code | `code --add-mcp '{"name":"plainmote","type":"http","url":"<地址>"}'`，或在 `mcp.json`（MCP: Open User Configuration）中加入 `"servers": {"plainmote": {"type": "http", "url": "<地址>"}}` |
| opencode | 在 `opencode.json` 中加入 `"mcp": {"plainmote": {"type": "remote", "url": "<地址>"}}`，然后运行 `opencode mcp auth plainmote` |
| Cursor | 在 `~/.cursor/mcp.json` 或项目中的 `.cursor/mcp.json` 加入 `"mcpServers": {"plainmote": {"url": "<地址>"}}`；开发者页面另提供一键安装链接 |
| Antigravity | 在 `mcp_config.json`（… › Manage MCP Servers › View raw config）中加入 `"mcpServers": {"plainmote": {"serverUrl": "<地址>"}}`，键名须为 `serverUrl` |
| Claude（桌面应用及 claude.ai） | 设置 › 连接器 › 添加自定义连接器 |
| ChatGPT（桌面应用及网页） | 设置 › 应用 › 高级设置中开启开发者模式，然后在 设置 › 应用 中创建应用，认证方式选择 OAuth |

Claude 和 ChatGPT 从其自身的服务器发起连接，因此部署须可从公网访问。其他支持远程 MCP 服务器和 OAuth 的应用，填写同一地址即可。应用首次连接时会打开本部署的授权页面，所有者在该页面登录（如尚未登录），核对应用名称和授权后的跳转地址，然后允许或拒绝。

## 授权

应用通过 OAuth 2.1 授权，采用带 PKCE（`S256`）的授权码流程。仅支持公开客户端，不使用客户端密钥。

1. 未携带有效令牌访问 `/mcp` 时，服务器返回 `401` 和 `WWW-Authenticate: Bearer resource_metadata="…/.well-known/oauth-protected-resource/mcp"`。受保护资源元数据指明授权服务器即本部署，其元数据位于 `/.well-known/oauth-authorization-server`。
2. 应用以以下两种方式之一表明身份：
   - **客户端 ID 元数据文档**：`client_id` 为一个 `https` 地址，指向说明应用名称和跳转地址的 JSON 文档。服务器获取该文档，授权页面将其域名显示为已验证。
   - **动态客户端注册**（RFC 7591），即 `POST /oauth/register`。应用自行提供的信息显示为未经验证。同一地址每小时最多注册 20 个客户端。

   跳转地址必须为 `https`、指向本机回环地址的 `http`（端口不限）或应用自有的协议，且不得包含片段。
3. 应用将所有者引导至 `/oauth/authorize`，并将 `resource` 设为 `/mcp` 地址（RFC 8707）。令牌与该地址绑定，在其他地址均不被接受。
4. 应用在 `/oauth/token` 用授权码换取令牌，此后也在该处刷新令牌。

### 权限范围

| 范围 | 允许 |
| --- | --- |
| `read` | 列出和读取资源及其版本 |
| `write` | 新建资源、保存新版本 |
| `share` | 创建和撤销分享链接 |

每次授权始终包含全部三项权限，与应用请求的范围无关。授权页面列出这些权限，不提供选择：所有者只需允许或拒绝。所有工具均不能删除任何内容。创建分享链接是唯一使内容可在账号之外被访问的操作，因此 `share_resource` 带有 `destructiveHint` 和 `openWorldHint` 标注，供客户端在执行前向用户确认。

### 有效期与撤销

- 访问令牌有效期为 1 小时。
- 每次刷新都会签发新的刷新令牌，所用的刷新令牌随即作废。再次出示已作废的刷新令牌，整个授权即告失效。
- 授权连续 30 天未刷新即失效。
- 每次授权都会询问所有者，每次授权分别保存。
- 应用可通过 `POST /oauth/revoke`（RFC 7009）撤销其令牌。所有者可在设置的“已授权应用”中撤销任一授权；该页面列出每个应用的权限、授权时间和最后使用时间。

令牌仅以摘要形式保存。账号被停用时，所有授权立即失效。

## 传输

- 仅支持 `POST /mcp` 上的 Streamable HTTP。每个响应均为单个 JSON 对象。服务器不推送流、不保持会话、不发送通知。`GET` 和 `DELETE` 返回 `405`。
- 支持的协议版本：
  - `2026-07-28`：每个请求在 `_meta` 中携带 `io.modelcontextprotocol/protocolVersion` 和 `clientCapabilities`。`MCP-Protocol-Version`、`Mcp-Method` 和 `Mcp-Name` 请求头须与请求体一致。`server/discover` 返回服务器说明。
  - `2025-11-25`、`2025-06-18` 和 `2025-03-26`：以 `initialize` 开始。
- 不接受 JSON-RPC 批量请求。
- 来自其他网站页面的请求（依据 `Origin` 请求头判断）返回 `403`。

## 工具

| 工具 | 权限 | 参数 | 作用 |
| --- | --- | --- | --- |
| `list_resources` | `read` | `query`、`kind`（`resource` 或 `quick_share`）、`cursor` | 按更新时间从近到远列出资源，每页 50 个；`query` 匹配名称和文件名 |
| `read_resource` | `read` | `resource`、`version`、`offset` | 返回当前版本或历史版本的文本，每次最多 256 KiB |
| `list_versions` | `read` | `resource` | 列出资源的版本 |
| `create_resource` | `write` | `content`、`filename`、`name` | 以文本新建资源 |
| `update_resource` | `write` | `resource`、`base_version`、`content`、`name`、`filename` | 将文本保存为新版本，沿用资源原有的编码和换行符 |
| `share_resource` | `share` | `resource`、`name`、`expires_in`、`max_uses` | 创建分享链接并返回其地址 |
| `unshare_resource` | `share` | `resource`、`link` | 撤销一条链接；省略 `link` 时撤销该资源的全部链接 |

- `resource` 可以是 ID、至少 6 个字符的 ID 前缀、完整的名称或文件名，也可以是资源页面或其某条分享链接的地址。以分享链接地址指定资源时，不消耗该链接的使用次数，也不要求该链接仍然有效。
- `base_version` 已不是当前版本时，`update_resource` 拒绝保存，因此不会覆盖较新的内容；应用应重新读取后再修改。与当前文本完全相同的文本不视为冲突，也不会保存任何内容。
- `share_resource` 的 `expires_in` 为 `1h`、`24h`、`7d` 等时长（最长 `30d`，默认 `7d`）；`max_uses` 为 `0`（不限）至 1,000,000。应用不能创建永不过期的链接，所有者可在资源页面上创建。由应用创建的链接会在资源的分享列表中标注创建它的应用名称。
- 二进制内容只返回说明，不返回内容。超过 256 KiB 的文本分段返回。快速分享可以列出和读取，但不能修改或再次分享。
- 冲突、容量已满等拒绝情形以设置了 `isError` 的工具结果返回，并附带可供模型据以处理的说明。其他账号的资源与不存在的资源返回完全相同的结果。

## 加密资源

服务器无法解密端到端加密的资源，也不会索取主密码。加密资源在列表中仅显示为占位项，附带 ID、大小、更新时间和地址，不提供名称和文件名。读取加密资源只返回提示，不返回内容；加密资源也不能通过 MCP 修改或分享。

## 限制

| 项目 | 限制 |
| --- | --- |
| 工具调用 | 每个授权每分钟 300 次，每个 IP 地址每分钟 600 次 |
| 创建分享链接 | 每个授权每小时 30 条 |
| 认证失败 | 每个 IP 地址每分钟 30 次 |
| 内容 | 与账号其他资源相同的大小和存储容量限制 |

超出限制的请求返回 `429`。
