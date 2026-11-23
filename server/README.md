# SaveVault 云端（game-save）

> SaveVault 的自建云端（已合并至 SaveVault monorepo 的 `server/` 目录）。主路径为 `/v1/savevault/...` 与 `/savevault/`，同时向后兼容旧版 `/v1/opensave/...`、`/opensave/`、旧环境变量及旧磁盘数据自动迁移。

The cross-component delivery contract and release gates are in
[`docs/INTEGRATION_ROADMAP.md`](docs/INTEGRATION_ROADMAP.md).

`game-save` is a self-hosted private backend for game-save synchronization. It
is the standalone successor to `eden-syncd`: the existing Eden protocol remains
available, while SaveVault uses a separate, typed archive API. The service never
interprets game-save contents.

The current release is `v1.8.0`. It is **not** published to any container
registry: build it yourself, the compose files do that with `build: .` and tag
the result `game-save:local`. Upgrade notes and the rollback checklist are in
[`docs/RELEASE_v1.8.0.md`](docs/RELEASE_v1.8.0.md).

## Client Protocols

### Eden

Eden continues to use its existing authenticated `/v1` object and snapshot-head
protocol. Existing Eden client URLs and sync keys do not need to change when an
`eden-syncd` container is replaced by `game-save`.

`GET /v1/scopes/{scope}/status` is a read-only status endpoint for newer Eden
clients. It returns the CAS-authoritative head, every recoverable version,
retained conflict copies, source device identity, and server receive times. A
new scope returns an empty state with HTTP 200. The request neither records a
device heartbeat nor changes save metadata; client-side state inspection must
not trigger an upload, download, or restore.

### SaveVault Native API

The matching SaveVault client integration uses the same private service URL and
sync key, without a WebDAV URL or Basic Auth credentials. Archives use a
mandatory namespace:

- `pc`
- `switch`
- `emulator-<name>` for future emulator types

The native endpoints are:

```text
GET    /v1/savevault/files
PUT    /v1/savevault/files/{type}/{name}
GET    /v1/savevault/files/{type}/{name}
DELETE /v1/savevault/files/{type}/{name}
GET    /v1/savevault/scopes/{type}/{gameId}
GET    /v1/savevault/scopes/{type}/{gameId}/head
PUT    /v1/savevault/scopes/{type}/{gameId}/snapshots/{snapshotId}
PUT    /v1/savevault/scopes/{type}/{gameId}/conflicts/{snapshotId}
GET    /v1/savevault/games
GET    /v1/savevault/games/{key}
GET    /v1/savevault/covers/{sha256}
GET    /v1/savevault/titles/switch
PUT    /v1/savevault/scopes/{type}/{gameId}/custom-name
PUT    /v1/savevault/scopes/{type}/{gameId}/custom-cover
DELETE /v1/savevault/scopes/{type}/{gameId}/custom-cover
```

