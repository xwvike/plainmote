# HTTP API

English | [简体中文](api.zh-CN.md)

The endpoints under `/api/v1/` serve scripts, CI pipelines, and custom HTTP clients. Authenticate with a personal access token (`pmt_…`) created directly under **Settings › Authorized apps** (`/account/devices`).

The API can list, read, create, update, tag, trash, inspect version history, manage share links, and make quick shares. Permanent deletion/restoration from trash, access history, and account settings remain web-only.

## Authentication

1. Open **Settings › Authorized apps** (`/account/devices`) and click **New token**.
2. Give the token a name, choose **Read only** (`read`) or **Read & write** (`write`), and pick an expiration (`30 days`, `90 days`, `1 year`, or `Never`).
3. Copy the `pmt_…` token shown once upon creation and send it in the `Authorization` header:

```bash
Authorization: Bearer pmt_…
```

- Tokens in query parameters or cookies are never accepted.
- A read-only token receives `403 read_only` from writing endpoints.
- Only a digest of each token is stored. Revoke any token at any time in **Settings › Authorized apps**, or revoke the current token with `DELETE /api/v1/token`.

## Conventions

- Requests and responses are JSON, except reading and saving resource content (`GET`/`PUT /api/v1/resources/{id}/content`), which use raw bytes. Times are RFC 3339 in UTC and sizes are in bytes.
- Every response carries `X-PlainMote-Version`. No CORS headers are sent.
- Errors return `{"error": "<code>", "message": "<detail>"}`.

### Rate limits

| Subject | Limit | On limit |
| --- | --- | --- |
| Authentication failures | 30 / minute per IP | `429 rate_limited` |
| Authenticated requests | 300 / minute per token | `429 rate_limited` |
| Share link creation | 60 / hour per token | `429 rate_limited` |
| Resource deletions (trash) | 60 / hour per token | `429 rate_limited` |

## Endpoints

| Method | Path | Scope | Purpose |
| --- | --- | --- | --- |
| `GET` | `/api/v1/resources` | `read` | List resources (`?q=`, `?tag=`, `?kind=resource\|quick_share`, `?ref=`) |
| `POST` | `/api/v1/resources` | `write` | Create a resource (JSON or `multipart/form-data`) |
| `GET` | `/api/v1/resources/{id}` | `read` | Get resource details |
| `PATCH` | `/api/v1/resources/{id}` | `write` | Update `name`, `filename`, or `tags` |
| `DELETE` | `/api/v1/resources/{id}` | `write` | Move resource to trash & revoke its share links |
| `GET` | `/api/v1/resources/{id}/content` | `read` | Read raw bytes (optional `?version=N`) |
| `PUT` | `/api/v1/resources/{id}/content` | `write` | Save new version from raw body (optional `If-Match`) |
| `GET` | `/api/v1/resources/{id}/versions` | `read` | List current and earlier versions |
| `GET` | `/api/v1/resources/{id}/links` | `read` | List active share links |
| `POST` | `/api/v1/resources/{id}/links` | `write` | Create a share link |
| `DELETE` | `/api/v1/resources/{id}/links/{link_id}` | `write` | Revoke a share link |
| `PATCH` | `/api/v1/resources/{id}/tags` | `write` | Incrementally add/remove tags |
| `GET` | `/api/v1/tags` | `read` | List account tags with counts |
| `POST` | `/api/v1/quick-shares` | `write` | Create a quick share (JSON or `multipart/form-data`) |
| `GET` | `/api/v1/me` | `read` | Current account & token info |
| `DELETE` | `/api/v1/token` | `read` | Revoke the current token |

## Examples

```bash
TOKEN=pmt_…
API=https://plainmote.link/api/v1

# List resources
curl -fsS -H "Authorization: Bearer $TOKEN" "$API/resources?q=nginx"

# Create a resource with JSON
curl -fsS -X POST "$API/resources" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"filename":"nginx.conf","content":"listen 80;\n","tags":["prod"]}'

# Or upload a binary file with multipart/form-data
curl -fsS -H "Authorization: Bearer $TOKEN" \
  -F name=app.log -F filename=app.log -F content=@app.log "$API/resources"

# Read raw content (add ?version=1 for history)
curl -fsS -o nginx.conf -H "Authorization: Bearer $TOKEN" "$API/resources/$ID/content"

# Save a new version (If-Match is optional; pass "v1" to guard against conflicts)
curl -fsS -X PUT -H "Authorization: Bearer $TOKEN" \
  --data-binary @nginx.conf "$API/resources/$ID/content"

# Rename or replace tags
curl -fsS -X PATCH "$API/resources/$ID" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"Production Nginx","tags":["prod","edge"]}'

# Create a share link (omit expires_in for never-expiring)
curl -fsS -X POST "$API/resources/$ID/links" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"edge-node-1","expires_in":"30d","max_uses":0}'

# Revoke a share link
curl -fsS -X DELETE -H "Authorization: Bearer $TOKEN" "$API/resources/$ID/links/$LINK_ID"

# Move resource to trash (30-day retention)
curl -fsS -X DELETE -H "Authorization: Bearer $TOKEN" "$API/resources/$ID"
```

## Reading share links

Fetching a share link needs no token — the URL itself is the credential. `GET` returns the raw bytes and `HEAD` returns headers only, with `ETag` / `Last-Modified` (`304 Not Modified`) support. Updating a resource immediately updates what every share link serves.
