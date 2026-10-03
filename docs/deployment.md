# Deploying PlainMote

English | [简体中文](deployment.zh-CN.md)

The production container runs only the PlainMote web service. PostgreSQL, S3-compatible object storage, TLS and the public entry point are provided outside the container; the container holds no data that needs to be kept and reads no configuration file.

## 1. Building the image

Build a local image for the current machine's architecture from the repository root:

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

The build also compiles the `plainmote` command line for macOS, Linux and Windows on amd64 and arm64 into the image's `/cli` directory, from which the service's `/cli` page offers them for download. `VERSION` is stamped into both the service and the command line, so their versions always match. The command line builds add about 35 MiB to the image.

For an ordinary x86 Linux server, an amd64 image can be built explicitly:

```bash
test -z "$(git status --porcelain)" || { echo 'working tree is not clean'; exit 1; }
VERSION="$(git rev-parse --short=12 HEAD)"
docker buildx build \
  --load \
  --platform linux/amd64 \
  --build-arg VERSION="$VERSION" \
  --build-arg REVISION="$(git rev-parse HEAD)" \
  --tag "plainmote:$VERSION" \
  .
```

To publish to GHCR with both amd64 and arm64 images:

```bash
export IMAGE=ghcr.io/OWNER/plainmote    # OWNER: your GitHub user or organisation
export VERSION="$(git rev-parse --short=12 HEAD)"

test -z "$(git status --porcelain)" || { echo 'working tree is not clean'; exit 1; }
printf '%s' "$GHCR_TOKEN" | docker login ghcr.io -u OWNER --password-stdin
docker buildx build \
  --push \
  --platform linux/amd64,linux/arm64 \
  --build-arg VERSION="$VERSION" \
  --build-arg REVISION="$(git rev-parse HEAD)" \
  --tag "$IMAGE:$VERSION" \
  .
```

Add the `latest` tag separately, once the version is known to be deployable. Deployment files should always use a commit tag or an image digest, never `latest`; otherwise the running code cannot be identified and a rollback cannot be relied on.

```bash
docker buildx imagetools create \
  --tag "$IMAGE:latest" \
  "$IMAGE:$VERSION"
```

A digest is stricter than a tag: it keeps pointing at the same content even if the tag is pushed again. Query it after publishing:

```bash
docker buildx imagetools inspect "$IMAGE:$VERSION"
```

To pin the build exactly, use the manifest list digest it prints in the server's `.env`:

```dotenv
PLAINMOTE_IMAGE=ghcr.io/OWNER/plainmote@sha256:DIGEST
```

The GHCR token needs at least `write:packages`; servers deploying a private image need `read:packages`. The image records the source repository, version and full commit hash in OCI labels, which can be checked with:

```bash
docker image inspect "plainmote:$VERSION" \
  --format '{{json .Config.Labels}}'
```

Without a registry, a single-architecture image can be exported and copied to the server:

```bash
docker save "plainmote:$VERSION" | gzip > "plainmote-$VERSION.tar.gz"
```

After loading it on the server, set `PLAINMOTE_IMAGE` in `.env` to `plainmote:VERSION`:

```bash
gzip -dc "plainmote-$VERSION.tar.gz" | docker load
```

## 2. Preparing external services

### PostgreSQL

Create a dedicated database and account, and build a connection URI with TLS parameters:

```text
postgres://plainmote:PASSWORD@postgres.example.com:5432/plainmote?sslmode=require
```

Characters such as `@`, `:` and `/` in the password must be URL-encoded. The service applies its embedded `schema.sql` at startup, so the database account needs permission to connect, create tables and indexes, and read and write. The project is evolving quickly and provides no compatibility layer for migrating old schemas automatically; check the schema changes and back up the database before upgrading.

### S3 or Cloudflare R2

Create the bucket in advance and grant the service's credentials permission to read, write and delete objects in it. The Cloudflare R2 endpoint has the form:

```text
https://ACCOUNT_ID.r2.cloudflarestorage.com
```

R2 uses `PLAINMOTE_BLOB_REGION=auto`. The bucket does not need public access: all content is read and delivered by PlainMote according to the sharing rules.

### GitHub OAuth App

