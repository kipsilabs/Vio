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

### Verified inventory requires a complete enumeration

A virtual file's `verified` inventory status means the playback probe returned
an inventory that passes the completeness gate, not merely that the probe
returned something. The gate detects one specific structural gap:
`scanner.ProbeVirtualSource` flags a partial enumeration
(`scanner.ErrVirtualProbePartial`) when a successful ffprobe result describes
video but no audio **for an item that declared audio**. It does not prove the
whole stream table was read — a source can still omit a track the probe never
enumerated — it refuses to call an obviously truncated read complete.
Video-only sources exist, so a missing audio stream is only an incomplete read
when the item's own declared metadata (an audio codec or declared audio track)
says the source carries audio: then a video-and-no-audio table is a truncated
read (a relay partial response, an analysis-budget cut-off, or an early return),
not a video-only file. An item that declares no audio may legitimately enumerate
video only, and that result is complete. The declaration the probe is judged
against is carried onto the probe input: the catalog row's own metadata, and,
on the paths that probe a row alone (the serve-layer rotation probe, the
stale-source fallback, and the transport fallback), the served candidate's
provider-declared audio codec or languages, so a row whose synthetic tracks have
not landed cannot hide the item's audio and let a thin enumeration verify. An
empty subtitle set is a valid complete answer; it is never a partial signal.
Finding one audio track does not by itself prove the rest of the table was read,
so the probe result is not allowed to be its own completeness oracle.

The completeness gate only holds if it actually runs. The playback probe cache
(`scanner.VirtualProbeCache`) is keyed on the expected-audio signal as well as
the source, so a result measured under a video-only expectation is never replayed
to a probe that expects audio. The shared cache-lookup helper
(`virtualProbeFromCache`) forwards the served candidate's declared audio onto the
lookup input before consulting the cache, so a caller that built its probe input
from a row with no audio metadata still looks the cache up under the
expectation the real probe would have used. Without that, a thin enumeration
cached for a video-only input would be served to an audio-declaring row — and
the cache-only recovery path would merge synthesized audio onto it and persist
it as verified — while the gate never saw it.

A partial probe is not a verdict about the candidate's bytes. The resolve serves
the candidate without consuming the probe-failure budget or releasing the sticky
pin, and the deferred lifecycle records no outcome and publishes nothing. On the
negotiated deferred path the session stays `pending` until a later complete probe
verifies it or the convergence watchdog fails it at the bound. The
non-negotiated paths have no session lifecycle for the watchdog to fail, so they
do not return `pending`: the background probe keeps its own repeated-attempt
streak, and the synchronous resolve settles immediately on the candidate's
declared metadata (non-verified). For both, a single partial is never penalized
(the pin is kept and the failure damper stays clear), and only a persistent
streak arms the failure damper so the resolve stops re-probing and reuses the
declared disposition. Synthesized or hint-only tracks (container index < 0) never
satisfy completeness and never stamp `probe_updated_at`; persisted probe evidence
requires a real stream index.

## Provider stream order

Setting `virtual_library.custom_format_preset` to `server-order` preserves the
Stremio provider's candidate order for listing, refresh, and automatic selection.
It bypasses custom-format scoring, rejection, profile sorting, and confirmed-source
promotion, including promotion before the selectable candidate cap. Saved format
weights remain available when another preset is selected. Failed-source filtering,
release deduplication, URL validation, quality-profile filters, and explicit
candidate pins still apply. The playback picker also bypasses device, probed-source,
and sticky-result promotion. Watch responses carry the provider order into version
lists and default selection; a viewer's explicit selection or display-sort override
still wins. Local files retain their existing positions. Indexer release ranking is
unaffected.

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
while indexer search degrades to warnings. A completed
refresh publishes `catalog.item.changed` with `change: "versions_updated"` so
clients invalidate the version list.

The refresh is list-only: it never opens a candidate's media URL, so no
candidate is probed, played, or downloaded by the refresh itself. Verified
track inventory arrives when the viewer explicitly plays one candidate.

### Cold-start attribution (list_reason + cache_miss_cause)

Every `resolveVirtualPlaybackSource` emits a `virtual resolve timing` line
with two observational fields (never control flow) that size the convertible
share of cold starts — starts that paid the provider round-trip but could
have reused a warm cache:

- `list_reason`: which early gate claimed the start (`fast_path`,
  `cache_hit`, `resumed`, `explicit_declared`, `empty_suppressed`,
  `listed`, `unknown`). `fast_path` is eligibility (intent), not outcome: a
  start can be fast-path-eligible at the gate yet reuse the cache
  (`cache_hit=true`) and take a late fast path in the attempt loop. The
  outcome fields (`fast_path`, `cache_hit`, `listed`) always describe what
  ran. A late fast path with no early gate (durable-resume inside the
  attempt loop, delivery grace) keeps `unknown` with `fast_path=true`.
- `cache_miss_cause`: why the best-result cache did not serve a `listed`
  start (`absent` = no entry, `expired` = lapsed TTL, `forced_relist`,
  `exclusion_or_unusable`, `explicit`, `cache_disabled`). Non-listing starts
  report `none`.

The same dimensions are on `/metrics` as
`silo_virtual_resolve_list_reason_total{list_reason,cache_miss_cause}`, and
prefetch admission/drop/completion as
`silo_virtual_prefetch_outcomes_total{outcome}` (the per-drop slog lines are
Debug, invisible at the default Info level). Labels are bounded: no content
IDs, URIs, or user identity. A day of the resolve series tells whether the
next win is prefetch coverage (`absent` dominant), cache TTL (`expired`
dominant), or forced work (`forced_relist`/exclusion dominant) — the bench
cannot answer that from latency alone. Prefetch itself stays list-only by
design (see `prefetchOne`): it warms the resolver and best-result caches and
never touches a candidate's media URL.

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
