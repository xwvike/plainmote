# Deploying PlainMote

English | [简体中文](deployment.zh-CN.md)

The container runs only the PlainMote service. PostgreSQL, S3-compatible object storage, TLS and the public entry point live outside it; the container holds no data worth keeping and reads no configuration file.

## 1. Getting the image

Every release is published to GitHub Container Registry as a public image for `linux/amd64` and `linux/arm64`:

| Tag | Points at |
| --- | --- |
| `ghcr.io/xwvike/plainmote:v1.2.3` | A release |
| `ghcr.io/xwvike/plainmote:<commit>` | The commit it was built from, by its first 12 characters |
| `ghcr.io/xwvike/plainmote:latest` | The newest release, for trying it out only |

Deploy a release, a commit or a digest, never `latest`: otherwise the running code cannot be identified and a rollback cannot be relied on. A digest is stricter than a tag: a tag can be moved, a digest always names the same content. Look it up:

```bash
docker buildx imagetools inspect ghcr.io/xwvike/plainmote:v1.2.3
```

and pin it in the server's `.env`:

```dotenv
PLAINMOTE_IMAGE=ghcr.io/xwvike/plainmote@sha256:DIGEST
```

Images are built by the repository's [release workflow](../.github/workflows/release.yml) in GitHub Actions and carry a signed build provenance attestation. To check that an image was built by that workflow:

```bash
gh attestation verify oci://ghcr.io/xwvike/plainmote@sha256:DIGEST --owner xwvike
```

The image's OCI labels record the source repository, version and full commit hash:

```bash
docker image inspect ghcr.io/xwvike/plainmote:v1.2.3 --format '{{json .Config.Labels}}'
```

The image's `/cli` directory also holds the `plainmote` command line for macOS, Linux and Windows on amd64 and arm64, about 35 MiB, which the service offers at `/cli` and `/developers/cli`. The command line and the service always carry the same version.

### Publishing a release

Tag a pushed commit on `main` and push the tag:

```bash
git tag v1.2.3
git push origin v1.2.3
```

The workflow publishes `v1.2.3` and the commit tag, and, for a version without a suffix such as `-rc.1`, `latest`; the run's summary shows the digest for `.env`. Run by hand, it publishes the commit tag alone. It uses only the repository's own `GITHUB_TOKEN` and holds no credentials for any server. After the first publication, check in the package's settings on GitHub that it is public; a private package needs every server to sign in before pulling (section 3).

### Building the image yourself

To build from the repository root for the current machine's architecture, for example a modified version:

```bash
test -z "$(git status --porcelain)" || { echo 'working tree is not clean'; exit 1; }
VERSION="$(git rev-parse --short=12 HEAD)"
docker buildx build \
  --load \
  --build-arg VERSION="$VERSION" \
  --build-arg REVISION="$(git rev-parse HEAD)" \
  --tag "plainmote:$VERSION" \
  .
```

- For an x86 server, built on another machine: add `--platform linux/amd64`.
- To push both architectures to a registry of your own: replace `--load` with `--push --platform linux/amd64,linux/arm64` and tag with the registry's address.
- Without a registry: export the image, load it on the server and set `PLAINMOTE_IMAGE` to `plainmote:VERSION`.

```bash
docker save "plainmote:$VERSION" | gzip > "plainmote-$VERSION.tar.gz"
gzip -dc "plainmote-$VERSION.tar.gz" | docker load    # on the server
```

A deployment of modified code must point `PLAINMOTE_SOURCE_URL` at its own source, as AGPL-3.0 requires. If it is published to GHCR, also change the `org.opencontainers.image.source` label in the `Dockerfile`, which GHCR uses to link the package to a repository.

## 2. Preparing external services

### PostgreSQL

Create a dedicated database and account, with TLS parameters in the connection URI:

```text
postgres://plainmote:PASSWORD@postgres.example.com:5432/plainmote?sslmode=require
```

Characters such as `@`, `:` and `/` in the password must be URL-encoded. The service applies its embedded `schema.sql` at startup, so the account needs permission to connect, create tables and indexes, and read and write. The project is evolving quickly and does not migrate old schemas automatically; check the schema changes and back up the database before upgrading.

### S3 or Cloudflare R2

Create the bucket in advance and let the service's credentials read, write and delete objects in it. The Cloudflare R2 endpoint is `https://ACCOUNT_ID.r2.cloudflarestorage.com`, with region `auto`. The bucket needs no public access: all content is read and delivered by PlainMote according to the sharing rules.

