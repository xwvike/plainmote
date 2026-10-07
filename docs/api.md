# Command line API

English | [简体中文](api.zh-CN.md)

The endpoints under `/api/v1/` serve the `plainmote` command line and can also be called directly from scripts. Callers authenticate with a personal access token obtained through device authorization: the command line or script requests a code, and the account owner enters and approves it in a signed-in browser.

The API can list, read, save and create resources, and make quick shares. It cannot delete resources, manage share links or change the account.

## Obtaining a token

Device authorization follows RFC 8628 in three steps:

1. The client calls `POST /api/v1/device/code` and receives a device code (`device_code`, held only by the client) and a user code (`user_code`, shown to the user).
2. The user opens `verification_uri` (that is, `/cli/device`) in a signed-in browser, enters the user code, checks the device details and selects Allow.
3. The client calls `POST /api/v1/device/token` every `interval` seconds until it receives a token or is refused.

The user code is eight characters drawn from 32 characters excluding `0`, `O`, `1` and `I`, shown as `XXXX-XXXX` and accepted in any case. Both codes expire after 10 minutes, and a device code can be exchanged for a token only once. Clients should not place the user code in the address they open: typing the code by hand is the step that confirms the person approving is the one who saw it.

### Requesting a code

`POST /api/v1/device/code`

With `Content-Type: application/json` and the body:

| Field | Description |
| --- | --- |
| `scope` | `read` (read-only) or `write` (read-write) |
| `device` | Device name, shown on the confirmation page and the Account page; at most 64 characters |
| `os` | System and architecture, such as `darwin/arm64`; at most 64 characters |
| `version` | Client version; at most 64 characters |

No other fields are accepted. Response:

```json
{
  "device_code": "…",
  "user_code": "KQWD-7XHM",
  "verification_uri": "https://plainmote.link/cli/device",
  "expires_in": 600,
  "interval": 5
}
```

### Exchanging the code for a token

`POST /api/v1/device/token`

With `Content-Type: application/json` and the body `{"device_code": "…"}`. Once the user approves:

```json
{
  "access_token": "pmt_…",
  "token_type": "bearer",
  "scope": "write",
  "expires_at": "2026-12-31T08:20:00Z",
  "login": "mira"
}
```

Until then the response is `400` with `{"error": "<code>", "interval": <seconds>}`:

| Code | Meaning |
| --- | --- |
| `authorization_pending` | The user has not decided yet; keep polling every `interval` seconds |
| `slow_down` | Polling too fast; the interval grows by 5 seconds (up to 60); continue at the new `interval` |
| `access_denied` | The user denied the sign-in, or the account is suspended |
| `expired_token` | The device code has expired, has already been exchanged or does not exist |

## Using the token

- Tokens begin with `pmt_` and are accepted only in the `Authorization: Bearer <token>` header. Tokens in the address and cookies are never accepted.
- A token is valid for 90 days from sign-in; using it does not extend that. After it expires, a new token must be obtained.
- A read-only token can call the reading endpoints and receives `403` from the writing ones.
- The service stores only a digest of each token. The account owner can see each token's device, access, sign-in time and last use on the Account page and revoke it at any time; a token can also revoke itself with `DELETE /api/v1/token`. When an account is suspended, all of its tokens are deleted.
- The `plainmote` command line keeps its token in the `credentials` file of its configuration directory (mode 600) and also reads the `PLAINMOTE_TOKEN` environment variable.

## Conventions

- Requests and responses are JSON, except when reading and saving resource content. Times are RFC 3339 in UTC and sizes are in bytes.
- Every response carries an `X-PlainMote-Version` header with the server's version.
- The API answers no cross-site requests and sends no CORS headers.
- Errors are returned as `{"error": "<code>", "message": "<description>"}`.

### Rate limits

| Applies to | Limit | When exceeded |
| --- | --- | --- |
| Code requests | 10 per IP per 10 minutes | `429`, `rate_limited` |
| Token polling | 120 per IP per minute | `429`, `slow_down` |
| Authentication failures | 30 per IP per minute | `429`, `rate_limited` for the rest of the minute |
| Authenticated requests | 300 per token per minute | `429`, `rate_limited` |

### Error codes

