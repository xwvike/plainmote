<p align="center">
  <img src="docs/logo.png" alt="PlainMote" width="160" height="160">
</p>

# PlainMote

English | [简体中文](README.zh-CN.md)

PlainMote keeps and shares configuration files, logs, notes and other files. Every save keeps a version; every recipient or machine gets a share link of its own, with its own expiry and use limit, revocable on its own, and every access through it is recorded. Besides the web, it works from a terminal, an API or an AI app.

Reference deployment: [plainmote.link](https://plainmote.link)

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/screenshots/quick-share-dark.png">
  <img src="docs/screenshots/quick-share-light.png" alt="The quick share box with a pasted log, its times, levels and key=value pairs highlighted, and the lifetime scale from 10 minutes to 30 days">
</picture>

## Features

- **Quick share**: no account needed. Paste text or drop a file on the home page and get a link that lasts from 10 minutes to 30 days; curl works too. A quick share made while signed in appears under My resources and can be kept as an ordinary resource before it expires.
- **Share links**: one resource can have many links, each with its own expiry and use limit, each revocable on its own. Updating the content keeps every address. Links answer with `ETag` and `304`, which suits machines polling for a config; opened in a browser, images, audio and video are shown in place.
- **Version history**: each change to the content keeps the version it replaced (up to 10 per resource, for 30 days), to compare, restore or save as a new resource. A save made against an outdated version is refused as a conflict instead of overwriting.
- **Access history**: every access through a link is recorded with its time, result, source IP and client, visible only to the owner and kept for 30 days by default.
- **Tags**: label resources with text tags such as `tmp` or `surge`, and filter the list by them.
- **Editor**: CodeMirror 6 with highlighting for common config formats and logs; it recognises UTF-8, GB18030, Shift_JIS and other encodings and saves in the file's own encoding and line endings.
- **End-to-end encryption (optional)**: with a master password, a resource or a quick share can be encrypted in the browser before upload; the service keeps only ciphertext.
- **Command line and MCP**: the `plainmote` command line views, edits and uploads resources; AI apps such as Claude, ChatGPT and Cursor read and write them through `/mcp` once authorized.
- **Interface**: English, Simplified Chinese, Traditional Chinese, Japanese, French and German, in light and dark themes.

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
curl -fsSL https://plainmote.link/cli/en | sh        # macOS, Linux
irm https://plainmote.link/cli/en.ps1 | iex          # Windows PowerShell
```

The install script verifies the build's SHA-256 and installs it for the current user, without administrator rights; after the service is upgraded, run it again to update. `/cli/en` installs a command line that speaks English, `/cli/zh` one that speaks Chinese.

```bash
plainmote login                               # confirm the sign-in in a browser
plainmote ls nginx
plainmote edit nginx.conf                     # opens your editor; closing it saves a new version
plainmote cat nginx.conf | grep listen
tail -n 200 app.log | plainmote push - --name app.log
plainmote push ./nginx.conf --to nginx.conf
plainmote push ./note.md --tag tmp            # create a resource with a tag
plainmote ls --tag tmp
plainmote share ./app.log --ttl 1d            # make a quick share; prints only its link
plainmote push ./prod.env --encrypt           # create an end-to-end encrypted resource
```

- A resource is named by its ID, an ID prefix of at least six characters, its name or its filename; when several match, the candidates are listed.
- If a resource is saved elsewhere while you edit it, the command line shows the difference and lets you save over it, edit again on the newer version or keep your file.
- Encrypted resources ask for the master password, or read it from `PLAINMOTE_MASTER_PASSWORD`.
- `plainmote help` lists every command and setting. The API it uses is described in [Command line API](docs/api.md).

### Sharing with curl

```bash
tail -n 200 app.log | curl -F 'content=<-' https://plainmote.link/paste
curl --data-binary @app.log 'https://plainmote.link/paste?ttl=1d&filename=app.log'
curl https://plainmote.link/paste    # prints usage
```

`ttl` is `10m`, `1h`, `1d`, `7d` or `30d`. The response body is the share link on one line, ready for a pipe or `$(...)`.

## Security and privacy

- **Links are credentials**: tokens are 256-bit random values; the database keeps only their digest and an encrypted copy. Share addresses are kept out of search engines and cannot be embedded by other sites.
- **No web hosting**: types a browser may execute, such as HTML, scripts and SVG, are never delivered as themselves; pages run only the script files the site serves.
- **Tokens and authorizations**: command line tokens and MCP authorizations are stored only as digests, cannot delete resources, and can be revoked at any time under Authorized apps in the settings.
- **End-to-end encryption**: WebCrypto in the browser (the Go standard library in the command line), AES-256-GCM; the master password never leaves the device. Decryption needs JavaScript; content is lost if both the master password and the recovery key are; size and access history are not encrypted. A four-character code only keeps out someone who merely sees the link: share sensitive content with the full link and its key. Design and threat model: [End-to-end encryption](docs/encryption.md).

## Self-hosting

PlainMote is a single stateless Go service. It needs PostgreSQL, S3-compatible object storage (such as Cloudflare R2 or RustFS), and GitHub or Google OAuth for sign-in. The image `ghcr.io/xwvike/plainmote` is built for amd64 and arm64, and the server needs only `compose.yaml` and `.env`:

```bash
cp .env.example .env    # fill in the image version, database, object storage, OAuth and keys
docker compose up -d
```

Environment variables, external services, the public entry point, upgrades and backups are in the [deployment guide](docs/deployment.md).

- **Administration**: there are no admin pages in the service, only an admin API protected by Ed25519 signatures; [PlainMote Admin](https://github.com/xwvike/plainmote-admin) is an admin page built on it. See the [administration guide](docs/admin.md).
- **Legal pages**: with `PLAINMOTE_CONTACT_EMAIL` set, the service serves an about page, a privacy policy, terms of service and a contact page. They describe the reference deployment: **review and adapt them to your law and your own operation** (`internal/web/templates/legal_*.html`).

## Documentation

- [Deployment](docs/deployment.md): the image, external services, environment variables, the public entry point, upgrades and backups
- [Administration](docs/admin.md): what administration covers, setup and the admin API
- [Command line API](docs/api.md): device authorization, tokens, the `/api/v1/` reference and fetching share links
- [MCP server](docs/mcp.md): connecting AI apps, authorization, tools and limits
- [End-to-end encryption](docs/encryption.md): keys, formats, flows and the threat model

## Development

```bash
docker compose -f compose.dev.yaml up --build
```

The application is at `http://localhost:8964` and the RustFS console at `http://localhost:9001/rustfs/console/` (user `plainmote`, password `plainmote-secret`). Sign-in needs the GitHub or Google OAuth settings in `.env`.

Running the tests:

```bash
docker compose -f compose.dev.yaml up -d postgres
PLAINMOTE_TEST_DATABASE_URL='postgres://plainmote:plainmote@127.0.0.1:54329/plainmote?sslmode=disable' go test ./...
go vet ./...
node tools/e2ee/e2ee_test.mjs
npm --prefix tools/codemirror test
```

With Node.js installed, `go test` also runs the browser's encryption, comparison and export modules and checks that the command line and the browser read each other's encrypted content; without it those tests are skipped. The editor's dependencies are committed as a bundle and never loaded from a CDN; rebuild it with `tools/codemirror/build.sh`. Regular builds do not need Node.js.

```text
cmd/plainmote/       service entry point
cmd/plainmote-admin/ admin key generation and request signing
cmd/plainmote-cli/   the plainmote command line
internal/app/        service assembly and HTTP lifecycle
internal/auth/       GitHub OAuth and Google OpenID Connect
internal/blob/       S3-compatible object storage
internal/config/     environment parsing and validation
internal/linediff/   line diff for version pages and the command line
internal/store/      PostgreSQL schema and business rules
internal/upstream/   fetching MCP client metadata documents from public addresses
internal/web/        routes, pages, static assets and public delivery
tools/               editor bundle and encryption tests
docs/                deployment, administration and API documentation, and images
```

## License

Copyright (C) 2026 xwvike

Released under the [GNU Affero General Public License v3.0](LICENSE): anyone who modifies the project and offers it to others over a network must make the modified source available to those users.

Licenses of third-party components (CodeMirror and others, MIT; jschardet, LGPL-2.1) are listed in [`tools/codemirror/NOTICE.md`](tools/codemirror/NOTICE.md).
