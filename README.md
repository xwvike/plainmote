<p align="center">
  <img src="docs/logo.png" alt="PlainMote" width="160" height="160">
</p>

# PlainMote

English | [简体中文](README.zh-CN.md)

Turn text and files into share links. The content is kept byte for byte; every link has its own expiry and use limit, can be revoked on its own, and every access through it is recorded.

Try it at [plainmote.link](https://plainmote.link).

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/screenshots/share-links-dark.png">
  <img src="docs/screenshots/share-links-light.png" alt="A resource and its share links: one link per recipient, each with its own time left and use count, with revoked and used-up links listed apart">
</picture>

## Features

- **Quick share**: no account needed. Paste text on the home page and get a link that expires within 30 minutes, or share straight from a terminal with curl.
- **Resources and share links**: sign in to keep text and files, then give each recipient a separate link with its own expiry and use limit. Revoking one leaves the others working.
- **Access history**: every access through a link is recorded with its time, result, source IP and client, visible only to the owner of the resource.
- **Remote resources**: keep just a public URL; the content is fetched from the origin on every access.
- **Online editor**: built on CodeMirror 6, with syntax highlighting for common config formats. It detects UTF-8, UTF-16, GB18030, Big5, Shift_JIS and other encodings, and saves back in the original encoding and line endings.
- **End-to-end encryption (optional)**: signed-in users can turn it on in their account settings. Quick shares are then encrypted in the browser, and the server stores only ciphertext.
- **Media preview**: in a browser, share links show images and play audio and video; other clients get the raw bytes.
- **Works without JavaScript**: the core features work with scripts turned off; scripts only enhance. End-to-end encryption is the exception, as encryption has to run in the browser.
- **Interface**: English, Simplified Chinese, Traditional Chinese, Japanese, French and German, chosen from the browser's language; the home page and legal pages also have an address per language (such as `/zh-cn/` or `/ja/`) for search engines. Light and dark themes.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/screenshots/access-history-dark.png">
  <img src="docs/screenshots/access-history-light.png" alt="Access history: the time, result, share link and source IP of every access">
</picture>

## Sharing from a terminal

```bash
tail -n 200 app.log | curl -F 'content=<-' https://plainmote.link/paste
curl -F content=@app.log https://plainmote.link/paste
curl --data-binary @app.log 'https://plainmote.link/paste?ttl=30&filename=app.log'
curl https://plainmote.link/paste    # prints usage
```

The response is the share link on a single line, ready for a pipe or `$(...)`. `/llms.txt` describes the same usage to language models in the llmstxt.org format.

## Security and privacy

- **A share link is a credential**: tokens are 256 random bits; the database keeps only a SHA-256 index and the original, encrypted with AES-GCM.
- **No hotlinking**: other sites cannot embed a share link as an image, video or script; such requests are refused before the token is read.
- **Not indexed**: share links, resources and account pages are all kept out of search engines; only the pages that describe the service can be indexed.
- **Not a web host**: types a browser might execute (HTML, scripts, SVG and so on) are never delivered as themselves; anonymous shares are delivered only as plain text (or, when encrypted, as ciphertext).
- **End-to-end encryption**: uses only the browser's built-in WebCrypto (AES-256-GCM; with a passphrase, the key is derived with PBKDF2-SHA-256).
  The key sits after the `#` in the link, which browsers never send to the server.
  Its limits are just as plain: decrypting needs JavaScript; a lost link or passphrase cannot be recovered; content size and access history are not encrypted;
  the encryption code is served by the server, so it is only as trustworthy as the deployed code, which is one reason this project is open source.

## Self-hosting

PlainMote is a single stateless Go service. It needs:

- PostgreSQL
- S3-compatible object storage (such as Cloudflare R2 or MinIO)
- A GitHub OAuth App, for sign-in

```bash
cp .env.example .env    # fill in the database, object storage, OAuth and keys
docker compose up -d
```

Building the image, preparing the external services, exposing it publicly, upgrades and rollbacks are covered in [`docs/deployment.md`](docs/deployment.md) (in Chinese).

### Configuration

Everything comes from environment variables, validated at startup; the service exits at once if one is missing or invalid.

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
| `PLAINMOTE_MAX_CONTENT_MIB` | `4` | Size limit for one piece of content |
| `PLAINMOTE_LOG_RETENTION` | `720h` | How long access history is kept; `0` keeps it forever |
| `PLAINMOTE_REGISTRATION_MODE` | `allowlist` | New account policy: `open`, `allowlist` or `closed` |
| `GITHUB_ALLOWED_IDS` | empty | Numeric GitHub user IDs allowed to register in `allowlist` mode, comma-separated |
| `PLAINMOTE_ANONYMOUS` | `false` | Whether the home page offers quick share without signing in |
| `PLAINMOTE_OPERATOR` | empty | Operator name, shown in the about page, privacy policy and terms |
| `PLAINMOTE_CONTACT_EMAIL` | empty | Contact email; the about, privacy, terms and contact pages are served only when it is set |
| `PLAINMOTE_SOURCE_URL` | this repository | Where the "Source code" link at the foot of every page points; a modified deployment must point it at its modified source, as the AGPL requires |

### About the legal pages

With `PLAINMOTE_CONTACT_EMAIL` set, the service serves an about page, privacy policy, terms of service and contact page (in Simplified Chinese and English).
They are written for how the reference deployment actually behaves, with durations, sizes and other values taken from the running configuration.
**If you deploy your own instance, review and adapt them to the law where you operate and to how you run the service**
(they live in `internal/web/templates/legal_*.html`); you are responsible for what your site promises.

## Development

The development environment starts PostgreSQL, MinIO and the app with live reload:

```bash
docker compose -f compose.dev.yaml up --build
```

The app is at `http://localhost:8964` and the MinIO console at `http://localhost:9001`. Signing in needs the GitHub OAuth settings in `.env`.

Tests:

```bash
docker compose -f compose.dev.yaml up -d postgres
PLAINMOTE_TEST_DATABASE_URL='postgres://plainmote:plainmote@127.0.0.1:54329/plainmote?sslmode=disable' go test ./...
go vet ./...
node tools/e2ee/e2ee_test.mjs
npm --prefix tools/codemirror test
```

The editor and encoding detection dependencies ship with the repository as a bundle and are never loaded from a third-party CDN at runtime.
Rebuild them with `tools/codemirror/build.sh`; everyday builds do not need Node.js.

```text
cmd/plainmote/       service entry point
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

Released under the [GNU Affero General Public License v3.0](LICENSE). Under that license, if you modify this project and offer it to others over a network, you must make the modified source available to those users.

Licenses of third-party components (CodeMirror and others, MIT; jschardet, LGPL-2.1) are listed in [`tools/codemirror/NOTICE.md`](tools/codemirror/NOTICE.md).