| Status | Code | Meaning |
| --- | --- | --- |
| `400` | `bad_request` | Malformed request |
| `400` | `invalid_scope` | `scope` is not `read` or `write` |
| `400` | `invalid_ttl` | `ttl` is not one of the lifetimes a quick share takes |
| `400` | `refused` | The request was refused for the reason in `message`, such as an invalid filename or a save to a quick share |
| `401` | `unauthorized` | Missing token, or an invalid, expired or revoked one; the response carries `WWW-Authenticate: Bearer` |
| `403` | `read_only` | A read-only token cannot write |
| `404` | `not_found` | The resource or endpoint does not exist, including resources of other accounts |
| `409` | `reference` | A remote resource has no content stored by the service |
| `412` | `conflict` | The save was based on a version that is no longer current; `current_version` is the current version number |
| `413` | `too_large` | The content exceeds the size limit |
| `413` | `quota` | The account's resource or storage limit would be exceeded |
| `415` | `bad_request` | Wrong request body type |
| `428` | `precondition_required` | A save without `If-Match` |
| `429` | `rate_limited`, `slow_down` | Rate limit exceeded |
| `500` | `internal` | Server failure; details go to the server log only |

## Endpoints

| Method | Path | Access | Purpose |
| --- | --- | --- | --- |
| `POST` | `/api/v1/device/code` | no token | Request a code |
| `POST` | `/api/v1/device/token` | no token | Exchange the code for a token |
| `GET` | `/api/v1/me` | read | The account and token in use |
| `GET` | `/api/v1/keyring` | read | The account's keyring, for end-to-end encrypted resources |
| `DELETE` | `/api/v1/token` | read | Revoke the token in use |
| `GET` | `/api/v1/resources` | read | List resources, or resolve a reference |
| `GET` | `/api/v1/resources/{id}` | read | Resource details |
| `GET` | `/api/v1/resources/{id}/content` | read | Read the content |
| `PUT` | `/api/v1/resources/{id}/content` | write | Save as a new version |
| `POST` | `/api/v1/resources` | write | Create a resource |
| `POST` | `/api/v1/quick-shares` | write | Make a quick share |

## Resource object

| Field | Type | Description |
| --- | --- | --- |
| `id` | string | Resource ID |
| `name` | string | Name |
| `filename` | string | Filename |
| `size` | integer | Size of the current content |
| `type` | string | Content type |
| `encoding` | string | Text encoding, such as `utf-8` or `gb18030`; omitted for content that is not text |
| `version` | integer | Current version number |
| `updated_at` | time | Update time |
| `remote` | boolean | Whether it is a remote resource |
| `editable` | boolean | Whether it is text that can be edited |
| `encrypted` | boolean | Whether the content is end-to-end encrypted; omitted when false |
| `sealed_key` | string | Present only on a resource encrypted under the master password: its content key wrapped by the account key, Base64URL without padding. Such a resource has an empty `name` and `filename` and the type `application/vnd.plainmote.sealed` |
| `sealed_meta` | string | Present with `sealed_key`: the encrypted name, filename and content type, Base64URL without padding |
| `taken_down` | boolean | Whether the operator has taken it down; omitted when false |
| `expires_at` | time | Present only on a quick share: when it is deleted. A quick share is read only (`editable` is false) until it is converted into a resource on its page |
| `url` | string | Address of the resource page |

## Reference

### The account in use

`GET /api/v1/me`

```json
{ "login": "mira", "user_id": "5f0c…", "scope": "write", "expires_at": "2026-12-31T08:20:00Z", "device": "mira-mbp", "server_version": "adbcdce8a464" }
```

`user_id` is the account's ID, which the additional data of the account's wrapped keys includes.

### The keyring

`GET /api/v1/keyring`

Returns the keyring the master password unlocks, as it is kept: the account key wrapped by the master key and by the recovery key, and what the master key is derived with. Nothing in it opens without the master password or the recovery key. An account without a master password gets `404`.

```json
{ "kdf": "pbkdf2-sha256", "iterations": 600000, "salt": "…", "wrapped_by_password": "…", "wrapped_by_recovery": "…", "lock_minutes": 15, "version": 1, "user_id": "5f0c…" }
```

Formats and additional data are described in [encryption.md](encryption.md).

### Revoking the token in use

`DELETE /api/v1/token`

Returns `204` on success.

### Listing resources

`GET /api/v1/resources?q=`

Returns `{"resources": [<resource object>...]}`, most recently updated first, at most 500 items. `q` matches part of the name or filename, case-insensitively.

