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

- **Quick share**: no account is required. Text or a file of up to 10 MiB submitted on the home page receives a share link valid for 10 minutes, 1 hour, 1 day, 7 days or 30 days; the default is 1 hour. Content can also be submitted from the command line with curl. A quick share created while signed in appears, marked, under My resources and its access history belongs to its creator; it is read only until its link expires and can be converted into an ordinary resource to keep it, otherwise its content is deleted at expiry while its access history is kept.
- **Resources and share links**: signed-in users can store files (100 MiB per account by default) and issue any number of share links for each resource. Each link has its own expiry and use limit; revoking one leaves the others in effect. When a resource is updated, every link keeps its address and serves the new content immediately.
- **Access history**: every access through a link is recorded with its time, result, source IP address and client details, visible only to the owner of the resource. Records are kept for 30 days by default.
- **Version history**: each save that changes a resource's content retains the version it replaces, up to 10 per resource, each for 30 days after it was replaced. Earlier versions can be viewed, compared with any other version, restored as a new version, copied into a new resource or deleted individually. Text is compared line by line; images, audio, video and other files are shown side by side with their size, SHA-256 digest and, for images, dimensions. Earlier versions count towards storage but occupy only the space not used by current content; when a save requires that space, the versions replaced longest ago are removed first. The access history records which version each access received. A save based on an outdated version is not applied; the conflict is reported instead, so that newer content is not overwritten.
- **Remote resources**: a resource may consist of a public URL only, in which case the service fetches the content from the origin on each access.
- **Conditional requests**: share links answer `HEAD`, and responses carry an `ETag` (and, for stored content, `Last-Modified`). A request with `If-None-Match` or `If-Modified-Since` receives `304` with no body while the content is unchanged. Every response counts as one use and is recorded.
- **Editor**: built on CodeMirror 6, with syntax highlighting for YAML, JSON, TOML, XML, INI and `.env`, shell, nginx, Dockerfile, SQL, diffs and logs; the quick share box uses the same editor. UTF-8, UTF-16, GB18030, Big5, Shift_JIS and other encodings are detected, and files are saved in their original encoding and line endings.
- **Command line**: `plainmote` views, edits and uploads resources and makes quick shares from a terminal, editing in the machine's own editor and saving a new version when it is closed; signing in is confirmed in a browser.
- **End-to-end encryption (optional)**: signed-in users can set a master password and turn on end-to-end encryption for individual resources: content, name, filename and earlier versions are encrypted in the browser before upload, and the service stores only ciphertext; each share link carries its own key, and the recipient decrypts in the browser. Quick shares can be encrypted the same way, with a switch in the home page's box; their owner can show the link again after unlocking. See [docs/encryption.md](docs/encryption.md) for the design and formats.
- **Media preview**: when a share link is opened in a browser, images are displayed and audio and video are played; other clients receive the original bytes.
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

`plainmote` views, edits and uploads resources from a terminal. Editing opens the content in the machine's own editor (Zed, VS Code, Vim and others); when the editor is closed, the content is saved as a new version.

### Installation

```bash
curl -fsSL https://plainmote.link/cli/en | sh        # macOS, Linux
irm https://plainmote.link/cli/en.ps1 | iex          # Windows PowerShell
```

The install script detects the system and processor architecture, downloads the matching build served by the deployment, verifies its SHA-256 and installs it for the current user (`~/.local/bin` on macOS and Linux); no administrator rights are needed. The script can be read at the same address before running it. Builds are provided for macOS, Linux and Windows on amd64 and arm64, stamped with the version of the server that serves them; after the service is upgraded, running the install command again updates the command line.

`/cli/en` installs a command line that speaks English, `/cli/zh` one that speaks Chinese, and `/cli` follows the system's locale. The `/cli` page shows the install command for the visitor's system and lists the builds of every platform with their checksums.

### Usage

```bash
plainmote login                               # confirm the sign-in in a browser
plainmote ls nginx
plainmote edit nginx.conf
plainmote cat nginx.conf | grep listen
tail -n 200 app.log | plainmote push - --name app.log
plainmote push ./nginx.conf --to nginx.conf
plainmote share ./app.log --ttl 1d              # make a quick share; prints only its link
plainmote push ./prod.env --encrypt             # create an end-to-end encrypted resource
plainmote ls --decrypt prod                     # list with encrypted names decrypted
```

Commands that open an encrypted resource ask for the master password, or read it from `PLAINMOTE_MASTER_PASSWORD`.

A resource is named by its ID, an ID prefix of at least six characters, or its exact name or filename; when several resources match, the candidates are listed instead of one being chosen.

### Signing in

Signing in uses device authorization (RFC 8628). The command line first shows a code and the confirmation address, and opens the browser when Enter is pressed; the address can also be opened by hand on any device. The user enters the code in a signed-in browser, checks the device details and approves. The code is never placed in the address that is opened. `--read-only` requests read-only access; `--no-browser` does not open a browser.