Every game in `GET /v1/savevault/games` carries a `meta` object with the
resolved display name (`custom` > embedded Switch title table > the most
recent client-reported identity name > the key's ID part) and an optional
custom cover. Custom names and covers are written by scope and stored by game
key, so all devices of one game see the same values. Cover bytes are uploaded
as the request body (`image/jpeg` or `image/png`, at most 1 MiB and 2048
pixels on the longest side) and downloaded from the content-addressed
`/v1/savevault/covers/{sha256}` route. Writes accept `If-Match: "<rev>"` for
optimistic concurrency; a stale revision returns `412` with the current
`meta`. The embedded Switch title table is also downloadable as gzip and
supports `If-None-Match`.

The same metadata can be managed from `/admin/api/savevault/games/{key}/custom-name`
and `/admin/api/savevault/games/{key}/custom-cover`; administrator cover URLs
use `/admin/api/savevault/covers/{sha256}`.

They require `Authorization: Bearer <GAME_SAVE_TOKEN>`. Native archives live at
`savevault-native/<type>/` and cannot collide with Eden encrypted objects or the
legacy WebDAV collection. The first upload creates an immutable archive; a
retry with the same bytes is idempotent, while different bytes under the same
name return `409 Conflict`. A client advances a typed `{type}/{gameId}` cloud
head only through the snapshot endpoint with an `If-Match` header for its
confirmed parent. A rejected branch is sent to the conflict endpoint and
remains recoverable without changing the head. A matching SaveVault client build
is required before selecting this provider.

Native SaveVault `GET` operations are authenticated but read-only: listing
archives or checking a typed scope/head never writes a device heartbeat.

#### Save management (C6)

Administrators can delete, restore, merge, export, and lock SaveVault scopes
from the dashboard API:

```text
GET    /admin/api/savevault/tombstones
DELETE /admin/api/savevault/tombstones/{type}/{gameId}
DELETE /admin/api/savevault/scopes/{type}/{gameId}          (If-Match required)
POST   /admin/api/savevault/scopes/{type}/{gameId}/restore
POST   /admin/api/savevault/scopes/{type}/{gameId}/retire   {"into": "<gameId>"}
POST   /admin/api/savevault/scopes/{type}/{gameId}/unretire
GET    /admin/api/savevault/scopes/{type}/{gameId}/export
PUT    /admin/api/savevault/scopes/{type}/{gameId}/snapshots/{snapshotId}/lock
DELETE /admin/api/savevault/scopes/{type}/{gameId}/snapshots/{snapshotId}/lock
GET    /admin/api/savevault/retention
PUT    /admin/api/savevault/retention
POST   /admin/api/savevault/retention/preview
```

A deleted or merged scope is kept as a tombstone in `savevault-manage.json`.
Client scope reads and writes, archive uploads, identity writes, and custom
name/cover writes for that scope return `410 Gone` with a machine-readable
`code`:

- `scope_deleted` includes `deletedAt`; the scope is recoverable for seven
  days from `savevault-trash/` and then maintenance removes the archived files.
- `scope_retired` includes `successor: {"type": "...", "gameId": "..."}`; the
  merged scope stays read-only and its archives remain downloadable.

A 410 is deliberately different from a 404: existing clients treat a scope
`GET` 404 as "this server does not support scopes" and fall back to older
behavior, while a 410 stops the upload before an orphan archive is created.
Locks are never removed by automatic retention or the manual prune endpoint.
The dashboard exposes the same operations: scope pages can export, merge, or
delete a scope and lock individual versions, while **维护** manages automatic
retention, orphan archives, and recently deleted scopes.

### Device Tokens

v1.7.0 adds per-device credentials alongside the shared `GAME_SAVE_TOKEN`.
Each client can claim a one-time pairing code from the dashboard or receive a
token generated on the web page, then send
`Authorization: Bearer gsdt_<id>_<secret>`. Tokens bind to the first valid
device ID they report, can be renamed or revoked immediately, and are stored in
`device-tokens.json` as SHA-256 digests only. The shared token keeps working
during a compatibility period controlled by `shared_token_mode`
(`allow`/`warn`/`deny`); `warn` also adds an
`X-Game-Save-Auth-Warning: shared-token-deprecated` response header. Pairing
codes, management endpoints, error codes, and migration steps are documented in
[`docs/C3_DEVICE_TOKENS.md`](docs/C3_DEVICE_TOKENS.md).

The dashboard's **令牌** page is the web entry point: it lists active and
revoked tokens, creates tokens, pairs devices with a code or QR code, and
switches the shared-token compatibility period. Pairing QR codes are rendered
locally with qrcode-generator 2.0.4 (`web/vendor/qrcode-generator.js`); its MIT
license is at [`web/vendor/LICENSE-qrcode-generator.txt`](web/vendor/LICENSE-qrcode-generator.txt).

### Users, Login And Tenants

Every tenant owns a vault: its own directory under `<data-dir>/vaults/<vault id>/`,
its own tokens, its own Eden scopes, its own SaveVault archives. `user_name` and
`descriptor` inside a scope are client-side display labels, never authorization.

A pre-vault data directory is migrated into `vaults/default/` on the first start,
so an existing single-tenant deployment keeps working with its old token.

`POST /v1/login` is the only unauthenticated data endpoint. It trades a user name
and password for the tenant's bearer token:

```bash
curl -s http://127.0.0.1:8788/v1/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"alice","password":"…"}'
# {"token":"…","vault":"alice","username":"alice"}
```

Passwords are stored as PBKDF2-HMAC-SHA256 with 210,000 rounds and a random salt.
Disabled and expired accounts fail exactly like a wrong password. Deleting a user
revokes the token and drops the vault from memory but keeps its data on disk.

Administrators manage tenants with the admin credential plus the
`X-Game-Save-Admin: 1` header:

| Request | Meaning |
| --- | --- |
| `GET /admin/api/users` | list tenants (name, vault, status, quota, last login) |
| `POST /admin/api/users` | create `{username, password, note?, quotaBytes?, expiresAt?}` |
| `POST /admin/api/users/{username}/password` | reset a password |
| `DELETE /admin/api/users/{username}` | revoke the tenant |

### SaveVault WebDAV Compatibility

Older SaveVault builds can continue to use `https://host:port/savevault/` with
Basic Auth. Set both `GAME_SAVE_SAVEVAULT_USERNAME` and
`GAME_SAVE_SAVEVAULT_PASSWORD` to enable it. WebDAV files remain in the separate
`savevault/` directory and do not share native SaveVault storage.

## Compatibility With `eden-syncd`

`game-save` is an in-place protocol-compatible replacement:

- New environment variables use `GAME_SAVE_*`.
- The corresponding `EDEN_SYNCD_*` variables remain accepted as a temporary
  fallback, but new deployments should use the new names.
- The old `X-Eden-Admin` dashboard write header is still accepted; new clients
  send `X-Game-Save-Admin`.
- Existing state files, encrypted blobs, dashboard metadata, and WebDAV files
  are read without a data migration.

The persistent data directory contains `objects/`, `state.json`,
`dashboard.json`, `settings.json`, `events.json`, `savevault/`, and (after a
native SaveVault upload) `savevault-native/`, `savevault-native.json`, plus
`savevault-native-state.json`. After the first custom name or cover write it
also contains `savevault-meta.json` and `savevault-meta/`; custom cover files
have no other copy, so back up the entire directory together. After the first
C6 operation it can also contain `savevault-manage.json` (tombstones, locks,
retention settings) and `savevault-trash/` (recoverable deleted scopes); back
up both together with the rest of the directory. The JSON catalog and state
contain audit metadata, content digests, source devices, parent lineage,
current heads, and retained conflicts; they never contain save contents.

## Configuration

| Flag | Environment variable | Default |
| --- | --- | --- |
| `--token` | `GAME_SAVE_TOKEN` | required |
| `--admin-token` | `GAME_SAVE_ADMIN_TOKEN` | dashboard disabled |
| `--data-dir` | `GAME_SAVE_DATA` | `./data` (`/var/lib/game-save` in Docker) |
| `--listen` | `GAME_SAVE_LISTEN` | `:8787` |
| `--savevault-username` | `GAME_SAVE_SAVEVAULT_USERNAME` | WebDAV disabled unless paired with password |
| `--savevault-password` | `GAME_SAVE_SAVEVAULT_PASSWORD` | WebDAV disabled unless paired with username |
| `--max-object-mib` | `GAME_SAVE_MAX_OBJECT_MIB` | `512` |
| `--keep-snapshots` | `GAME_SAVE_KEEP_SNAPSHOTS` | `0` (unlimited) |
| `--keep-days` | `GAME_SAVE_KEEP_DAYS` | `0` (unlimited) |
| `--maintenance-interval` | `GAME_SAVE_MAINTENANCE_INTERVAL` | `6h` |
| `--bark-url` | `GAME_SAVE_BARK_URL` | disabled |
| `--bark-group` | `GAME_SAVE_BARK_GROUP` | `game-save` |
| `--bark-ttl` | `GAME_SAVE_BARK_TTL` | `600` seconds |
| `--bark-events` | `GAME_SAVE_BARK_EVENTS` | `conflict` |
| `--bark-timeout` | `GAME_SAVE_BARK_TIMEOUT` | `5s` |
| `--log-format` | `GAME_SAVE_LOG_FORMAT` | `text` or `json` |

`game-save healthcheck` probes `GET /healthz`. `game-save --version` reports
the build version.

## Run Locally

```sh
export GAME_SAVE_TOKEN='replace-with-a-long-random-sync-token'
export GAME_SAVE_ADMIN_TOKEN='replace-with-a-different-admin-token'
go run . --data-dir ./data --listen 127.0.0.1:8787
```

The health endpoint is unauthenticated. All `/v1` endpoints require the sync
token. Set an administrator token different from the sync token to enable the
private dashboard at `/admin/`.

Eden save writes require Save Sync protocol v2. Upgrade every Eden client
before deploying the `game-save` release that enables this requirement: older
clients can still read, but cannot upload a stale local save as a new cloud
head. SaveVault's separate native and WebDAV endpoints are unaffected.

When current clients detect divergent local files, they preserve the candidate
as a fixed **conflict copy** instead of advancing the cloud head. It appears in
the dashboard's snapshot history and can be inspected, downloaded, or made
current with the existing restore action. Conflict copies are never removed by
the retention policy; delete one explicitly after reviewing it.

## Preventing Save Rollback

`game-save` does not use wall-clock time as the authority for a save. A client
may only advance a cloud head when its local files match the content digest of
the head it previously confirmed. A stale local directory, manual replacement,
or interrupted session is retained as a conflict copy instead. During the v2
client migration, an existing local directory must first match a read-only
download of the cloud head before it becomes trusted.

For reliable cross-device play:

1. Upgrade every Eden client to Save Sync protocol v2 before upgrading the
   server to `v1.1.0` or later.
2. Play one title on only one device at a time. Stop emulation normally and
   keep that device online until it returns to the game library before opening
   the same title elsewhere.
3. After a forced process kill, power loss, or network outage, reopen Eden on
   the device that was playing first. It retries the locally persisted snapshot
   before continuing that title on another device.
4. If a conflict copy appears, stop both sessions, compare the actual game
   state, and use the dashboard's restore action once. The matching device
   recognizes the selected cloud content and resumes ordinary synchronization.

## Bark Notifications

Set `GAME_SAVE_BARK_URL` to the Bark base URL including its device path. A
complete URL copied from a Bark `curl` example is also accepted: its final
`/title/body` segments and query string are removed automatically. For example,
`https://bark.example.com/device-key/title/body` becomes
`https://bark.example.com/device-key`.

`game-save` calls Bark asynchronously after its local activity event is
persisted. A Bark outage never blocks an upload, conflict response, dashboard
action, or maintenance task. The default only notifies `conflict`, which is the
actionable condition where a device attempted to advance a stale cloud head and
its local snapshot was retained. Set `GAME_SAVE_BARK_EVENTS=conflict,upload`
to also receive successful uploads. Duplicate events for the same game, device,
and cloud head are coalesced for five minutes.

## Docker And NAS Deployment

Copy [`.env.example`](.env.example) to `.env`, replace placeholders, then use a
fixed release tag rather than `latest`:

```sh
docker compose -f compose.nas.yaml up -d
```

`compose.nas.yaml` builds the image locally (`build: .`, tagged
`game-save:local`; override with `GAME_SAVE_IMAGE` to reuse one built
elsewhere), maps the service to port `8788`, and stores data at
`/volume1/docker/game-save/data`. Set `PUID` and `PGID` to the owner of that
directory. The generic default binds to loopback;
choose the NAS private address only when the LAN firewall protects the port.

For Synology Container Manager, use
[`compose.synology.yaml`](compose.synology.yaml). It avoids Compose variable
interpolation that some DSM releases reject. Change its host path and `user`
field for the NAS before deployment.

## Safe Migration From `eden-syncd`

1. Back up the old data directory while the old container is still healthy.
2. Stop the old `eden-syncd` container, but do not remove it or its data.
3. Copy the old data directory to the new `game-save` project directory.
4. Create a new `game-save` container with the same host port and mount the new
   copy at `/var/lib/game-save`.
5. Verify `GET /healthz`, an existing authenticated Eden `/v1` request, and the
   SaveVault route before treating the migration as complete.

The old container can remain stopped as a rollback target. Do not run old and
new containers against the same data directory at the same time.

## Dashboard

Set `GAME_SAVE_ADMIN_TOKEN` to enable `/admin/`. The dashboard uses an HttpOnly,
SameSite=Strict session cookie. State-changing dashboard API requests require
`X-Game-Save-Admin: 1`. Keep the dashboard on a private network and use HTTPS
for any remote access.

## Development

```sh
go test ./...
go vet ./...
docker build --build-arg VERSION=v1.8.0 -t game-save:v1.8.0 .
```

The GitHub Actions release workflow publishes multi-architecture Docker Hub
images only for `v*` Git tags or an explicit manual dispatch.
