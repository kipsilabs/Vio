# Silo to Vio Migration (Phase 1)

This guide is for admins who already run a Docker Compose based Silo install
and want to switch that host to Vio Server without rebuilding their library
from scratch.

Vio is based on Silo (© Silo Media L.L.C., AGPL-3.0-or-later). Silo is a
trademark of Silo Media L.L.C.; Vio is an unofficial fork, not affiliated
with or endorsed by Silo Media.

Phase 1 is a **rebrand cutover at the deployment layer**: new product name,
new image, new paths, and a new `VIO_` env prefix. The application contract
(API routes, database schema, settings keys) is unchanged — see
[What Phase 1 does NOT change](#what-phase-1-does-not-change).

## Best practice

Treat the rename as a controlled cutover, not an in-place edit while the old
app is running:

1. Take a host snapshot or a PostgreSQL dump.
2. Copy your working Silo `.env` to the Vio checkout and translate the
   variables per the table below.
3. Fix `.env` until every path and credential matches the old deployment.
4. Run the cutover during a quiet window.
5. Verify health, readiness, media paths, users, and plugins before deleting
   any old compatibility symlink or backup.

## Phase 1 cutover table

| Area | Silo (old) | Vio (new) | Notes |
| --- | --- | --- | --- |
| Server data root | `/opt/silo` (`SILO_DATA_ROOT`) | `/opt/vio` (`VIO_DATA_ROOT`) | Move the data root (postgres, redis, meilisearch, plugins, compat, transcode) or bind-mount the old path; leave a compatibility symlink from the old path until verified. |
| In-container state | `/var/lib/silo/...` | `/var/lib/vio/...` | Covers `plugins`, `compat`, and the Jellyfin web install dir. Update bind mounts together with the data root. |
| Transcode scratch | `/tmp/silo-transcode` | `/tmp/vio-transcode` | Default when no override is set. Custom transcode dirs keep working unchanged. |
| Download artifacts | `silo-download-artifacts` (sibling of the transcode dir) | `vio-download-artifacts` | Same sibling-dir rule, new leaf name. Custom artifact dirs keep working unchanged. |
| Env prefix | `SILO_*` | `VIO_*` | `SILO_DATA_ROOT` → `VIO_DATA_ROOT`, `SILO_IMAGE` → `VIO_IMAGE`, `SILO_PLUGIN_CACHE_DIR` → `VIO_PLUGIN_CACHE_DIR`, etc. Legacy `SILO_*` values are still accepted as fallback when the `VIO_*` equivalent is unset — set the `VIO_*` form going forward. |
| Compose service | `silo` | `vio` | Update `docker compose logs`, `exec`, and health-check references. |
| Container image | `ghcr.io/silo-server/silo-server:latest` (`SILO_IMAGE`) | `ghcr.io/kipsilabs/vio:latest` (`VIO_IMAGE`) | Override `VIO_IMAGE` to pin a specific tag. |
| PostgreSQL defaults | `POSTGRES_USER=silo`, `POSTGRES_DB=silo` | `POSTGRES_USER=vio`, `POSTGRES_DB=vio` for **new** installs | **Migrating installs must keep the old values.** If the old database used `silo` (or any custom) credentials, keep those exact values. Do not switch to the Vio defaults unless you are starting with a fresh database. |
| Meilisearch index | `silo_media_items` (`catalog.search.meilisearch.index`) | unchanged in Phase 1 | Keep the existing index name and let the existing index keep serving. Do not rename or rebuild indexes as part of the cutover. |
| Meilisearch embedder | `silo_recommendations` | unchanged in Phase 1 | Keep the existing embedder name. |
| HTTP headers | `X-Silo-*` | unchanged in Phase 1 | `X-Silo-*` request/response headers continue to work and are accepted as the canonical form. A future `X-Vio-*` alias may be added; nothing to change now. |

## Prepare `.env`

From a Vio checkout:

```sh
cp .env.example .env
```

Translate your Silo values:

```dotenv
MEDIA_ROOT=/host/path/to/media
MEDIA_CONTAINER_ROOT=/old/container/path/to/media
VIO_DATA_ROOT=/opt/vio
VIO_IMAGE=ghcr.io/kipsilabs/vio:latest
POSTGRES_USER=silo
POSTGRES_PASSWORD=<existing password, unchanged>
POSTGRES_DB=silo
```

`MEDIA_CONTAINER_ROOT` matters for existing installs. New Vio installs can use
`/mnt/media`, but migrated libraries may already store absolute paths from the
old container. Preserve the old in-container path here if existing library
paths use it.

If your old database used different PostgreSQL credentials, keep those exact
values (see the table above).

Legacy `SILO_*` variables (e.g. `SILO_DATA_ROOT`, `SILO_IMAGE`) are still read
as fallback when the corresponding `VIO_*` variable is unset, so an
untranslated `.env` keeps starting. Prefer the `VIO_*` forms — the fallback
exists to make the cutover forgiving, not as a permanent configuration style.

## Cut over

1. Stop the Silo compose stack.
2. Move the data root (`/opt/silo` → `/opt/vio`), or re-point the bind mounts
   at the same underlying directories.
3. Leave a compatibility symlink from the old data root to the new one until
   the migration is verified.
4. Start the Vio compose stack.
5. Update plugin cache paths from `/var/lib/silo/plugins` to
   `/var/lib/vio/plugins` wherever they appear outside the compose file
   (scripts, external mounts).

Old Silo plugin binaries that break against the Vio plugin runtime should be
reinstalled as Vio-native plugin packages from the admin UI after the server
is healthy.

## Verify

```sh
docker compose ps
curl -fsS http://localhost:8090/api/v1/health
curl -fsS http://localhost:8090/api/v1/ready
docker compose logs --tail=200 vio
```

`/api/v1/health` and `/api/v1/ready` are retained operational probes. They
keep their paths — Phase 1 does not rename API routes, so existing probe
configuration keeps working.

In the admin UI, verify:

- admin login works
- libraries point at paths visible inside the Vio container
- users and profiles are present
- metadata plugins are installed and provider chains are configured
- playback starts for at least one direct-play item and one transcode/remux
  item

## What Phase 1 does NOT change

Explicitly out of scope for this cutover — do not expect new names here, and
do not rename them by hand:

- **Go module path** — still `github.com/Silo-Server/silo-server`.
- **API routes** — `/api/v1`, `/api/v2`, and compatibility surfaces keep
  their paths.
- **Database columns and tables** — including `silo_*` columns (e.g.
  `silo_user_id`, `silo_profile_id`).
- **Settings keys** — e.g. `catalog.search.meilisearch.index` and its
  `silo_media_items` default.
- **Jellyfin `ProductName`** — still reports `"Jellyfin Server"`.
- **Problem-type URLs** — still rooted at
  `https://siloserver.org/docs/api/v2/problems/`.
- **`silo_*` Prometheus metrics** — e.g. `silo_build_info`,
  `silo_work_*`, `silo_queue_*`, `silo_redis_*`.
- **`X-Silo-*` headers** — still canonical; accepted as-is.

A later phase may introduce `Vio`/`vio_*`/`X-Vio-*` equivalents with
backwards-compatible aliases. Phase 1 deliberately does not.

## Rollback

Prefer restoring the host snapshot. If no snapshot is available, stop Vio,
restore the PostgreSQL dump, and move the data root back to the old location
(or re-point the old Silo compose stack at the compatibility symlink). Keep
the backup until you have verified library scans, metadata refreshes, and
playback under Vio.