Sign-ins are kept per server, so several deployments can be signed in at once. The server is taken from `--server`, then the `PLAINMOTE_SERVER` environment variable, then the setting saved by `plainmote server <url>`; writing commands such as `edit` and `push` name the server they changed.

### Editing and settings

A save is made against the version that was downloaded. If the resource was saved elsewhere in the meantime, the command line shows the difference and offers to save over it, to edit again on the newer version or to keep the file; an edit that cannot be saved is never discarded.

The editor is taken from the `PLAINMOTE_EDITOR` environment variable, then the setting saved by `plainmote config editor <command>`, then `$VISUAL` and `$EDITOR`; when none is set, the first edit lists the editors installed on the machine and remembers the one chosen. The language of messages is taken from the `PLAINMOTE_LANG` environment variable, then the setting saved by `plainmote config language zh|en|auto`, then the system's locale. `plainmote config` shows every setting.

The API the command line uses is described in [Command line API](docs/api.md).

### Sharing with curl

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
- **Only the site's own scripts**: a content security policy lets pages run only the script files the site serves, never inline scripts, and keeps other sites from framing them.
- **Command line tokens**: issued by device authorization and stored only as a digest. A token is valid for 90 days from sign-in, is read-only or read-write, and cannot delete resources, manage share links or change the account. Every signed-in device, with its last use, is listed on the Account page and can be revoked there. The API accepts a token only in the `Authorization` header, never from a cookie or the address, and answers no cross-site requests.
- **End-to-end encryption**: uses only the browser's built-in WebCrypto, with AES-256-GCM. By default the key is carried in the part of the link after `#`, which browsers do not send to the server. The creator may use a four-character code instead, in which case the recipient's browser derives the key from the code with PBKDF2-SHA-256 (600,000 iterations).
- **Limits of encryption**: decryption requires JavaScript; content cannot be recovered if both the master password and the recovery key are lost; content size and access history are not encrypted. A code is drawn from 31 characters, about 920,000 combinations: it prevents viewing by someone who merely sees the link, but not by an attacker who obtains the link and tries every code, so sensitive content should be shared with the full link and its key. Because the encryption code is delivered by the service, its trustworthiness depends on the integrity of the deployed code; the published source code allows this to be verified.

## Self-hosting

PlainMote is a single stateless Go service with the following dependencies:

- PostgreSQL
- S3-compatible object storage (such as Cloudflare R2 or MinIO)
- A GitHub OAuth App (for sign-in)

```bash
cp .env.example .env    # fill in the database, object storage, OAuth and keys
docker compose up -d
```

Building the image, preparing the external services, public exposure, upgrades and rollbacks are described in the [deployment guide](docs/deployment.md).

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
| `PLAINMOTE_CLI_DIR` | `/cli` | Directory holding the command line builds offered at `/cli`; the container image already includes them |

### Administration

The service has no administration pages. Instead it provides a set of JSON endpoints protected by Ed25519 signatures, covering a deployment overview, suspending and unsuspending accounts, plans, taking down and deleting resources, revoking links and an audit log, for use by an administration page hosted elsewhere or run locally. The service holds only the public key, and the endpoints never return resource content, share addresses or the visitor details of access records. [PlainMote Admin](https://github.com/xwvike/plainmote-admin) is an administration page built on them. The capabilities, setup and complete API reference are in the [administration guide](docs/admin.md).

### About the legal pages

When `PLAINMOTE_CONTACT_EMAIL` is set, the service provides an about page, a privacy policy, terms of service and a contact page in Simplified Chinese and English. These texts describe the behaviour of the reference deployment; durations, sizes and other values are taken from the running configuration. **Operators of other deployments should review these texts and adapt them to the applicable law and their own operation** (the files are `internal/web/templates/legal_*.html`). Each operator is responsible for the commitments made on its site.

## Documentation

- [Deployment](docs/deployment.md): building the image, external services, the public entry point, upgrades and rollbacks, runtime constraints and backups
- [Administration](docs/admin.md): what administration covers, setup and the admin API reference
- [Command line API](docs/api.md): device authorization, tokens and the `/api/v1/` reference

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
cmd/plainmote-cli/   the plainmote command line
internal/app/        service assembly and HTTP lifecycle
internal/auth/       GitHub OAuth
internal/blob/       S3-compatible object storage
internal/config/     environment parsing and validation
internal/linediff/   line diff for version pages and the command line
internal/store/      PostgreSQL schema and business rules
internal/upstream/   remote URL validation and fetching
internal/web/        routes, pages, static assets and public delivery
tools/               editor bundle and encryption tests
docs/                deployment, administration and API documentation, and images
```

## License

Copyright (C) 2026 xwvike

Released under the [GNU Affero General Public License v3.0](LICENSE). Under this license, anyone who modifies the project and offers it to others over a network must make the modified source code available to those users.

Licenses of third-party components (CodeMirror and others, MIT; jschardet, LGPL-2.1) are listed in [`tools/codemirror/NOTICE.md`](tools/codemirror/NOTICE.md).
