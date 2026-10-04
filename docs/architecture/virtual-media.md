# Virtual media architecture

Vio supports storage-free catalog entries through an additive core and RuntimeHost
architecture. Virtual media files are backed by remote streams (e.g. Stremio providers)
rather than local storage; Vio owns catalog persistence, indexing, metadata refresh,
authorization, and playback.

## Trust boundary

- External integrations never receive database credentials and never depend on
  internal table layouts.
- `RuntimeHost.UpsertVirtualMedia` derives the caller's installation identity
  from the authenticated plugin process. It does not accept an installation ID
  from untrusted input.
- The host validates media type, destination library, external identity, and
  URI scheme before opening a transaction.
- Registration is idempotent. Stable provider IDs select the canonical item,
  while a URI-level advisory lock prevents duplicate virtual files.
- Search-index events commit in the same transaction. Metadata refresh and
  home-section cache invalidation run through normal catalog services afterward.

## Playback lifecycle

1. Virtual media rows are registered with `container=virtual`; no placeholder
   files are created on disk.
2. Playback planning recognizes a virtual source without filesystem probing.
3. At playback time Vio resolves the virtual URI through the core virtual
   library (`internal/virtuallibrary`) by path. In Vio, virtual playback resolves
   through core; no virtual streaming traffic dispatches to external plugins.
4. Direct-compatible sources are proxied through Vio with byte-range support.
   Sources requiring conversion continue through Vio's capability-aware
   remux/transcode path. Provider credentials never enter client responses,
   durable plans, or transcode recipes.
5. The resolver runs at playback time so signed upstream URLs are not persisted.

## Indexer-only releases

A title can exist at the indexers before the streaming provider has cached it.
The "Refresh List" flow (`POST /api/v2/media/{media_id}/virtual-candidates:refresh`)
lists provider candidates, searches Prowlarr for matching releases
the provider does not already list, and persists them in
`virtual_indexer_releases` (scoped per content id, episode id, and media
folder). The watch detail exposes them additively as `indexer_releases`, and a
user can request one on the provider
(`POST /api/v2/media/{media_id}/virtual-releases/{release_id}:request`).

The stored `download_url` is server-internal: the request endpoint resolves the
row by its opaque id, uses the stored URL, and never accepts or returns a URL,
preventing SSRF primitives. `MarkIndexerReleaseQueued` is the domain identity
that makes a repeated request idempotent. The refresh is a durable `adminjob`
job owned by the requesting user; the provider re-list is the only fatal stage,
while indexer search and candidate probing degrade to warnings. A completed
refresh publishes `catalog.item.changed` with `change: "versions_updated"` so
clients invalidate the version list.

## Library picker schema formats

Config schemas can declare properties that should be chosen from libraries rather
than typed as free text by specifying a host-known JSON Schema `format`:

- `silo-library` — any enabled library.
- `silo-library-movie` — an enabled movie-capable library (`movie`/`movies`,
  plus `mixed`).
- `silo-library-tv` — an enabled TV-capable library (`series`/`tv`/`show`/
  `tvshows`, plus `mixed`).

The admin UI renders the property as a dropdown of enabled libraries labeled
`Name (ID)`, and the value written to the field is the library's numeric id as a
string. If a saved id no longer matches an enabled library, the form keeps it
visible as `"<id> (not found)"` rather than silently dropping it.

## Non-destructive retirement policy

Stremio virtual streaming is built directly into Vio core (`internal/virtuallibrary`),
configured via `virtual_library.*` server settings (Admin > Settings > Streaming).
The standalone virtual-library plugin (`com.drondeseries.vio-virtual-library`) is retired.

To protect existing deployments from data loss:
- The startup migration sequence (`internal/virtuallibrary/migrate.go`) reads
  plugin configuration from the installation store and copies it to core settings
  without needing the old plugin binary to run.
- Destructive uninstallation (`DeleteInstallation`) is explicitly deferred:
  generic plugin deletion triggers `RemoveVirtualMediaInstallation`, which would
  cascade-delete all virtual catalog files carrying legacy plugin installation IDs.
  Legacy installations remain dormant/disabled until catalog rows are re-keyed to
  core ownership (installation ID 0).
- Persistent state file paths (`.vio-virtual-library-*.json`) are preserved to
  prevent orphaning monitoring state on existing servers.
