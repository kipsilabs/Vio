---
title: Stremio Integration
description: Stream cloud-cached debrid torrents in Vio through a Stremio addon manifest.
summary: How to connect a Stremio provider manifest, map libraries, and troubleshoot virtual playback.
tags:
  - vio
  - docs
  - wiki
  - stremio
  - virtual-library
  - admin
audience:
  - operator
last_reviewed: 2026-10-11
related:
  - ./media-requests.md
  - ./collections-guide.md
  - ../../architecture/virtual-media.md
---

# Stremio Integration

Vio streams cloud-cached torrents directly from a Stremio addon provider —
no local files, no separate download step. You paste the provider's manifest
URL once in Vio's admin settings; Vio then resolves stream candidates per
title at browse and playback time and plays the best direct HTTP stream in
its native player.

This is **core streaming**, built into Vio (`internal/virtuallibrary`) and
configured with `virtual_library.*` server settings — not a plugin. (The
old standalone virtual-library plugin is retired; existing deployments are
migrated automatically, see
[Virtual media architecture](../../architecture/virtual-media.md#non-destructive-retirement-policy).)

## Prerequisites

- A running Vio server with **Movie** and **Series** libraries already
  created (**Admin → Libraries**). If you use the built-in
  [virtual libraries](#admin-configuration-in-vio) (`virtual://movies`,
  `virtual://series`), the **Admin → Virtual Library** page can create
  them for you; either way the virtual catalog maps into these target
  libraries (`virtual_library.movie_library_id`, default 1, and
  `virtual_library.series_library_id`, default 2) — it does not create its
  own.
- A **TMDB API key** (recommended). Vio uses it to enrich the catalog and to
  map TMDB IDs to provider IDs, which widens coverage for titles the Stremio
  addon only lists under one ID scheme.
- A **Stremio addon manifest with a debrid provider configured** — for
  example [Torrentio](https://torrentio.strem.fun/) paired with Real-Debrid,
  AllDebrid, Premiumize, or Torbox. Vio expects the addon to return **direct
  HTTP streams** (debrid-cached); raw uncached torrents with no HTTP URL
  cannot play.

## Obtaining a manifest URL

1. Open the addon's configuration page in a browser (e.g. Torrentio's
   config page) or in the Stremio app.
2. Enter your debrid provider API key, pick your quality/size filters, and
   save — the page produces a personalized install URL.
3. Copy the final URL. It ends in `/manifest.json` and looks like:

   ```text
   https://<addon-host>/<your-config>/manifest.json
   ```

> [!WARNING]
> The manifest URL embeds your private debrid API token. Treat it as a
> secret: only server admins can see it in Vio, never share it publicly, and
> re-paste a fresh URL in Vio if you rotate your debrid key.

Vio validates the manifest on save: it must end in `/manifest.json` and
declare `resources` including `stream` and `types` including `movie` /
`series`. At playback time Vio rewrites it per title to
`…/stream/<movie|series>/<imdb-id>.json` to fetch candidates.

## Admin configuration in Vio

1. Go to **Admin → Settings → Streaming**.
2. In the **Stremio provider** group:
   - Turn on **Enable streaming** (`virtual_library.enabled`).
   - Paste the **Manifest URL** (`virtual_library.manifest_url`).
     HTTPS is required; `http://` is accepted only for private/local hosts
     when **Allow HTTP for local manifests**
     (`virtual_library.allow_insecure_http`) is on — use it for local test
     manifests only.
   - Optionally set the **TMDB API key**
     (`virtual_library.tmdb_api_key`) for broader ID coverage.
   - Tune **Cache TTL (minutes)** (`virtual_library.cache_ttl_minutes`,
     default 10) — how long provider candidates are reused before
     re-fetching — and the **candidate trust window**
     (`virtual_library.candidate_store_hours`) if shown.
   - Use **Test connection** to verify the manifest before saving.
3. Map the target libraries and indexer/search settings (same Streaming
   page or server settings):
   - `virtual_library.movie_library_id` (default 1) and
     `virtual_library.series_library_id` (default 2) — the libraries virtual
     titles land in.
   - Quality presets/profiles (`virtual_library.quality_preset`,
     `virtual_library.enable_quality_profiles`,
     `virtual_library.fallback_to_any_stream`) decide which candidate wins.
4. Verify on the **Admin → Virtual Library** page (`/admin/virtual-library`):
   if the movie/series targets don't exist yet, the **Virtual library
   setup** card offers one-click **Create Virtual Movies** / **Create
   Virtual Series** (zero-storage `virtual://movies` / `virtual://series`
   libraries). Once the manifest resolves, virtual titles appear in the
   **Release Queue** with title, library, source, candidate counts, and
   last-delivered/last-seen times. If the queue is empty, the manifest is
   unreachable or returns no candidates — see Troubleshooting below.

Changing the manifest URL or toggling streaming takes effect after the
settings save (a restart may be required — the UI marks restart-required
keys).

## Playback flow (what users see)

1. A virtual title shows up in the library like any other title, with its
   versions resolved from the provider's stream list.
2. On play, Vio re-resolves the manifest for that title, picks the best
   candidate under the quality policy, and proxies the direct-compatible
   source with byte-range support (or remuxes/transcodes when the source
   needs conversion).
3. Stream URLs are signed per session — provider credentials never reach the
   client, and signed upstream URLs are never persisted.

## Requests and Virtual Library

Approved media requests can fulfill through the built-in Virtual Library
router instead of (or alongside) Sonarr/Radarr — see
[Media requests](media-requests.md#automatic-virtual-library-fallback).

### Request a title versus request an indexer release

Two different actions, don't confuse them:

- **Request a title** — the normal Requests flow (Discover → Request, or the
  Request button on a title). Creates a Requests record
  (`pending → approved → queued → downloading → completed`), goes through
  approval policy, and fulfills via the configured router (Virtual Library
  core and/or Sonarr/Radarr).
- **Request an indexer release** — on a title's detail page,
  `POST /api/v2/media/{media_id}/virtual-releases/{release_id}:request`
  asks the provider to fetch one specific indexer-only release (a release
  the indexer lists but the streaming provider hasn't cached yet). This is
  provider-direct, outside the Requests queue: no approval step, idempotent
  per release (`MarkIndexerReleaseQueued`), and it never accepts a URL from
  the client.

Use the first to get a title by any means; use the second when you can see
the exact release you want in the indexer list.

## Security considerations

- **Manifest URL secrecy.** The URL carries your debrid token. It is stored
  server-side and visible to admins only — don't paste it into issues,
  screenshots, or chat.
- **HTTPS.** Debrid endpoints must be HTTPS. `allow_insecure_http` exists
  only for local test manifests on private networks.
- **Private network streams.** `virtual_library.allow_private_streams`
  (default off) lets playback contact localhost/LAN/link-local addresses. A
  compromised provider could then make Vio request internal services. Keep
  it off unless you stream from a LAN source you trust; TLS checks stay on
  either way.

## Troubleshooting

| Symptom | Likely cause / fix |
| --- | --- |
| "streaming provider URL must end in /manifest.json" | Paste the full URL including `/manifest.json`, not the addon homepage. |
| Empty Release Queue / no candidates | Manifest unreachable, debrid key invalid/expired, or provider has nothing cached for those titles. Test the manifest URL in a browser — it should return JSON. |
| Title plays something else / wrong version | Quality preset too loose; tighten the quality profile or disable `fallback_to_any_stream`. |
| Only uncached torrents listed | Vio needs direct HTTP streams. Configure the addon with a debrid provider so torrents resolve to cached HTTP URLs. |
| Streams stop after debrid key rotation | Re-paste the fresh manifest URL — the old one embeds the old token. |
| Requests never fulfill via Virtual Library | Check the [requests guide](media-requests.md#troubleshooting): the implicit fallback only applies when no router connection resolves; a misconfigured Sonarr/Radarr row fails the request instead of falling back. |
| Need deeper detail | Server logs under the `virtuallibrary` logger show resolution failures; the normative design lives in [Virtual media architecture](../../architecture/virtual-media.md). |

## Limits

- Movies and series only — the rest of the library kinds are out of scope.
- No live TV, tuners, IPTV, EPG, DVR, or remote-URL shortcuts: those are
  permanently out of scope (see the repo's non-goals), and this integration
  doesn't open a back door to them.