### GitHub OAuth App

Create a dedicated OAuth App on GitHub:

- Application name: `PlainMote`
- Homepage URL: `PLAINMOTE_PUBLIC_URL`
- Authorization callback URL: `PLAINMOTE_PUBLIC_URL/auth/github/callback`

GitHub's authorization page shows this App's name. A project or organisation account should own it; under a personal account, GitHub may show that account in the authorization details.

### Google OAuth client

Google sign-in is optional, beside or instead of GitHub. In the Google Cloud Console, under Google Auth Platform:

1. **Branding**: application name, support email, home page (`PLAINMOTE_PUBLIC_URL/about`), privacy policy (`/privacy`) and terms of service (`/terms`), and the domain as an authorized domain. The domain must be verified in Search Console under the same Google account. These pages exist only with `PLAINMOTE_CONTACT_EMAIL` set.
2. **Audience**: External. While the app is in testing, only the listed test users can sign in; publish it to production for everyone.
3. **Data access**: only `openid` and `.../auth/userinfo.profile`. PlainMote asks for nothing else, the email address included, and these scopes need no app verification.
4. **Clients**: a Web application client with the redirect URI `PLAINMOTE_PUBLIC_URL/auth/google/callback`. The client secret is shown only once, when it is created.
5. Optionally, submit the branding for verification, after which the consent screen shows the application's name.

An account is identified by its Google account ID (`sub`). Accounts made with GitHub and with Google are separate; their owner can link the other method in the settings under Sign-in methods.

### Registration

`PLAINMOTE_REGISTRATION_MODE` decides whether a GitHub or Google account that has never signed in may create an account; the default is `closed`. Existing accounts sign in either way, so it can be changed at any time with a restart. On a new deployment, set it to `open`, sign in to create your own account, and set it back to `closed` if nobody else should register.

A sign-in method is offered only when both its client ID and secret are set; with one missing, its button is hidden and the startup log says why. With neither method complete, the service does not start.

## 3. Preparing the deployment directory

The server needs only `compose.yaml` and `.env`, not the source code:

```text
/opt/plainmote/
  compose.yaml
  .env
```

Copy [`.env.example`](../.env.example) to `.env`, fill in the required values, and pin the image to a release or digest (section 1).

### Environment variables

Every setting comes from the environment and is checked at startup; the service exits at once if one is missing or invalid.

Required:

| Variable | Purpose |
| --- | --- |
| `DATABASE_URL` | PostgreSQL connection URL |
| `PLAINMOTE_PUBLIC_URL` | Public HTTP(S) origin, used for share links and the OAuth callback |
| `PLAINMOTE_BLOB_ENDPOINT` | S3-compatible endpoint |
| `PLAINMOTE_BLOB_BUCKET` | Bucket name |
| `PLAINMOTE_BLOB_ACCESS_KEY` | Access Key ID |
| `PLAINMOTE_BLOB_SECRET_KEY` | Secret Access Key |
| `PLAINMOTE_TOKEN_KEY` | 32-byte token encryption key (64 hex digits or Base64); see below |

Optional:

| Variable | Default | Purpose |
| --- | --- | --- |
| `PLAINMOTE_LISTEN` | `:8964` | HTTP listen address |
| `PLAINMOTE_BLOB_REGION` | `auto` | S3 region |
| `PLAINMOTE_TRUSTED_PROXIES` | empty | Trusted proxy IPs or CIDRs, comma-separated (section 5) |
| `PLAINMOTE_VISITOR_LOCATION` | empty | `cloudflare` records visitors' approximate location in the access history from Cloudflare's location headers (section 5) |
| `PLAINMOTE_SESSION_TTL` | `720h` | Sign-in session lifetime |
| `PLAINMOTE_MAX_CONTENT_MIB` | `10` | Size limit for one piece of content |
| `PLAINMOTE_LOG_RETENTION` | `720h` | How long access history is kept; `0` keeps it forever |
| `PLAINMOTE_REGISTRATION_MODE` | `closed` | `open` or `closed` to new accounts |
| `GITHUB_CLIENT_ID`, `GITHUB_CLIENT_SECRET` | empty | GitHub OAuth App; at least one of GitHub and Google is needed |
| `GOOGLE_CLIENT_ID`, `GOOGLE_CLIENT_SECRET` | empty | Google OAuth client; only the name and picture are requested, never the email address |
| `PLAINMOTE_ANONYMOUS` | `false` | Offer quick share on the home page and at `/paste` without signing in; each address (an IPv6 /64 counts as one) may make 120 an hour and 256 MiB a day |
| `PLAINMOTE_OPERATOR` | empty | Operator name for the about page, privacy policy and terms |
| `PLAINMOTE_CONTACT_EMAIL` | empty | Contact email; the about, privacy, terms and contact pages exist only when it is set |
| `PLAINMOTE_SOURCE_URL` | this repository | Target of the "Source code" link on every page; a modified deployment must point it at its modified source, as the AGPL requires |
| `PLAINMOTE_ADMIN_KEYS` | empty | Ed25519 public keys allowed to call the admin API, Base64, comma-separated; without them the API does not exist |
| `PLAINMOTE_ADMIN_ORIGINS` | empty | Browser origins of admin pages allowed to call the admin API, comma-separated |
| `PLAINMOTE_CLI_DIR` | `/cli` | Directory of the command line builds offered at `/cli`; the image includes them |