Create a dedicated OAuth App on GitHub:

- Application name: `PlainMote`
- Homepage URL: the production `PLAINMOTE_PUBLIC_URL`
- Authorization callback URL: `PLAINMOTE_PUBLIC_URL/auth/github/callback`

The name shown on the authorization page comes from this OAuth App. It should be owned by a project or organisation account; when it is created under a personal account, GitHub may still show that account's ownership in the authorization details.

## 3. Preparing the deployment directory

The server needs only `compose.yaml` and `.env`, not the source code:

```text
/opt/plainmote/
  compose.yaml
  .env
```

Copy [`.env.example`](../.env.example) to `.env`, fill in every required value, and pin the image to the version or digest just published:

```dotenv
PLAINMOTE_IMAGE=ghcr.io/OWNER/plainmote:COMMIT_TAG
```

Generate the token encryption key:

```bash
openssl rand -hex 32
```

`PLAINMOTE_TOKEN_KEY` must be backed up for the long term and be the same for every replica. It is not a password that can be rotated at will: if it is lost or replaced, the share tokens already in the database can no longer be decrypted.

To enable the admin interface, set `PLAINMOTE_ADMIN_KEYS` and `PLAINMOTE_ADMIN_ORIGINS`; generating keys and using the administration page are described in [Administration](admin.md). The private key does not belong on the server.

A publicly available instance should set `PLAINMOTE_CONTACT_EMAIL` (and optionally `PLAINMOTE_OPERATOR`) to provide the privacy policy, terms of service, about and contact pages. Third-party OAuth providers such as Google require these pages on a verified domain for review; `https://<domain>/about` can serve as the application's home page, and `/privacy` and `/terms` as the privacy policy and terms of service addresses.

Restrict access to `.env`:

```bash
chmod 600 .env
```

If the image is a private package, sign in to GHCR on the server first:

```bash
printf '%s' "$GHCR_TOKEN" | docker login ghcr.io -u OWNER --password-stdin
```

## 4. Starting and checking

Check the configuration as Compose expands it. The command only checks; do not save its expanded output, which contains secrets, to a log or send it to anyone:

```bash
docker compose config --quiet
```

Pull and start the pinned image:

```bash
docker compose pull
docker compose up -d --remove-orphans
docker compose ps
```

By default the service is published only on the host's `127.0.0.1:8964`. Check the process from the host:

```bash
curl --fail --silent --show-error http://127.0.0.1:8964/healthz
docker compose logs --tail=100 plainmote
```

`/healthz` returning `ok` means only that the HTTP process is responding. The service has already connected to PostgreSQL and applied the schema at startup; the S3/R2 credentials should be verified by creating, reading and deleting a test resource as part of acceptance.

## 5. Public entry point

PlainMote serves HTTP and does not terminate TLS. Use Nginx, Caddy or Cloudflare Tunnel to forward the public HTTPS origin to `127.0.0.1:8964`, preserving `Host`, `X-Forwarded-Proto` and the client IP headers.

Only addresses of proxies that connect to the service directly may be listed in `PLAINMOTE_TRUSTED_PROXIES`. Do not trust a whole range merely because it is private. When the variable is empty, no forwarding header is read and every source IP in the access history is the proxy's own address. With Docker or a tunnel, the address the service sees may be the bridge gateway: after deploying, open a share link; the source IP recorded in the access history is the value to set. Open the link again afterwards to confirm the record now shows the real client IP.

With Cloudflare, also:

1. Rate-limit writes to paths beginning with `/paste` (including `/paste/encrypted`). This is required when anonymous sharing is enabled.
2. Rate-limit `/api/v1/device/`. The service already limits code requests and polling per IP; a limit at the entry point stops the traffic before it reaches the service.
3. Rate-limit `/d/*` if needed, but never enable `Cache Everything`.
4. Keep the `Cache-Control: no-store` the service sends, so that revocations and use limits cannot be bypassed by an edge cache.
5. Do not also publish the container port to the internet; that would create an entry point that bypasses the tunnel and the rate limits.

## 6. Upgrades and rollbacks

Build a new commit tag for every release. To upgrade, change `PLAINMOTE_IMAGE` in `.env` and run:

