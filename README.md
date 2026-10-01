<p align="center">
  <img src="docs/logo.png" alt="PlainMote" width="160" height="160">
</p>

# PlainMote

English | [简体中文](README.zh-CN.md)

PlainMote is a web service for sharing configuration files, logs and other files. Content is stored and delivered byte for byte. Each piece of content can be shared through a separate link for each recipient or machine; each link has its own expiry and use limit, can be revoked independently, and records every access made through it.

Reference deployment: [plainmote.link](https://plainmote.link)

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/screenshots/quick-share-dark.png">
  <img src="docs/screenshots/quick-share-light.png" alt="The quick share box with a pasted log, its times, levels and key=value pairs highlighted, and the lifetime scale from 10 minutes to 30 days">
</picture>

## Features

- **Quick share**: no account is required. Text or a file of up to 10 MiB submitted on the home page receives a share link valid for 10 minutes, 1 hour, 1 day, 7 days or 30 days; the default is 1 hour. Content can also be submitted from the command line with curl.
- **Resources and share links**: signed-in users can store files (100 MiB per account by default) and issue any number of share links for each resource. Each link has its own expiry and use limit; revoking one leaves the others in effect. When a resource is updated, every link keeps its address and serves the new content immediately.
- **Access history**: every access through a link is recorded with its time, result, source IP address and client details, visible only to the owner of the resource. Records are kept for 30 days by default.
- **Version history**: each save that changes a resource's content retains the version it replaces, up to 10 per resource, each for 30 days after it was replaced. Earlier versions can be viewed, compared with any other version, restored as a new version, copied into a new resource or deleted individually. Text is compared line by line; images, audio, video and other files are shown side by side with their size, SHA-256 digest and, for images, dimensions. Earlier versions count towards storage but occupy only the space not used by current content; when a save requires that space, the versions replaced longest ago are removed first. The access history records which version each access received. A save based on an outdated version is not applied; the conflict is reported instead, so that newer content is not overwritten.
- **Remote resources**: a resource may consist of a public URL only, in which case the service fetches the content from the origin on each access.
- **Conditional requests**: share links answer `HEAD`, and responses carry an `ETag` (and, for stored content, `Last-Modified`). A request with `If-None-Match` or `If-Modified-Since` receives `304` with no body while the content is unchanged. Every response counts as one use and is recorded.
- **Editor**: built on CodeMirror 6, with syntax highlighting for YAML, JSON, TOML, XML, INI and `.env`, shell, nginx, Dockerfile, SQL, diffs and logs; the quick share box uses the same editor. UTF-8, UTF-16, GB18030, Big5, Shift_JIS and other encodings are detected, and files are saved in their original encoding and line endings.
- **End-to-end encryption (optional)**: signed-in users can enable it in their account settings. Quick shares created on the home page, whether text or files, are then encrypted in the browser before upload, and the service stores only ciphertext.
- **Media preview**: when a share link is opened in a browser, images are displayed and audio and video are played; other clients receive the original bytes.
- **No JavaScript required**: core functions work with scripts disabled; scripts only enhance the interface. End-to-end encryption is the exception, as encryption and decryption run in the browser.
- **Interface**: English, Simplified Chinese, Traditional Chinese, Japanese, French and German, selected from the browser's language. The home page and legal pages also have a separate address per language (such as `/zh-cn/` or `/ja/`) for search engines. Light and dark themes are available.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/screenshots/share-links-dark.png">
  <img src="docs/screenshots/share-links-light.png" alt="A config and its share links: one for each machine and each person, each with its own time left and use count, with revoked and used-up links listed apart">
</picture>

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/screenshots/access-history-dark.png">
  <img src="docs/screenshots/access-history-light.png" alt="Access history: the time, result, share link and source IP of every access, machines polling alongside people opening the link">
</picture>

## Command line

```bash
tail -n 200 app.log | curl -F 'content=<-' https://plainmote.link/paste
curl -F content=@app.log https://plainmote.link/paste
curl --data-binary @app.log 'https://plainmote.link/paste?ttl=1d&filename=app.log'
curl https://plainmote.link/paste    # prints usage
```

`ttl` accepts `10m`, `1h`, `1d`, `7d` or `30d`; a bare number is read as minutes. On success the response body is the share link on a single line, suitable for a pipe or `$(...)`. `/llms.txt` provides the same information to language models in the llmstxt.org format.

## Security and privacy

- **Links are credentials**: tokens are 256-bit random values. The database stores only their SHA-256 digest, used for lookup, and the original encrypted with AES-GCM.
- **No cross-site embedding**: requests from other sites that embed a share link as an image, video or script are refused before the token is read.
- **Not indexed**: share links, resources and account pages are excluded from search engines; only the pages describing the service can be indexed.
- **No web hosting**: types a browser may execute (HTML, scripts, SVG and similar) are never delivered as themselves. For quick shares made without an account, text is delivered as plain text, images, audio and video confirmed by their file signatures are delivered as their own types, and all other files are delivered only as downloads; encrypted shares are always delivered as ciphertext.
- **End-to-end encryption**: uses only the browser's built-in WebCrypto, with AES-256-GCM. By default the key is carried in the part of the link after `#`, which browsers do not send to the server. The creator may use a four-character code instead, in which case the recipient's browser derives the key from the code with PBKDF2-SHA-256 (600,000 iterations).
- **Limits of encryption**: decryption requires JavaScript; a lost link or code cannot be recovered; content size and access history are not encrypted. A code is drawn from 31 characters, about 920,000 combinations: it prevents viewing by someone who merely sees the link, but not by an attacker who obtains the link and tries every code, so sensitive content should be shared with the full link and its key. Because the encryption code is delivered by the service, its trustworthiness depends on the integrity of the deployed code; the published source code allows this to be verified.

## Self-hosting

PlainMote is a single stateless Go service with the following dependencies:

- PostgreSQL
- S3-compatible object storage (such as Cloudflare R2 or MinIO)
- A GitHub OAuth App (for sign-in)

```bash
cp .env.example .env    # fill in the database, object storage, OAuth and keys
docker compose up -d
```

Building the image, preparing the external services, public exposure, upgrades and rollbacks are described in [`docs/deployment.md`](docs/deployment.md) (in Chinese).

### Configuration

All settings are read from environment variables and validated at startup; the service exits immediately if a required value is missing or any value is invalid.

Required:

| Variable | Purpose |
| --- | --- |
| `DATABASE_URL` | PostgreSQL connection URL |
| `PLAINMOTE_PUBLIC_URL` | Public HTTP(S) origin, used for share links and the OAuth callback |
| `PLAINMOTE_BLOB_ENDPOINT` | S3-compatible endpoint |
| `PLAINMOTE_BLOB_BUCKET` | Bucket name |
| `PLAINMOTE_BLOB_ACCESS_KEY` | Access Key ID |
| `PLAINMOTE_BLOB_SECRET_KEY` | Secret Access Key |
| `PLAINMOTE_TOKEN_KEY` | 32-byte token encryption key (64 hex digits or Base64); changing it makes existing share links undecryptable |
| `GITHUB_CLIENT_ID` | GitHub OAuth Client ID |
| `GITHUB_CLIENT_SECRET` | GitHub OAuth Client Secret |

Optional:

| Variable | Default | Purpose |
| --- | --- | --- |
| `PLAINMOTE_LISTEN` | `:8964` | HTTP listen address |
| `PLAINMOTE_BLOB_REGION` | `auto` | S3 region |
| `PLAINMOTE_TRUSTED_PROXIES` | empty | Trusted proxy IPs or CIDRs, comma-separated |
| `PLAINMOTE_SESSION_TTL` | `720h` | Sign-in session lifetime |
| `PLAINMOTE_MAX_CONTENT_MIB` | `10` | Size limit for one piece of content |
| `PLAINMOTE_LOG_RETENTION` | `720h` | How long access history is kept; `0` keeps it forever |
| `PLAINMOTE_REGISTRATION_MODE` | `allowlist` | New account policy: `open`, `allowlist` or `closed` |
| `GITHUB_ALLOWED_IDS` | empty | Numeric GitHub user IDs allowed to register in `allowlist` mode, comma-separated |
| `PLAINMOTE_ANONYMOUS` | `false` | Whether the home page offers quick share without signing in |
| `PLAINMOTE_OPERATOR` | empty | Operator name, shown in the about page, privacy policy and terms |
| `PLAINMOTE_CONTACT_EMAIL` | empty | Contact email; the about, privacy, terms and contact pages are served only when it is set |
| `PLAINMOTE_SOURCE_URL` | this repository | Where the "Source code" link at the foot of every page points; a modified deployment must point it at its modified source, as the AGPL requires |
| `PLAINMOTE_ADMIN_KEYS` | empty | Ed25519 public keys allowed to call the admin interface, Base64, comma-separated; the interface does not exist while this is empty |
| `PLAINMOTE_ADMIN_ORIGINS` | empty | Browser origins of admin pages allowed to call the admin interface across sites, comma-separated |

### Administration

The service has no administration pages. It provides a signed JSON interface instead, described in [`docs/admin-api.md`](docs/admin-api.md) (in Chinese), for use by an administration page hosted elsewhere or run locally. Requests are signed with an Ed25519 private key held by the administrator; the service holds only the public key. The interface covers a deployment overview, suspending accounts, taking down or deleting resources, revoking links and granting plans, and records every change in an append-only audit log. It never returns resource content, share addresses or the client details of access records. `go run ./cmd/plainmote-admin keygen` generates a key pair, and `go run ./cmd/plainmote-admin call` signs individual requests.

[PlainMote Admin](https://github.com/xwvike/plainmote-admin) (MIT) is an administration page for this interface. It runs entirely in the browser, stores the private key as a non-extractable key and can manage several deployments. It can be run locally, deployed as a static site or used at its published address, [xwvike.github.io/plainmote-admin](https://xwvike.github.io/plainmote-admin/). The origin it is served from must be listed in `PLAINMOTE_ADMIN_ORIGINS`. Browsers store the key per origin, so any script served from the same origin can use it; the page should therefore be served from an origin that hosts no other content.

### About the legal pages

When `PLAINMOTE_CONTACT_EMAIL` is set, the service provides an about page, a privacy policy, terms of service and a contact page in Simplified Chinese and English. These texts describe the behaviour of the reference deployment; durations, sizes and other values are taken from the running configuration. **Operators of other deployments should review these texts and adapt them to the applicable law and their own operation** (the files are `internal/web/templates/legal_*.html`). Each operator is responsible for the commitments made on its site.

## Development

The development environment comprises PostgreSQL, MinIO and the application with live reload:

```bash
docker compose -f compose.dev.yaml up --build
```

The application is served at `http://localhost:8964` and the MinIO console at `http://localhost:9001`. Sign-in requires the GitHub OAuth settings in `.env`.

Running the tests:

```bash
docker compose -f compose.dev.yaml up -d postgres
PLAINMOTE_TEST_DATABASE_URL='postgres://plainmote:plainmote@127.0.0.1:54329/plainmote?sslmode=disable' go test ./...
go vet ./...
node tools/e2ee/e2ee_test.mjs
npm --prefix tools/codemirror test
```

The dependencies of the editor and of encoding detection are committed as a bundle and are not loaded from a third-party CDN at runtime. The bundle is rebuilt with `tools/codemirror/build.sh`; regular builds do not require Node.js.

```text
cmd/plainmote/       service entry point
cmd/plainmote-admin/ admin key generation and request signing
internal/app/        service assembly and HTTP lifecycle
internal/auth/       GitHub OAuth
internal/blob/       S3-compatible object storage
internal/config/     environment parsing and validation
internal/store/      PostgreSQL schema and business rules
internal/upstream/   remote URL validation and fetching
internal/web/        routes, pages, static assets and public delivery
tools/               editor bundle and encryption tests
docs/                deployment guide and images
```

## License

Copyright (C) 2026 xwvike

Released under the [GNU Affero General Public License v3.0](LICENSE). Under this license, anyone who modifies the project and offers it to others over a network must make the modified source code available to those users.

Licenses of third-party components (CodeMirror and others, MIT; jschardet, LGPL-2.1) are listed in [`tools/codemirror/NOTICE.md`](tools/codemirror/NOTICE.md).