`GET /api/v1/resources?ref=`

Resolves a resource reference typed on the command line: `ref` may be a full ID, an ID prefix of at least six characters, or an exact name or filename. Every match is returned, at most 20; when several match, the caller decides, as the service does not choose.

### Resource details

`GET /api/v1/resources/{id}`

Returns a resource object.

### Reading the content

`GET /api/v1/resources/{id}/content`

Returns the current content as raw bytes, in the encoding and line endings it was saved with. Response headers:

| Header | Content |
| --- | --- |
| `Content-Type` | Content type, with `charset` for text |
| `ETag` | Version tag, such as `"v5"` |
| `X-PlainMote-Resource-Version` | Version number, such as `5` |
| `X-PlainMote-Encoding` | Text encoding; absent for content that is not text |

A remote resource returns `409` (`reference`). An encrypted resource returns its ciphertext; it is decrypted on the client with its `sealed_key`.

### Saving a new version

`PUT /api/v1/resources/{id}/content`

The request body is the new content as raw bytes. Request headers:

| Header | Content |
| --- | --- |
| `If-Match` | Required. The version tag the edit started from, such as `"v5"`; `*` saves regardless of the current version |
| `X-PlainMote-Encoding` | Optional. The content's text encoding, usually the `X-PlainMote-Encoding` returned when it was read; detected by the service when omitted |

On success:

```json
{ "version": 6, "new_version": true, "trimmed": 0 }
```

- `new_version`: `false` when the content equals the current version, in which case no version is created.
- `trimmed`: the number of earlier versions removed to make room.

When the resource has since been saved as a newer version, the response is `412`:

```json
{ "error": "conflict", "message": "…", "current_version": 7 }
```

The size limit is the deployment's `PLAINMOTE_MAX_CONTENT_MIB`. The name and filename are unchanged; the rules for earlier versions, storage and conflicts are the same as for saving in the browser.

For an encrypted resource, the body is the new content encrypted under the resource's content key, and the request also carries `X-PlainMote-Sealed-Meta`: the encrypted metadata to go with it, Base64URL without padding. As encrypting the same content twice gives different bytes, every save creates a version, and a save against any version but the current one is a conflict.

### Creating a resource

`POST /api/v1/resources`

The request body is `multipart/form-data`:

| Field | Description |
| --- | --- |
| `content` | Required; the content, as a file |
| `name` | Optional name; the filename is used when omitted |
| `filename` | Optional filename, used to recognise the content type |
| `encoding` | Optional text encoding; detected by the service when omitted |

Returns `201` with the resource object on success.

An encrypted resource is created with the content encrypted on the client and three more fields instead of `name`, `filename` and `encoding`: `id`, the new resource's ID (a UUID chosen by the client, as its encryption is bound to it); `sealed_key`, its content key wrapped by the account key; and `sealed_meta`, its encrypted metadata, both Base64URL without padding.

### Making a quick share

`POST /api/v1/quick-shares`

Does what the home page's box does for a signed-in account: stores the content with one share link that ends with it, and keeps it in the account's resources until then. Available only where the deployment offers quick shares; elsewhere the response is `404`.

The request body is `multipart/form-data`:

| Field | Description |
| --- | --- |
| `content` | Required; the content, as a file, at most 10 MiB |
| `filename` | Optional; the name at the end of the link |
| `ttl` | Optional; `10m`, `1h`, `1d`, `7d` or `30d`, default `1h` |

Returns `201` with the resource object and `share_url`, the link.

An encrypted quick share, as `plainmote share --encrypt` makes, is sent with the content encrypted on the client and these fields instead of `filename`, all Base64URL without padding except the IDs: `id` and `link_id`, UUIDs chosen by the client; `sealed_key`, the content key wrapped by the account key; `sealed_meta`, the encrypted metadata; `link_key`, the content key wrapped by the link's key; and `owner_key`, the link's key wrapped by the account key. `share_url` then lacks the key, which the client adds after `#`. The formats are described in [encryption.md](encryption.md). Text is delivered as plain text, images, audio and video as themselves, and anything else as a download, as for quick shares made on the home page. The quick share counts toward the account's storage. Its content is deleted when it expires; its access history is kept.

## Examples

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

A token written on the command line is visible to other users of the same machine in the process list. Scripts should read the token from a file, for example with curl's `-H @file`, or use the `plainmote` command line itself.
