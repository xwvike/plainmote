# Administration

English | [简体中文](admin.zh-CN.md)

PlainMote has no administration pages. Operators manage the service through a set of HTTP endpoints protected by Ed25519 signatures. The administration page is deployed separately from the service and can run anywhere, including locally. The service stores only the public key; the private key that signs requests is held by the operator.

The first part of this document describes what administration covers and how it affects users; the second part is the complete API reference.

## Capabilities

| Area | Operations |
| --- | --- |
| Deployment overview | Version, main settings, database and object storage status, user and resource counts and sizes, share link and access statistics, the result of the last scheduled cleanup |
| Accounts | Find by login or a sign-in method's account ID; view resource counts, storage and active plans; suspend and unsuspend |
| Plans | Create and delete plans; grant plans to accounts or revoke them, optionally with an expiry |
| Resources | Find by owner, name or status; view metadata and share links; take down and restore; delete |
| Share links | Find the resource and link behind a reported share address; revoke a single link |
| Audit log | Review every change made through the admin interface |

The interface has the following limits:

- **No access to content**: no endpoint returns resource content, earlier versions, share link addresses or the visitor details (IP address and client information) in access records. Operators see metadata and aggregate figures only; for a remote resource, only the host name of its origin is given.
- **Every change is justified and recorded**: each change requires a reason and is written to an append-only audit log with its time, key, operation, target, reason and source IP. Audit records cannot be modified or deleted, and are not removed with the access history.
- **Registration excluded**: whether new accounts can register is set by `PLAINMOTE_REGISTRATION_MODE` at startup.

### Effect on users

| Operation | What users see |
| --- | --- |
| Suspend an account | All of the account's sign-in sessions and command line sign-ins end immediately, and the sign-in page shows the reason on the next attempt; all of its share links stop delivering. No data is deleted |
| Unsuspend an account | The account can sign in again; share links resume with their original expiry and use limits; command lines must sign in again |
| Take down a resource | All of the resource's share links stop delivering; the owner sees the reason on the resource page, the resource list shows it as taken down, and the owner can no longer create share links for it or copy its earlier versions into new resources, but can still edit or delete it. A quick share made without signing in has no owner to notify and is deleted instead; a signed-in user's quick share is treated as a resource |
| Restore a resource | Share links resume with their original expiry and use limits |
| Delete a resource | The same as deletion by the owner: content, earlier versions and share links are deleted; access history is kept |
| Revoke a link | The same as revocation by the owner |

Public requests to share links of a suspended account or a taken-down resource receive `401`, the same response as for an ended link, with no reason given. The owner's access history records them as "account suspended" and "taken down" respectively.

## Setup

### Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `PLAINMOTE_ADMIN_KEYS` | empty | Ed25519 public keys allowed to call the admin interface: Base64-encoded 32-byte raw keys, comma-separated. Old and new keys may coexist during rotation. While empty, the admin interface does not exist and every path returns `404` |
| `PLAINMOTE_ADMIN_ORIGINS` | empty | Origins of pages allowed to call the admin interface from a browser across sites, comma-separated, such as `http://localhost:5173,https://admin.example.com`. While empty, cross-site requests are not answered |

With Docker Compose, `compose.yaml` already passes both variables to the container; set them in `.env`.

### Keys

The `plainmote-admin` tool in this repository generates a key pair:

```bash
go run ./cmd/plainmote-admin keygen
# key id:  <key identifier>
# public:  <Base64 public key>   set as PLAINMOTE_ADMIN_KEYS
# private: <JWK private key>     import into the admin page, or save to a file for call; never place it on the server
```

The private key should be generated and kept on the operator's own device. A leak of the server's environment exposes only the public key, which is not enough to forge an admin request.

### Administration page

