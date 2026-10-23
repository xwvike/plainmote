# MCP server

English | [简体中文](mcp.zh-CN.md)

`/mcp` is a remote [Model Context Protocol](https://modelcontextprotocol.io) server. AI apps such as Claude, ChatGPT, Cursor and Codex connect to it and, once the account owner has authorized them in a browser, can list, read, save and create resources, and create and revoke share links if the owner allows it.

An app cannot delete resources, read access history, change the account or read end-to-end encrypted content. Unencrypted content an app reads is sent to that app and to the model provider it uses.

## Connecting an app

The server address is the deployment's address followed by `/mcp`, for example `https://plainmote.link/mcp`. The developer page `/developers/mcp` gives the steps below for each app with the deployment's own address filled in.

| App | How to add the server |
| --- | --- |
| Claude Code | `claude mcp add --transport http --scope user plainmote <address>`, then `/mcp` in Claude Code to authorize |
| Codex | `codex mcp add plainmote --url <address>`, which opens a browser to authorize; if it does not, `codex mcp login plainmote` |
| Zed | `"context_servers": {"plainmote": {"url": "<address>"}}` in `settings.json` |
| VS Code | `code --add-mcp '{"name":"plainmote","type":"http","url":"<address>"}'`, or `"servers": {"plainmote": {"type": "http", "url": "<address>"}}` in `mcp.json` (MCP: Open User Configuration) |
| opencode | `"mcp": {"plainmote": {"type": "remote", "url": "<address>"}}` in `opencode.json`, then `opencode mcp auth plainmote` |
| Cursor | `"mcpServers": {"plainmote": {"url": "<address>"}}` in `~/.cursor/mcp.json` or a project's `.cursor/mcp.json`; the page also offers a one-click install link |
| Antigravity | `"mcpServers": {"plainmote": {"serverUrl": "<address>"}}` in `mcp_config.json` (… › Manage MCP Servers › View raw config); the key must be `serverUrl` |
| Claude (desktop app and claude.ai) | Settings › Connectors › Add custom connector |
| ChatGPT (desktop app and web) | Settings › Apps › Advanced settings: turn on developer mode; then Settings › Apps › Create app, with OAuth authentication |

Claude and ChatGPT connect from their own servers, so for them the deployment must be reachable from the internet. Other apps that support remote MCP servers with OAuth take the same address. The first time an app connects, it opens the deployment's consent page: the owner signs in if needed, checks the app's name and where it returns to, and allows or denies it.

## Authorization

Apps are authorized with OAuth 2.1: the authorization code flow with PKCE (`S256`), for public clients only, with no client secret.

1. A request to `/mcp` without a valid token is answered `401` with `WWW-Authenticate: Bearer resource_metadata="…/.well-known/oauth-protected-resource/mcp"`. The protected resource metadata names the deployment itself as the authorization server, whose metadata is at `/.well-known/oauth-authorization-server`.
2. The app identifies itself in one of two ways:
   - **Client ID metadata document**: the `client_id` is an `https` address of a JSON document that gives the app's name and redirect URIs. The server fetches the document, and the consent page shows its domain as verified.
   - **Dynamic client registration** (RFC 7591) at `POST /oauth/register`. What the app says about itself is shown as unverified. An address may register at most 20 clients an hour.

   Redirect URIs must be `https`, `http` to a loopback address (on any port), or a scheme of the app's own, and carry no fragment.
3. The app sends the owner to `/oauth/authorize` with `resource` set to the `/mcp` address (RFC 8707). Tokens are bound to that address and accepted nowhere else.
4. The app exchanges the code at `/oauth/token` and, later, refreshes there.

### Scopes

| Scope | Allows |
| --- | --- |
| `read` | Listing and reading resources and their versions |
| `write` | Creating resources and saving new versions |
| `share` | Creating and revoking share links |

An authorization always carries all three, whatever the app asked for. The consent page lists them and offers no choice: the owner allows the app or denies it. No tool deletes anything. Creating a share link is the one step that reaches outside the account, so `share_resource` carries the `destructiveHint` and `openWorldHint` annotations, for the client to confirm with its user before running it.

### Lifetimes and revocation

- An access token lasts one hour.
- Each refresh issues a new refresh token, and the one used is spent. Presenting a spent refresh token again ends the whole authorization.
- An authorization lapses after 30 days without a refresh.
- Consent is asked every time an app is authorized, and each authorization is kept separately.
- The app can revoke its tokens at `POST /oauth/revoke` (RFC 7009). The owner can revoke any authorization in the settings under Authorized apps, which lists each app with its permissions, when it was authorized and when it was last used.

Tokens are stored only as digests. Suspending the account ends every authorization at once.

## Transport

- Streamable HTTP over `POST /mcp` only. Every response is a single JSON object. The server does not stream, keeps no session and sends no notifications. `GET` and `DELETE` are answered `405`.
- Supported protocol versions:
  - `2026-07-28`: each request carries `_meta` with `io.modelcontextprotocol/protocolVersion` and `clientCapabilities`. The `MCP-Protocol-Version`, `Mcp-Method` and `Mcp-Name` headers must agree with the body. `server/discover` describes the server.
  - `2025-11-25`, `2025-06-18` and `2025-03-26`: begin with `initialize`.
- JSON-RPC batches are not accepted.
- Requests from a browser page of another site, as told by the `Origin` header, are refused with `403`.

## Tools

| Tool | Scope | Arguments | Does |
| --- | --- | --- | --- |
| `list_resources` | `read` | `query`, `kind` (`resource` or `quick_share`), `tag`, `cursor` | Lists resources with their tags, most recently updated first, 50 to a page; `query` matches names and filenames, `tag` keeps those carrying it |
| `list_tags` | `read` | none | Lists the account's tags with how many resources carry each |
| `read_resource` | `read` | `resource`, `version`, `offset`, `limit` | Returns the text of the current version or an earlier one, in parts of 32 KiB, or of `limit` bytes from 4 KiB to 256 KiB |
| `list_versions` | `read` | `resource` | Lists a resource's versions, each with its size, the change in size from the version before it and the SHA-256 of the stored bytes |
| `diff_versions` | `read` | `resource`, `from`, `to` | Compares two versions as a unified diff with three lines of context; by default the current version and the one before it |
| `create_resource` | `write` | `content`, `filename`, `name`, `tags` | Creates a resource from text |
| `edit_resource` | `write` | `resource`, `base_version`, `edits` | Replaces parts of the text, each `old_text` with its `new_text`, and saves the result as a new version |
| `update_resource` | `write` | `resource`, `base_version`, `content`, `name`, `filename` | Saves the text as a new version, keeping the resource's encoding and line endings |
| `tag_resource` | `write` | `resource`, `add`, `remove` | Adds and removes tags; the content and version stay as they are |
| `share_resource` | `share` | `resource`, `name`, `expires_in`, `max_uses` | Creates a share link and returns its address |
| `unshare_resource` | `share` | `resource`, `link` | Revokes one link, or every link of the resource when `link` is omitted |

- A `resource` is an ID, an ID prefix of at least six characters, an exact name or filename, or the address of the resource's page or of one of its share links. Naming a resource by a share link's address neither uses up the link nor requires it to still work.
- `edit_resource` and `update_resource` are refused when `base_version` is no longer the current version, so newer content is never overwritten. The app should read again and redo its change. With `update_resource`, text identical to the current text is not a conflict: it saves nothing.
- `edit_resource` applies its edits in order, each to the text the ones before it left. Each `old_text` must appear exactly once, or `replace_all` must be set; otherwise nothing is saved. Line breaks match whether the file uses LF or CRLF.
- `share_resource` takes `expires_in` as a duration such as `1h`, `24h` or `7d` (at most `30d`; default `7d`), and `max_uses` from `0` (no limit) to 1,000,000. An app cannot make a link that never expires; the owner can, on the resource's page. Links created by an app are marked in the resource's share list with the app's name.
- A tag is free text of at most 24 characters; one differing only in case from a tag the account uses is that tag. Encrypted resources and quick shares can be tagged, and their tags are listed, as tags are not encrypted.
- Binary content is described rather than returned. Text longer than a part is returned in parts; a unified diff longer than 32 KiB is cut short. Quick shares can be listed and read, but not updated or shared again.
- Every tool declares an `outputSchema` for its structured result.
- A refusal, such as a conflict or a full quota, is returned as a tool result with `isError` set and a message the model can act on. A resource of another account is reported exactly as one that does not exist.

## Encrypted resources

The server cannot decrypt end-to-end encrypted resources and does not ask for the master password. In lists they appear only as placeholders, with their ID, size, update time, address and tags; their names and filenames are not given. Reading one returns a notice instead of content, and they cannot be updated or shared through MCP.

## Limits

| Limit | Value |
| --- | --- |
| Tool calls | 300 a minute per authorization, and 600 a minute per IP address |
| Share links created | 30 an hour per authorization |
| Failed authentications | 30 a minute per IP address |
| Content | The same size and storage limits as the account's other resources |

A request over a limit is answered `429`.