### Keys and pages

Generate the token encryption key:

```bash
openssl rand -hex 32
```

Back `PLAINMOTE_TOKEN_KEY` up for the long term and give every replica the same value. It cannot be rotated at will: if it is lost or replaced, the share tokens already in the database can no longer be decrypted.

The admin API needs `PLAINMOTE_ADMIN_KEYS` and `PLAINMOTE_ADMIN_ORIGINS`; keys and the admin page are described in [Administration](admin.md). The private key does not belong on the server.

A public instance should set `PLAINMOTE_CONTACT_EMAIL` (and optionally `PLAINMOTE_OPERATOR`) to serve the privacy policy, terms of service, about and contact pages. OAuth providers such as Google require these pages on a verified domain for review: `/about` can serve as the application's home page, `/privacy` and `/terms` as its privacy policy and terms of service.

Restrict access to `.env`:

```bash
chmod 600 .env
```

If the image is a private package, sign in to its registry on the server first, for GHCR with a token that has `read:packages`:

```bash
printf '%s' "$GHCR_TOKEN" | docker login ghcr.io -u USER --password-stdin
```

## 4. Starting and checking

Check the configuration as Compose expands it. The command only checks; the expanded output contains secrets, so do not save it to a log or send it to anyone:

```bash
docker compose config --quiet
```

Pull and start:

```bash
docker compose pull
docker compose up -d --remove-orphans
docker compose ps
```

By default the service is published only on the host's `127.0.0.1:8964`. Check it from the host:

```bash
curl --fail --silent --show-error http://127.0.0.1:8964/healthz
docker compose logs --tail=100 plainmote
```

`/healthz` returning `ok` means only that the HTTP process is responding. PostgreSQL was connected and the schema applied at startup; the object storage credentials are proven only by creating, reading and deleting a resource.

## 5. Public entry point

PlainMote serves HTTP and does not terminate TLS. Put it behind Cloudflare with Cloudflare Tunnel, forwarding the public HTTPS origin to `127.0.0.1:8964` and preserving `Host` and `X-Forwarded-Proto`. The service takes the client IP from the `CF-Connecting-IP` header Cloudflare sets, so no other proxy may stand in front of it: a proxy that passes client headers through lets visitors choose the IP that is recorded and rate-limited.

List in `PLAINMOTE_TRUSTED_PROXIES` only the proxies that connect to the service directly, and do not trust a whole range merely because it is private. While it is empty, no forwarding header is read and every source IP in the access history is the proxy's own. With Docker or a tunnel, the address the service sees may be the bridge gateway: after deploying, open a share link; the source IP in the access history is the value to set. Open the link again to confirm the record now shows the real client IP. While the value is wrong, every client behind the proxy counts as that one address for the per-IP rate limits, so one client's failed attempts hold back everyone; when an unlisted peer sends `CF-Connecting-IP` or `X-Forwarded-For`, the service says so once in its log.

In Cloudflare, also:

1. Rate-limit writes to paths beginning with `/paste` (including `/paste/encrypted`). This is required when anonymous sharing is on.
2. Rate-limit `/api/v1/device/`, `/oauth/register`, `/oauth/token` and `/mcp`. The service already limits these per IP (an IPv6 client by its /64), and `/mcp` also per authorization; a limit at the entry point stops the traffic before it reaches the service.
3. Rate-limit `/d/*` if needed, but never enable `Cache Everything`.
4. Keep the `Cache-Control: no-store` the service sends, so that no edge cache can bypass revocations and use limits.
5. Do not also publish the container port to the internet; that would be an entry point around the tunnel and the rate limits.
6. Optionally, turn on the "Add visitor location headers" Managed Transform in the Cloudflare dashboard (Rules → Settings) and set `PLAINMOTE_VISITOR_LOCATION=cloudflare`: the access history then shows the country or region, first-level administrative division and city Cloudflare estimates from the client IP, and the privacy policy says so. The headers are read only from addresses in `PLAINMOTE_TRUSTED_PROXIES`, and the service refuses to start with this setting while that list is empty. Records made before it was turned on have no location.

## 6. Upgrades and rollbacks

Change `PLAINMOTE_IMAGE` in `.env` and run:

```bash
docker compose pull
docker compose up -d --remove-orphans
curl --fail --silent --show-error http://127.0.0.1:8964/healthz
```

Check the startup log, then sign in, read a resource and open a share link. To roll back, set `PLAINMOTE_IMAGE` back to the previous release or digest and run the same commands.

Once a new version is deployed, `/cli` serves the new command line; users upgrade by running the install command again, and existing sign-ins are kept.

The schema carries no backward compatibility guarantee yet. Once the new version has written data the old one cannot read, switching back to the old image is not a complete rollback, so back up PostgreSQL before every release. Content in S3/R2 should be covered by the provider's versioning or backups.

## 7. Runtime constraints

- The container runs as UID/GID `65532` with a read-only root file system; only a temporary `/tmp` is mounted.
- On SIGTERM, the service takes at most 10 seconds to shut down HTTP; Compose allows 15 seconds to exit.
- Each process keeps at most max(4, number of CPUs) PostgreSQL connections; set `pool_max_conns` in `DATABASE_URL` to change it, such as `?pool_max_conns=10`.
- The service provides no edge protection; rules against abuse of anonymous writes and public delivery belong at the entry point. `/d/*`, the sign-in pages and resource pages must not be cached by a CDN.
- Replicas must share PostgreSQL, S3/R2 and the same `PLAINMOTE_TOKEN_KEY`. This state is held only in a single process's memory: the rate-limit counts of every API and of quick shares made without an account, the admin API's authentication failure counts, and the nonces of admin requests already used. With several replicas, rate limits are therefore counted per replica, and a signed admin request could be replayed to another replica within its validity window. If several replicas are needed, add rate limits at the entry point and route `/_admin/` to a single replica.

## 8. Time

Times are stored as `timestamptz`. Set PostgreSQL's `timezone` and `log_timezone` to `UTC` so that logs and manual queries never mix local time zones; the host's time zone does not affect the service. Pages carry times in UTC and the browser shows them in its own time zone and interface language; with JavaScript disabled, they are shown in UTC.

## 9. Disk, logs and backups

PostgreSQL has no production-suitable setting for a maximum database size. A hard quota on the data directory makes writes fail with `ENOSPC` when it runs out, which can interrupt transactions, checkpoints and WAL writes. Monitor these values and alert while there is still room to act:

```sql
SELECT pg_size_pretty(pg_database_size(current_database()));
```

```bash
du -sh /path/to/postgres/data
df -h /
journalctl --disk-usage
```

The logical database size does not include WAL, table bloat, container logs or backups, so the SQL result alone is not enough. For a root partition of about 20 GiB, start from these thresholds and adjust them to the rate of growth:

- Logical database size: warn at 3 GiB, critical at 4 GiB.
- PostgreSQL data directory: warn at 4 GiB, critical at 5 GiB.
- Free space on the root partition: warn below 3 GiB, critical below 1.5 GiB.

Access history is kept for 30 days by default (`PLAINMOTE_LOG_RETENTION`). Ordinary `VACUUM` lets PostgreSQL reuse the space freed by deletions but does not shrink files or return the space to the operating system. Do not run `VACUUM FULL` as routine cleanup: it locks tables and needs extra temporary disk space.

Compose limits each container's logs to 3 files of 10 MiB. The host's journald should also set `SystemMaxUse` and `SystemKeepFree`, otherwise system logs can still fill the root partition.

Run `pg_dump -Fc` at least daily and check each backup with `pg_restore --list`. A backup on the database's own disk covers accidental deletion and logical damage, not the loss of the disk or the VPS; production backups should also be copied, with separate credentials, to another machine or independent object storage.