```bash
docker compose pull
docker compose up -d --remove-orphans
curl --fail --silent --show-error http://127.0.0.1:8964/healthz
```

Check the startup log and complete a sign-in, a resource read and a share link access. To roll back, set `PLAINMOTE_IMAGE` back to the previous commit tag and run the same commands.

The command line is upgraded with the image: once a new version is deployed, `/cli` serves the new command line, and users upgrade by running the install command again. Existing sign-ins are not affected.

The database schema currently carries no backward compatibility guarantee. If the new version has written data the old version cannot understand, switching back to the old image is not a complete rollback; a PostgreSQL backup must be kept before every release. Content stored in S3/R2 should also be covered by the provider's versioning or backup policy.

## 7. Runtime constraints

- The container runs as UID/GID `65532` with a read-only root file system; only a temporary `/tmp` is mounted.
- Compose limits container logs to 3 files of 10 MiB each, so Docker's JSON logs cannot fill the host's disk.
- On SIGTERM, the service takes at most 10 seconds to shut down HTTP; Compose allows 15 seconds to exit.
- Replicas must share PostgreSQL, S3/R2 and the same `PLAINMOTE_TOKEN_KEY`.
- The following state is held only in the memory of a single process: the rate-limit counts of the command line API and device authorization, the admin interface's authentication failure counts, and the nonces of admin requests already used. With several replicas, rate limits are counted per replica, and a signed admin request could be replayed to another replica within its validity window. If several replicas are needed, add rate limits at the entry point and route `/_admin/` requests to a single replica.
- The service does not provide edge rate limiting; protection of anonymous writes and public delivery must be configured at the entry point.
- `/d/*`, the sign-in pages and resource pages must not be cached by a CDN.

## 8. Time and time zones

Times are stored in the database as `timestamptz`; the service writes and compares absolute points in time. Setting both `timezone` and `log_timezone` to `UTC` in production PostgreSQL keeps database logs, manual queries and cross-region troubleshooting free of mixed local time zones. The host can keep whatever time zone its operators prefer; it affects only how systemd timers and host logs are displayed.

Pages output `<time datetime="...">` with the point in time in UTC, and the browser formats it in its own time zone and the current interface language. Users in Japan therefore see Japanese time, and no user needs to set a time zone in their account. With JavaScript disabled, pages show UTC explicitly as a fallback. Switching the interface language changes only how dates are formatted, not the point in time or the browser's time zone.

## 9. Disk, logs and backups

PostgreSQL has no production-suitable setting for a maximum database size. A hard file system quota on the data directory makes writes fail with `ENOSPC` when it runs out, which can interrupt transactions, checkpoints and WAL writes. Monitor the following three values and alert while there is still room to act:

```sql
SELECT pg_size_pretty(pg_database_size(current_database()));
```

```bash
du -sh /path/to/postgres/data
df -h /
journalctl --disk-usage
```

The logical database size does not include WAL, table bloat, container logs or backups, so the SQL result alone is not enough. For a root partition of about 20 GiB, the following thresholds are a starting point, to be adjusted to the rate of growth:

- Warn when the logical database size reaches 3 GiB; critical at 4 GiB.
- Warn when the PostgreSQL data directory reaches 4 GiB; critical at 5 GiB.
- Warn when free space on the root partition falls below 3 GiB; critical below 1.5 GiB.

`PLAINMOTE_LOG_RETENTION` keeps access history for 30 days by default. Ordinary `VACUUM` lets PostgreSQL reuse the space freed by deletions but does not immediately shrink files and return the space to the operating system. Do not use periodic `VACUUM FULL` as routine cleanup: it locks tables and needs additional temporary disk space.

Compose limits each container's JSON logs to 3 files of 10 MiB. The host's journald should also set `SystemMaxUse` and `SystemKeepFree`, otherwise system service logs can keep growing on the root partition.

Run `pg_dump -Fc` at least daily and verify that each backup is readable with `pg_restore --list`. A backup on the same disk as the database protects only against accidental deletion or logical damage, not against losing the disk or the VPS; production backups should also be copied, with separate credentials, to another machine or independent object storage.