[PlainMote Admin](https://github.com/xwvike/plainmote-admin) (MIT) is an administration page for this interface. It runs entirely in the browser, stores the private key as a non-extractable key and can manage several deployments. It can be run locally, deployed as a static site or used at its published address, [xwvike.github.io/plainmote-admin](https://xwvike.github.io/plainmote-admin/). The origin it is served from must be listed in `PLAINMOTE_ADMIN_ORIGINS`. Browsers store the key per origin, so any script served from the same origin can use it; the page should therefore be served from an origin that hosts no other content.

### Command line requests

Without the administration page, `plainmote-admin call` signs and sends individual requests:

```bash
go run ./cmd/plainmote-admin call -key admin.jwk GET https://plainmote.link/_admin/v1/overview
go run ./cmd/plainmote-admin call -key admin.jwk POST https://plainmote.link/_admin/v1/lookup '{"link":"https://plainmote.link/d/…"}'
```

## API reference

### Conventions

- The path prefix is `/_admin/v1/`. Request and response bodies are JSON; a request body may not exceed 64 KiB.
- Times are RFC 3339 in UTC, sizes are in bytes and IDs are UUIDs.
- List endpoints accept the query parameters `page` (from 1, default 1) and `size` (1–100, default 20) and return `{"items": [...], "total": <count>}`.
- Endpoints that make a change require a `reason` field of 1–500 characters in the request body. It is written to the audit log; the reasons for suspending an account and taking down a resource are shown to the user.

### Authentication

Every request carries the following headers:

| Header | Content |
| --- | --- |
| `X-PlainMote-Key` | Key identifier: the first 16 hexadecimal characters of the SHA-256 digest of the public key |
| `X-PlainMote-Timestamp` | Unix time in seconds |
| `X-PlainMote-Nonce` | At least 16 random bytes, Base64url without padding, at most 128 characters |
| `X-PlainMote-Signature` | Ed25519 signature of the signing string, Base64url without padding |

The signing string is the following six lines joined with `\n`, with no trailing newline:

```
PLAINMOTE-ADMIN-V1
<request method, upper case>
<request path and query string, exactly as sent>
<value of X-PlainMote-Timestamp>
<value of X-PlainMote-Nonce>
<SHA-256 digest of the request body, lower-case hex; the digest of the empty string when there is no body>
```

A request is refused when its timestamp differs from the server's time by more than 300 seconds, its nonce has been seen within the last 660 seconds, its key identifier is not in `PLAINMOTE_ADMIN_KEYS`, its signature is invalid or its body exceeds 64 KiB. Only a request that passes verification uses up its nonce. Used nonces are held in the service's memory and are forgotten when the process restarts.

A failed authentication receives the same plain-text `404` as a path that does not exist. After more than 30 failures from one source IP within a minute, every request from that IP receives `404` for the rest of that minute.

In browsers, WebCrypto supports Ed25519 (Chrome 137+, Firefox 129+, Safari 17+). The private key should be imported with `extractable: false` and kept for the current session, or stored in IndexedDB as a `CryptoKey` object, never as plain text.

### Cross-origin requests

Only when the request's `Origin` is listed in `PLAINMOTE_ADMIN_ORIGINS` does the service answer the `OPTIONS` preflight (`204`), with:

- `Access-Control-Allow-Origin`: the requesting origin;
- `Access-Control-Allow-Methods`: `GET, POST, DELETE`;
- `Access-Control-Allow-Headers`: `Content-Type` and the four authentication headers above;
- `Access-Control-Max-Age`: `600`.

The admin interface uses no cookies and never sends `Access-Control-Allow-Credentials`.

### Errors

An authenticated request that fails receives `{"error": "<code>", "message": "<description>"}`:

| Status | Code | Meaning |
| --- | --- | --- |
| `400` | `bad_request` | Invalid request body or query parameter |
| `400` | `bad_reason` | `reason` missing, or not 1–500 characters long |
| `404` | `not_found` | The object or endpoint does not exist |
| `409` | `already_suspended`, `not_suspended` | The account is already in that state |
| `409` | `already_taken_down`, `not_taken_down` | The resource is already in that state |
| `409` | `already_revoked` | The link is already revoked |
| `409` | `plan_exists`, `invalid_plan` | Duplicate plan name, or invalid name or limits |
| `409` | `plan_protected`, `plan_in_use` | The default plan cannot be deleted, granted or revoked; a plan still held by an account cannot be deleted |
| `500` | `internal` | Server failure; details go to the server log only |

### Endpoints

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/_admin/v1/overview` | Deployment overview |
| `GET` | `/_admin/v1/users` | List accounts |
| `GET` | `/_admin/v1/users/{id}` | Account details |
| `POST` | `/_admin/v1/users/{id}/suspend` | Suspend an account |
| `POST` | `/_admin/v1/users/{id}/unsuspend` | Unsuspend an account |
| `POST` | `/_admin/v1/users/{id}/plans` | Grant a plan |
| `DELETE` | `/_admin/v1/users/{id}/plans/{plan_id}` | Revoke a plan |
| `GET` | `/_admin/v1/plans` | List plans |
| `POST` | `/_admin/v1/plans` | Create a plan |
| `DELETE` | `/_admin/v1/plans/{id}` | Delete a plan |
| `GET` | `/_admin/v1/resources` | List resources |
| `GET` | `/_admin/v1/resources/{id}` | Resource details |
| `GET` | `/_admin/v1/resources/{id}/links` | A resource's share links |
| `POST` | `/_admin/v1/resources/{id}/takedown` | Take down a resource |
| `POST` | `/_admin/v1/resources/{id}/restore` | Restore a resource |
| `DELETE` | `/_admin/v1/resources/{id}` | Delete a resource |
| `POST` | `/_admin/v1/links/{id}/revoke` | Revoke a link |
| `POST` | `/_admin/v1/lookup` | Find the resource and link behind a share address |
| `GET` | `/_admin/v1/audit` | Audit log |

### Objects

#### Account

| Field | Type | Description |
| --- | --- | --- |
| `id` | string | Account ID |
| `github_id` | string | Numeric GitHub user ID; empty for an account without GitHub sign-in |
| `login` | string | Username: the GitHub login, or the Google name for an account without GitHub sign-in |
| `name` | string | Display name, from the same provider as `login` |
| `identities` | array | Sign-in methods, GitHub first: `provider` (`github` or `google`), `subject` (the provider's account ID), `login`, `created_at` |
| `created_at` | time | Registration time |
| `last_signed_in_at` | time \| null | Time of the most recent sign-in |
| `status` | string | `active` or `suspended` |
| `suspended_reason` | string | Reason for suspension; empty when not suspended |
| `resources` | integer | Number of resources |
| `resources_limit` | integer | Resource limit |
| `live_links` | integer | Number of live share links |
| `storage.current_bytes` | integer | Storage used by current content |
| `storage.history_bytes` | integer | Storage used by earlier versions |
| `storage.limit_bytes` | integer | Storage limit |
| `plans` | array | Account details only: active plans, described below |
| `master_password` | boolean | Whether the account has set a master password for end-to-end encryption |
| `master_password_at` | time \| null | When it was set; `null` without one. Nothing of the keys is ever returned, and a forgotten master password cannot be recovered by the operator |

`resources_limit` and `storage.limit_bytes` are the sums over all plans currently in effect, taking grant and expiry times into account. Each item of `plans`:

| Field | Type | Description |
| --- | --- | --- |
| `plan_id` | string | Plan ID |
| `name` | string | Plan name |
| `default` | boolean | Whether it is the default plan |
| `granted_at` | time | Grant time |
| `expires_at` | time \| null | Expiry; `null` means it does not expire |

#### Plan

| Field | Type | Description |
| --- | --- | --- |
| `id` | string | Plan ID |
| `name` | string | Name |
| `max_resources` | integer | Resource limit |
| `max_storage` | integer | Storage limit |
| `default` | boolean | Whether it is the default plan, which every account holds |
| `users` | integer | Number of accounts holding the plan |

#### Resource

| Field | Type | Description |
| --- | --- | --- |
| `id` | string | Resource ID |
| `owner.id`, `owner.login` | string | Owner; `owner.login` is `anonymous` for quick shares made without signing in |
| `name` | string | Name |
| `filename` | string | Filename |
| `kind` | string | `stored` (content kept by the service) or `remote` (remote resource) |
| `content_type` | string | Content type; `application/vnd.plainmote.sealed` for an end-to-end encrypted resource, whose `name` and `filename` are empty |
| `size` | integer | Size of the current content |
| `origin_host` | string | Host name of a remote resource's origin; the full address is never returned |
| `version` | integer | Current version number |
| `history_versions` | integer | Number of earlier versions |
| `history_bytes` | integer | Storage used by earlier versions |
| `live_links` | integer | Number of live share links |
| `created_at`, `updated_at` | time | Creation and update times |
| `status` | string | `active` or `taken_down` |
| `takedown_reason` | string | Reason for the takedown; empty when not taken down |
| `expires_at` | time \| null | Set on a signed-in quick share not yet kept as a resource: when it is deleted |
| `encrypted` | string | How the content is end-to-end encrypted: `master_password` (under the owner's master password; `name` and `filename` are empty, being encrypted too), `link_key` (a quick share encrypted the earlier way, its key only in its link), or empty when it is not encrypted |

#### Share link

| Field | Type | Description |
| --- | --- | --- |
| `id` | string | Link ID |
| `name` | string | Note |
| `created_at` | time | Creation time |
| `expires_at` | time \| null | Expiry; `null` means it does not expire |
| `max_uses` | integer | Use limit; `0` means unlimited |
| `used_count` | integer | Uses so far |
| `revoked_at` | time \| null | Revocation time |
| `live` | boolean | Whether the link is currently live |

No endpoint returns a share link's token or address.

#### Audit record

| Field | Type | Description |
| --- | --- | --- |
| `id` | string | Record ID |
| `at` | time | Time of the operation |
| `key` | string | Identifier of the key used |
| `action` | string | Operation, listed below |
| `target_type` | string | `user`, `plan`, `resource` or `link` |
| `target_id` | string | Target ID |
| `target_label` | string | Snapshot of the target's name at the time, kept after the target is deleted: the login for an account, the name (or filename) for a resource, the name for a plan, the note (or the resource's name) for a link |
| `reason` | string | The reason given |
| `detail` | object \| null | Additional information, listed below |
| `remote_ip` | string | Source IP of the request |

| `action` | `detail` |
| --- | --- |
| `user.suspend`, `user.unsuspend` | `null` |
| `user.plan.grant` | `{"plan_id", "plan_name", "expires_at"}` |
| `user.plan.revoke` | `{"plan_id", "plan_name"}` |
| `plan.create`, `plan.delete` | `{"max_resources", "max_storage"}` |
| `resource.takedown` | `null`; `{"deleted": true}` when a quick share made without signing in was deleted by the takedown |
| `resource.restore`, `resource.delete` | `null` |
| `link.revoke` | `{"resource_id"}` |

### Deployment overview

`GET /_admin/v1/overview`

```json
{
  "version": "adbcdce8a464",
  "revision": "adbcdce8a464…",
  "started_at": "2026-10-03T03:42:30Z",
  "config": {
    "public_url": "https://plainmote.link",
    "registration_mode": "closed",
    "anonymous": true,
    "max_content_bytes": 10485760,
    "log_retention_hours": 720,
    "history_keep": 10,
    "history_retention_days": 30
  },
  "health": { "database": "ok", "object_storage": "ok" },
  "users": { "total": 12, "suspended": 0, "signed_in_last_30d": 7, "master_password": 3 },
  "resources": { "total": 84, "remote": 3, "taken_down": 0, "current_bytes": 5996544, "history_bytes": 8540160, "history_versions": 41, "quick_shares": 5, "encrypted": 9 },
  "links": { "live": 51, "ended_last_7d": 9 },
  "access": {
    "last_24h": { "success": 120, "expired": 3, "exhausted": 1, "revoked": 0, "upstream_error": 0, "taken_down": 0, "suspended": 0 },
    "last_7d": { "success": 902, "expired": 17, "exhausted": 4, "revoked": 2, "upstream_error": 0, "taken_down": 0, "suspended": 0 }
  },
  "anonymous": { "live_pastes": 6, "bytes": 81920, "limit_bytes": 21474836480 },
  "prune": { "last_run_at": "2026-10-03T04:00:00Z", "access_logs": 0, "sessions": 2, "pastes": 1, "versions": 0 }
}
```

- `health`: the status of the database and object storage, `ok` or `error`; `unknown` when the object storage cannot be checked. Object storage is checked with a single metadata request on the bucket, without reading any object.
- `access`: accesses through share links in the last 24 hours and 7 days, by result. Repeated refusals that were merged into one record are counted individually.
- `users.master_password`: accounts that have set a master password. `resources.quick_shares`: signed-in quick shares not yet kept, which `resources.total` includes; `resources.encrypted`: end-to-end encrypted resources, quick shares among them.
- `anonymous`: the number of live quick shares made without signing in, the storage they use and the anonymous allowance. Signed-in users' quick shares count toward their own accounts.
- `prune`: the result of the most recent scheduled cleanup since the process started; `null` before the first one.

### Accounts

`GET /_admin/v1/users?q=&status=&page=&size=`

`q` matches part of the login (case-insensitive) or the whole account ID of any sign-in method; `status` is `active` or `suspended`. Returns a list of account objects (without `plans`).

`GET /_admin/v1/users/{id}`

Returns an account object, including `plans`.

`POST /_admin/v1/users/{id}/suspend`

Body: `{"reason": "…"}`. Suspends the account and returns the updated account object. Error code: `already_suspended`.

`POST /_admin/v1/users/{id}/unsuspend`

Body: `{"reason": "…"}`. Unsuspends the account and returns the updated account object. Error code: `not_suspended`.

### Plans

`GET /_admin/v1/plans`

Returns every grantable plan, as a list whose `total` is the number of plans. The anonymous allowance used by quick shares is not a plan and is not included.

`POST /_admin/v1/plans`

```json
{ "name": "friends", "max_resources": 100, "max_storage": 1073741824, "reason": "…" }
```

Creates a plan and returns `201` with the plan object. The name is 1–100 characters and the limits may not be negative. Error codes: `plan_exists`, `invalid_plan`.

`DELETE /_admin/v1/plans/{id}`

Body: `{"reason": "…"}`. Deletes the plan and returns `{"id": "…", "deleted": true}`. The default plan cannot be deleted (`plan_protected`). A plan that any account still holds, including grants that have expired but were not revoked, cannot be deleted (`plan_in_use`); revoke each grant first.

`POST /_admin/v1/users/{id}/plans`

```json
{ "plan_id": "…", "expires_at": "2027-01-01T00:00:00Z", "reason": "…" }
```

Grants the plan to the account; its limits are added to those of the account's other plans. `expires_at` may be `null` for no expiry. Granting a plan the account already holds only updates its expiry. The default plan cannot be granted (`plan_protected`). Returns the updated account object.

`DELETE /_admin/v1/users/{id}/plans/{plan_id}`

Body: `{"reason": "…"}`. Revokes a granted plan and returns the updated account object. The default plan cannot be revoked (`plan_protected`).

### Resources

`GET /_admin/v1/resources?owner=&q=&status=&kind=&encrypted=&page=&size=`

`owner` is an account ID; `q` matches part of the name or filename, case-insensitively - an encrypted resource has neither, and is found by its owner or its ID instead; `status` is `active` or `taken_down`; `kind` is `quick_share` (signed-in quick shares not yet kept) or `resource` (everything else); `encrypted=1` keeps only end-to-end encrypted ones. Returns a list of resource objects, newest first.

`GET /_admin/v1/resources/{id}`

Returns a resource object.

`GET /_admin/v1/resources/{id}/links?page=&size=`

Returns every share link of the resource, including ended ones, newest first.

`POST /_admin/v1/resources/{id}/takedown`

Body: `{"reason": "…"}`. Takes the resource down and returns the updated resource object; returns `{"id": "…", "deleted": true}` when a quick share made without signing in was deleted instead. Error code: `already_taken_down`.

`POST /_admin/v1/resources/{id}/restore`

Body: `{"reason": "…"}`. Restores the resource and returns the updated resource object. Error code: `not_taken_down`.

`DELETE /_admin/v1/resources/{id}`

Body: `{"reason": "…"}`. Deletes the resource with its earlier versions and share links; access history is kept. Returns `{"id": "…", "deleted": true}`.

### Share links

`POST /_admin/v1/links/{id}/revoke`

Body: `{"reason": "…"}`. Revokes the link and returns `{"id": "…", "revoked": true}`. Error code: `already_revoked`.

`POST /_admin/v1/lookup`

```json
{ "link": "https://plainmote.link/d/…" }
```

`link` may be the full share address, its path or the token alone. An address with a key after `#` - an encrypted share's - must be sent without it: the key opens the content and is never to leave the operator's browser. The service ignores anything after `#` all the same. Returns the resource the link belongs to and the link's own state:

```json
{ "resource": { "id": "…", "name": "checkout-api", "…": "…" }, "link": { "id": "…", "name": "web-01", "live": true, "…": "…" } }
```

`resource` is a resource object and `link` a share link object. The address is sent in a POST body so that it never appears in a URL or a log; the response does not repeat the token.

### Audit log

`GET /_admin/v1/audit?target=&page=&size=`

`target` is an object ID; when given, only records about that object are returned. Returns a list of audit record objects, newest first.
