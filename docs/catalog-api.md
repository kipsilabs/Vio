# Catalog API

> **API lifecycle:** this documents the stable `/api/v2` native contract, which locks with Silo
> 1.0. The frozen alpha `/api/v1` surface answers the same features through the pre-1.0 bridge
> window and is then retired. See [the native API contract](architecture/api-contract.md).

## Version liveness

`FileVersion.available` reports the durable per-version health signal on the
item detail and item-versions responses: a virtual candidate is available when
it has not been stamped failed (`failed_at` is NULL), a local file when it is
not marked missing (`missing_since` is NULL). The field is omitted when the
version is available, so an absent `available` means available/unknown; `false`
means the version is currently unavailable. `FileVersion.failed` remains the
virtual-only "produced no bytes at stream-open" flag.

On the playback side, the `/api/v2` playback plan projection carries
`effective_virtual_uri` alongside `virtual_source_revision`, so a client can
detect a substituted virtual candidate and adopt it in its version menu.

`POST /api/v1/catalog/versions/check` batch-tests a set of media file IDs and
stamps the durable signal. The request is:

```json
{
  "file_ids": [123, 456]
}
```

At most 40 IDs are accepted per request (413 `too_large` beyond that; 400
`bad_request` when the body is missing or the list is empty). Each file is
tested cheaply — virtual rows resolve their pinned `?result=` candidate through
the provider (no media transfer), local rows are read from `missing_since` with
no probe — with bounded concurrency and a per-file timeout. A confirmed dead
pin stamps `failed_at`; a successful resolution clears it. Ambiguous provider
errors (provider down, timeout) leave the stamp unchanged and report the row's
current computed availability, so a provider outage cannot mass-tag versions.

**Response** (200 OK):

```json
{
  "results": [
    { "file_id": 123, "available": true },
    { "file_id": 456, "available": false }
  ]
}
```

Unknown or deleted file IDs are reported as `available: false`.

## Multi-audio language support and release metadata

MULTi/DUAL releases — a single audio stream tagged `und`/`mul`/empty whose
languages live in the track title (e.g. "English / French / Spanish") — now
carry real language identity instead of being collapsed to a single code.

- **`FileVersion.audio_tracks[].languages`** — the full advertised language
  list for a MULTi/DUAL track, parsed from the track title at probe time when
  the container language tag is absent, undetermined, or multiple. `language`
  keeps the primary code (the first concrete one when the tag was
  undetermined). Rows probed before this support are backfilled at read time
  from the embedded title until the probe-version re-probe catches up.
- **`FileVersion.audio_tracks[].index`** — the **absolute container stream
  index** as ffprobe reported it (video is typically stream 0, the first
  audio stream 1, subtitles interleaved between them). It is NOT the
  audio-only FFmpeg ordinal used by `0:a:N` map specifiers, and NOT the
  track's position in the `audio_tracks` array. Track list order can differ
  from container order on MULTi releases; clients that need to select a
  track for ffmpeg must convert this index to the audio-only ordinal
  (the rank of the selected track among tracks ordered by absolute index —
  see `playback.AudioStreamOrdinal` server-side) rather than sending this
  value directly.
- **`FileVersion.release_name`** / **`FileVersion.release_group`** — the file
  stem (basename without extension) and the trailing group tag on
  release-style names (`Movie.2023.2160p.AltMount` → group `AltMount`), so
  clients can show which release a version is. `release_group` is empty when no
  group tag is present.

`GET /api/v1/catalog/filters` reports the language facets on
`audio_languages` and `subtitle_languages` (alongside `resolutions`) when
`include_technical` is true (the default). A MULTi track satisfies the
audio-language browse filter for any of its `languages[]` codes, not just its
primary `language`.
## People search

`GET /api/v2/catalog/people` (`listPeople`) accepts a name fragment in `q` and
`limit` from 1 to 100 (default 20). Case-insensitive exact name matches come first;
other matches sort by name, with person ID breaking ties. Ranking happens before
applying the limit.

The optional `media_scope` parameter limits results to people credited on items
in that scope. It accepts `video` (movies and series), `movie`, `series`, `episode`,
`audiobook`, `ebook`, or `manga`. Omit it to search credits across all media scopes.
Results require at least one credit visible to the viewer. Library restrictions, disabled libraries, rating
limits, and excluded media types apply before the limit, including when the media
scope is omitted. Every credit role participates, so directors match video searches
and authors and narrators match audiobook searches. A person with several matching
credits appears once.

Episode credits inherit library visibility and rating limits from their parent
series. Media scope and excluded media types still apply to the credited episode.

`GET /api/v2/catalog/search/capabilities` advertises `people_media_scope: true`
when people search supports media scopes and viewer access filtering. Clients
must check this signal before offering people results, including unscoped
searches. Web omits the People row on older servers that do not advertise support.

Web search applies its selected media scope to both titles and people. People
responses remain an `{items}` collection with string IDs. The v1 bridge retains
its existing alphabetical, unscoped search.

## Person detail

`GET /api/v2/catalog/people/{id}` (`getPerson`) returns one person. A read counts
as a view: when the person's metadata is incomplete or stale and no provider lookup
ran recently, the server queues a background refresh.

Clients that warm a cache speculatively, such as web prefetching the cast of an
open item, pass `prefetch=true`. A prefetch returns the same person but does not
queue a refresh; missing metadata is left to the server's background sweep. Read
the person without `prefetch` when the user actually opens them.

`GET /api/v2/catalog/search/capabilities` advertises `person_prefetch: true` when
the server accepts the parameter. Check it first: `/api/v2` rejects unknown query
parameters, so an older server answers a prefetch read with `422`.

## Saved browse sort

`PUT /api/v2/collections/sort-preference` (`setCollectionSortPreference`) saves the
acting profile's sort for a library collection, user collection, Watchlist, or
Favorites. The profile is identified by the `X-Profile-Id` header. The request body
is a `CollectionSortPreference`:

```json
{
  "collection_kind": "watchlist",
  "field": "added_at",
  "order": "desc"
}
```

A successful save returns `200` with the stored `CollectionSortPreference`.

`collection_kind` accepts `library`, `user`, `watchlist`, or `favorites`.
`collection_id` is a string and is required for collection kinds; it is omitted or
ignored for Watchlist and Favorites. Saved personal-list preferences accept
non-personalized sort fields; `added_at` means the date the item was added to the
list. Personalized sorts (`progress`, `date_viewed`, and `plays`) are rejected for
both saved preferences and Favorites/Watchlist browse. History accepts
`date_viewed` with an active profile, but rejects mutable `progress` and `plays`
sorts. An empty `field` pins the profile to list source order.

`DELETE /api/v2/collections/sort-preference?collection_kind=watchlist`
(`clearCollectionSortPreference`) removes the saved preference and returns `204`.
Collection kinds also require `collection_id` on DELETE.

When a catalog request has no explicit sort, its saved preference is applied
before the source default. `GET /api/v2/catalog` reports an applied saved/default
sort as `effective_sort`; source order omits that field. `effective_sort` is
reported the same way for `group=work` requests, and `sort_metrics` on each item
describes the effective sort rather than the (possibly empty) requested one.

## Feature detection

`GET /api/v2/collections/capabilities` (`getCollectionCapabilities`) returns a
`CollectionCapabilities` document whose `sort_preference_kinds` lists the
`collection_kind` values this server accepts:

```json
{
  "sort_preference_kinds": ["library", "user", "watchlist", "favorites"]
}
```

Check it before saving a Watchlist or Favorites preference. The
`collection_sort_preferences` boolean reports only that saved preferences exist at
all, so it cannot be used to detect the personal-list kinds. When
`sort_preference_kinds` is absent, assume `library` and `user` only. The
document supports `If-None-Match` and returns `304` when the caller's copy is
current.

Virtual-item materialization is a bridge-era admin repair path, not part of the
v2 collections contract. It is advertised by `admin_item_materialize` on
`GET /api/v1/collections/capabilities` and served by
`POST /api/v1/admin/collections/{id}/materialize/{item_id}`. See
[Admin item materialization](#admin-item-materialization).

`login_sharing: true` reports that a personal collection is either private to its
creator or shared with every profile on the login, that `listCollections` includes other
profiles' shared collections, and that only the creator changes or orders a collection. Show
**Shared with me** and the single **Show to other profiles** switch only when it is true; see
[the personal collections API](collections-api.md). `groups` is always false.

The document's `import_sources` lists the sources a new imported collection can come
from (`mdblist`, `tmdb`, `tmdb_list`); it is empty when `imports` is false. Check for
`tmdb_list` before calling `importTMDBListCollection` (`POST /api/v2/collections/import/tmdb-list`),
which follows a public TMDB list. The administrator capability document
(`getAdminCollectionCapabilities`) carries the same field for `importAdminTMDBList`.

## Library-scoped version lists

`library_id` on `getCatalogItem`, `listCatalogItemVersions`, `listCatalogItemEpisodes`,
`listSeriesSeasons`, `getSeriesSeason`, and `listSeasonEpisodes` names the library the
viewer opened the item from. It always validates that the item is a member of that
library and picks the library's metadata language as the presentation fallback. It
does not, by default, change which files come back: an item stored in several
libraries lists every version the viewer may access, so a "Movies" and "Movies 4K"
split still shows both files from either library.

The administrator setting `catalog.scope_versions_to_library` (default `false`)
makes those reads answer only the versions stored in the named library. A read
without `library_id` is unaffected, and so is `getWatchDetail`, watch-together
selection, and the Jellyfin compatibility surface: an item always plays from its
full accessible version list. No client change is needed: the setting only
changes what an existing `library_id` request returns.

## Section quality badges

Home and library section cards derive `overlay_summary` from the best accessible,
non-missing media file: resolution first, then dynamic range. A series includes
all its eligible episode files even when an episode also appears on the page.
Library restrictions and the playback quality ceiling apply before selecting the
file, so a restricted profile's badge describes a file that profile can access.

Each section response reads current committed file metadata. Badge summaries have
no result cache: a subsequent request sees file updates, removals, and library
moves. Clients must fetch again to update their existing cards.

Recently added TV groups a show's new episodes by arrival time. An episode
first seen within 2 hours of the show's previous arrival joins that arrival's
group, so a chain of imports stays one group even when it spans longer than 2
hours. A group with one episode returns an `episode` card; a larger group
returns the `series` card, ordered by its newest arrival. Grouping ignores
how the files were scanned: one scan per imported episode, as arr webhooks
produce, groups the same way as one library scan. On the home row, the
section's `total_count` is a lower bound: it exceeds `item_limit` when more
cards exist. The catalog view reports the exact count.

Recently-added section membership is shared only within the same library and
access scope. Scan-complete events are coalesced into invalidations at most once
per 30 seconds; invalidation requests a refresh on the next read. While
one background rebuild runs, readers may use the previous membership for at most
30 seconds from the first read after invalidation, capped by its original expiry.
An idle scope retains that original expiry until a reader requests the refresh. Repeated scans and failed refreshes
cannot extend that deadline. Cold or expired membership requires a fresh build;
an older in-flight build cannot replace the current generation. Badge summaries
and per-profile playability are recomputed during this grace period.

## Bridge note

The alpha `/api/v1` surface exposes the same saved-sort and capability features at
`/api/v1/collections/sort-preference`, `/api/v1/collections/capabilities`, and
`/api/v1/catalog`, with numeric `collection_id` values instead of strings. Those
paths are frozen: no feature work lands on them, and Silo 1.0 answers the whole
`/api/v1` namespace with `410 Gone` and the `client_upgrade_required` problem code.
Build against `/api/v2`.

## Personal-list pagination

`GET /api/v2/favorites` and `GET /api/v2/watchlist` use opaque cursors over descending
`added_at`, then descending item ID. The cursor retains the database timestamp's full precision;
clients must send it unchanged rather than construct it from visible timestamps. PostgreSQL orders
by the stored timestamp column so the existing profile/time indexes can serve the page. Visible
`added_at` fields remain UTC timestamps with millisecond precision. The frozen v1 list queries and their timestamp
formatting are unchanged.

## Watchlist titles outside the library

The watchlist can also hold movies and series the library doesn't have, keyed by
TMDB ID. They are not catalog items: `GET /api/v2/watchlist`, `GET /api/v2/catalog`
with `source=watchlist`, smart filters and the home Watchlist row never return them.
Read them with `GET /api/v2/watchlist/titles`, which pages by the same kind of opaque
cursor over descending `added_at`, then descending title ID; add and remove them with
`PUT` and `DELETE /api/v2/watchlist/titles/{media_type}/{tmdb_id}`. Check
`watchlist_titles_supported` on `GET /api/v2/requests/status` first. It is false
while requests are off, and the operations then answer `409 capability_disabled`.

When such a title reaches the library, the next watchlist read moves it onto the
library watchlist with its original `added_at`: the watchlist list and entry reads,
a catalog query with `source=watchlist`, the home Watchlist row and an item's
`user_state.in_watchlist`, as well as `GET /api/v2/watchlist/titles` itself. See
[api-contract.md](architecture/api-contract.md#watchlist-titles) and
[External watchlist titles](architecture/external-watchlist.md).

## Catalog query windows

`POST /api/v2/catalog/query` is the structured-body form of `GET /api/v2/catalog`.
It accepts the browse source identifiers, `q`, `name_prefix`, `type`, rule
`groups` and `match`, `sort`/`order`, a page `limit` up to 100 (GET allows 200), and an optional
`query_limit` for the complete result traversal. GET accepts the same rule groups
as a JSON array in `groups` and expresses descending sort as `sort=-field`.
Unknown rule fields and unsupported operators return `422`.

`name_prefix` matches the start of the key title sorting uses: the sort title,
or the title when no sort title is set. "The Hobbit" with sort title
"Hobbit, The" matches `h`, not `t` or `the`. Jellyfin's `NameStartsWith`
follows the same rule. The one exception is recently added TV, which also
matches an episode's own title so episode cards can be found by name.

Both operations return shared catalog cards, `page.next_cursor`, `page.has_more`,
`total`, `total_exact`, and `window_cursor`. Send `next_cursor` unchanged as
`cursor`, without `seek`, for an adjacent page. This reuses the ordering boundary
already returned by the previous page. A virtualized client can retain
`window_cursor` and send it with `seek`, a zero-based result position, to request
a distant window or return to position zero. Use this path when the preceding
page's continuation is unavailable; independent distant windows can load in
parallel without fetching intermediate pages. A seek locates an additional SQL
ordering boundary; it can scan the sorted prefix and does not have constant cost.
The complete browse request has a
10-second deadline and honors client cancellation. Keep only visible and
overscan pages active, and cancel requests when the query changes.

Cursors are bound to the operation, viewer/access policy, filters, page size,
query cap, and requested sort. Changing these inputs starts a new traversal.
`skip_total` may change between windows without invalidating the cursor. The
cursor retains a resolved saved sort so later pages do not reread a changed
preference. A nonexact total is an estimate or lower bound, not a verified final result count.

SQL query continuation retains the complete typed ordering tuple, including the
unique item identity and explicit null ordering. Page rows and an optional count
share one PostgreSQL snapshot. Later pages read live data: an insertion cutoff
excludes newer catalog arrivals where supported, but does not freeze titles,
ratings, progress, visibility, or other mutable sort/filter values. Clients must
not treat a cursor as a frozen catalog export.

Collection-source cursors additionally retain the selected collection's durable
revision. Authoritative revision reads bracket parent/access resolution and page
construction; a committed definition, membership, or order change invalidates the
result. A changed collection returns `400` `invalid_cursor`; restart the query.
These checks do not invalidate a collection when unrelated catalog data changes.

PostgreSQL-dependent viewer predicates require the selected user-store provider
to expose its authoritative SQL state. Unsupported SQLite query combinations
return `501` `capability_unsupported`, rather than silently reading unrelated
PostgreSQL viewer rows. SQLite manual collection source-order paging remains
supported; arbitrary manual sorting, nonzero manual seeks, and personalized SQL
filters are unsupported during storage consolidation.

Recent-TV continuation compares the final event timestamp, target type, target
identity, and event identity after event grouping. Recently-added, released, and
random sections retain their source ordering; random sections retain a seed in
the cursor. Audiobook author/narrator/series groups compare their normalized group
identity after any count or duration sort. Work grouping chooses the first
accessible ebook/audiobook edition under the complete source order before applying
the group cursor. A query cap limits source editions before grouping.

### Search continuation

Text searches with a nonempty `q` and the default `query` source accept explicit
`relevance` sorting, including structured requests with rule groups. Other
sources and saved collection definitions reject `relevance`; it describes a
text query's ranking rather than a persistent collection order.

`GET /api/v2/catalog/search/capabilities` reports the selected provider and, for
Meilisearch, `result_window_limit`, `session_ttl_seconds`, and
`max_sessions_per_account`. Catalog query bodies default to 50 results per page. Search
responses also expose the applicable window limit and fixed session expiry in
`search_diagnostics`.

PostgreSQL search retains the complete relevance tuple or requested SQL sort
rather than a numeric page. It selects the FTS or bounded fuzzy retrieval family
on the first page and retains that choice. The existing fuzzy candidate cap and
reranking remain in force. A fuzzy-family query that becomes a richer FTS query
returns `invalid_cursor` so the client can restart. PostgreSQL search retains its
three-second deadline inside the overall browse deadline.

Meilisearch captures the configured ranked result window in one provider response,
then stores its filtered, ordered candidate IDs in shared Redis for 15 minutes.
The configured window must be between 1 and 1,000 candidates; a larger runtime
index setting returns `capability_unsupported` before serving a partial ranking.
This is the provider's reachable window, not an exact global match count.
At most 16 ranking sessions are retained per account; starting another discards
the oldest retained session. Session requests do not extend expiry.

The retained ranking binds the query, provider configuration, account/profile,
and access scope. Pages reauthorize each candidate and advance past deleted or
inaccessible IDs. Explicit window seeks count visible rows within this bounded
ranking. Metadata and access remain live; the retained IDs and their order are
immutable. Fallback may select PostgreSQL before the first page, but a continuation
never switches providers. Expiry or Redis eviction returns `invalid_cursor`;
Redis failure returns `dependency_unavailable`. Restarting performs a new search.

## Admin item materialization

`POST /api/v1/admin/collections/{id}/materialize/{item_id}`

This is a bridge-era admin repair path with no v2 port. Requires administrator
authentication. Idempotently establishes or repairs virtual playback files,
profile variants, and released episodes for a collection item.

`files_created` and `files_existing` count distinct virtual file identities
across the base item and released episode variants for this operation. A file
identity is the `(owner installation, target library, virtual URI)` tuple.
`episodes_materialized` counts distinct released episodes represented by the
result, not the number of provider/profile files. Repeating the request reports
the same episode count and moves files from `files_created` to
`files_existing`.

**Response** (200 OK):

```json
{
  "success": true,
  "content_id": "movie-tmdb-12345",
  "media_type": "movie",
  "files_created": 1,
  "files_existing": 0,
  "episodes_materialized": 0,
  "message": "Materialized 1 virtual files (0 existing) for movie-tmdb-12345 (movie)"
}
```

**Status Codes**:
- `200 OK`: Item was successfully materialized or verified (idempotent).
- `400 Bad Request`: Ineligible media type, incompatible target library, or virtual playback is disabled on the collection.
- `401 Unauthorized`: Missing or invalid authentication token.
- `403 Forbidden`: Authenticated user is not an administrator.
- `404 Not Found`: Collection or item not found, or item is not a member of the collection.
- `503 Service Unavailable`: Upstream virtual provider plugin is unavailable or misconfigured.

**Sync staging**: during collection sync, newly prepared files stay hidden
until membership acceptance commits. Removing a collection member only deletes
catalog rows proven to be collection-created virtual state; ordinary
metadata-only entries survive as catalog rows.

## History ordering

History defaults to chronological watch-event order. Explicit `date_viewed`
sorting uses each displayed item's latest visible history event, including
episode events collapsed into their parent series; it does not require a
completed watch. `order=asc` puts the oldest latest watch first, and `desc`
puts the newest first. Library/media-scope/search overlays retain this order
before pagination. History does not currently support saved sort preferences.

## Season-list artwork

`GET /api/v2/images/capabilities` advertises
`"season_list_artwork_param": "include_artwork"`. On
`GET /api/v2/catalog/series/{id}/seasons`, this optional boolean defaults to
`true`: omitted and `true` retain the usual artwork. `false` skips poster
preparation and omits `poster_url` and `poster_thumbhash` from each season,
while preserving metadata, viewer rollups, play targets and the `items` envelope.
Invalid booleans return `422 validation_failed`. The parameter does not apply
to single-season or episode operations. Clients can use the capability to
select text-only season lists; callers that omit it keep their existing behavior.

## Episode release timing

Episode rows (`listCatalogItemEpisodes`, `listSeasonEpisodes`) and episode
details (`getCatalogItem` on an episode id) carry `release_state`:
`upcoming` when the calendar `air_date` is after today (UTC), `released`
otherwise, absent when the air date is unknown. The decision is one shared
server-side classification (`catalog.ReleaseStateForAirDate`): no timezone
conversion beyond the stored calendar date, and unknown or malformed dates
fail open (absent) so missing metadata never bans playback.

`GET /api/v2/capabilities/episode-release` is the feature-detection
document for the member. Clients read it before branching on
`release_state` and keep today's plain rendering when the capability is
unavailable or the field is absent — an older server that omits the field
must not gain the upcoming treatment. The release state is presentation
timing only — it never gates playback. A future date with a playable file
still plays; the badge says when it aired, not whether it is available.

The bundled web client dims upcoming artwork, shows an `Upcoming · <date>`
label in season lists, grids, the episode carousel, and the episode detail
hero, and keeps detail navigation, Play eligibility, menus, and
watched/progress state intact. Frozen v1 episode responses do not expose
this field.

Third-party Jellyfin clients need no change: `PremiereDate` already carries
the air date, fileless episodes already map `LocationType: Virtual` with no
`MediaSources` (so they show as unavailable), and no DTO field was added.
First-party mobile clients need the same badge-or-plain rule when they adopt
`release_state`: branch on the capability, never on client-side date
inference; Play eligibility stays unchanged; unknown/absent states render
as today.

## Episode title cards

An episode's title card is its still. Episode rows (`listCatalogItemEpisodes`,
`listSeasonEpisodes`) carry it as `still_url` and `still_thumbhash`, falling
back to the series' backdrop or poster when the episode has no still of its own
(`episodeResponseShell`). Episode details (`getCatalogItem` on an episode id)
are a different shape: the episode's still is served as `poster_url` with
`poster_thumbhash`, and the series backdrop is served separately as
`backdrop_url` with `backdrop_thumbhash`. Neither detail member falls back to
the other; when the episode has no still, `poster_url` is absent and the series
backdrop remains in `backdrop_url`. The bundled web client picks the hero
artwork in its own order — `poster_url` (the still), then `backdrop_url`, then
the season row's `still_url` — and pairs the chosen URL with that same source's
thumbhash even when it has none. An administrator sets a title card through
`POST /api/v2/admin/items/{episode_id}/images/apply` with `type: "titlecard"`
(or `"still"`); see
[Admin catalog API](admin-catalog-api.md#item-image-selection). The two names
are the same artwork. A title card applied to a movie, series, or season is
refused with `unsupported_image_type`, because only an episode has one. That
refusal is an allowed bridge correctness fix: before `titlecard` aliased the
still, the shared apply resolver fell through to `poster` and silently stored
the image as that scope's poster. A silent wrong-type store was a bug, not a
contract, so refusing it on the frozen bridge is a fix rather than a contract
change; every request that was valid before keeps its image type and result.

## Collection membership titles

`GET /api/v2/collections/{id}/items` and
`GET /api/v2/admin/collections/{id}/items` include an optional `title` on each
membership row when its catalog title is available. Editors can display that
title while retaining `media_item_id` for mutations and ordering. Clients should
fall back to the ID when the title is absent. Personal membership pages hydrate
titles through the existing viewer access filter; admin pages require acting
administrator access. Membership identity, ordering and cursor revision checks
are unchanged. Frozen v1 membership responses do not expose this field.

## Collection virtual playback default

Collection creation requests accept a `virtual_playback` boolean: the TMDB,
Trakt, and MDBList import endpoints and the template-bundle apply endpoint. It
controls whether items matched outside the selected libraries are kept as
zero-storage virtual entries that Silo Virtual Library resolves at playback.

The `/api/v2` import and template-apply request bodies expose the same optional
`virtual_playback` boolean; omitting it defaults to on, and `false` opts out.

`virtual_playback` defaults to on when the field is omitted, so third-party
clients and creates-from-template get the same behavior as the first-party
admin UI. An explicit `false` disables it and limits the collection to items
present in the selected libraries; an explicit `true` keeps the default
behavior. The default does not change the stored field's semantics: an enabled
collection stores `virtual_playback: true`, and a disabled one omits the key,
which every reader treats as false.

## Advisory age

Movies and series may carry `advisory_age`, a recommended minimum viewer age from
an advisory service such as Common Sense Media, and `advisory_source`, which names
who recommended it (`commonsense` or `mdblist`). Both are optional and appear
only together; an item with no advisory omits both.

The advisory is not a certification. `content_rating` remains the certification a
rating body issued, and it alone drives the content-rating ceiling
(`max_content_rating`). Do not present the advisory as a rating a viewer has to
satisfy.

Coverage is partial by design. The providers that supply advisory ages are rate
limited per day, so on a large library some titles carry one and others do not,
and the set grows over time. Absence means "not fetched yet", never "suitable
for everyone".

Whether to show the badge is a per-profile choice, `catalog.show_advisory_age`
in the settings contract, default off. The field is served regardless; the
setting decides whether a client renders it and never changes what a profile may
watch. Detect support by reading the setting from the settings contract
capabilities rather than sniffing versions.

### Advisory-age limit

A household manager can also limit a profile by advisory age with the profile's
`max_advisory_age` (an integer from 1 to 21, or `null` for no limit) on the v2
profile operations. The server hides every title whose `advisory_age` is above
the limit, everywhere the content-rating ceiling applies: browse, search, detail,
episodes, sections, progress, recommendations and the Jellyfin-compatible API.
Episodes use their series' advisory age. Other item types, including the beta
book libraries, never carry an advisory age, so the limit never hides them.

- The limit only ever tightens. It is ANDed with `max_content_rating`, and a
  title must pass both.
- By default a title with no advisory age is **not** hidden by the limit; the
  content-rating ceiling alone decides it. `access.unrated_content` does not
  apply to the advisory limit.
- A profile can instead require an advisory age with `require_advisory_age`
  (boolean, default `false`). With it set, a title with no advisory age is
  hidden too, so the profile sees only titles an advisory service rated at or
  under the limit. It has no effect without `max_advisory_age`. On a large
  library that has not been looked up yet, such a profile starts nearly empty
  and fills in as ages arrive: the opposite of the default, where titles
  disappear as ages arrive.
- Because coverage grows as the provider enriches the library, the set of titles
  a limited profile sees can shrink over time, for example when a title a child
  could see gains an advisory age above the limit. `advisory_titles` on
  `getAdminDashboardStats` reports how many movies and series carry an advisory
  age, next to `total_movies` and `total_shows`.
- Media-request discovery cannot apply the limit, because titles outside the
  library carry no advisory age.
- Only a household manager (a server admin, or the primary profile) can set or
  clear either field; a restricted profile cannot change its own limit.
  Changing either bumps the account's access policy revision, the same as
  changing `max_content_rating`.
- Detect support with `max_advisory_age_supported` and
  `require_advisory_age_supported` on the `listProfiles` response. They are
  separate because `require_advisory_age` arrived later, so a server can report
  the first without the second. The profile operations reject unknown members,
  so do not send either field to a server that does not report it.

Frozen v1 responses do not expose these fields.

## Collections on title pages

The v2 item detail of a movie or series carries `collections`, the visible
server collections the item belongs to. Each entry has:

- `id`: the collection id, used to open its full item list
  (`listCatalogItems?source=library_collection&collection_id=<id>`).
- `title`: the collection's display name.
- `poster_url`: the collection's poster, presigned and short-lived, empty when
  it has none.
- `poster_thumbhash`: the poster's thumbhash, absent when the collection has no
  poster.
- `item_count`: the collection's stored member total.

The list is present on every v2 item detail as an array, empty when the item is
in none and when the membership lookup fails. It lists stored server-collection
memberships only: a smart collection derives its members from its query at read
time and stores no rows, so it is never listed, and personal (user) collections
are a separate surface this field does not include. `item_count` is the
collection's total membership, not the members the viewer may see.

The membership key is the item's own id for a movie or series. An episode detail
answers its parent series' collections and a season detail answers its series',
so both report the series' memberships rather than their own; a title with no
series, such as an audiobook or ebook, answers empty. A collection the viewer's
library scope cannot reach, and a hidden collection, are never listed.
`poster_url` follows the same viewer rule as the library Collections tab: an
uploaded or template poster is shared, while a generated collage shows only
members the viewer can access.

`PATCH /api/v2/admin/items/{id}/metadata` returns the same `collections` row on
its detail body, so a metadata save does not appear to drop the item's
memberships.

`GET /api/v2/capabilities/item-collections` is the feature-detection document.
It answers `state: available` on a server whose item detail carries
`collections`, and `unsupported` otherwise. Clients that omit the row on an
older server without the operation keep their existing behavior.

## Ratings on title pages

Every client shows a title's external ratings the same way: the v2 item detail
carries `ratings`, the list to render, already chosen and formatted by the
server. It is present on every v2 item detail, as an empty array when there is
nothing to show. Each entry has:

- `source`: `imdb`, `tmdb`, or a name a metadata plugin declared (see "Rating
  sources" below).
- `name`: the source's plain-text mark: `IMDb`, `TMDB`, or the name the plugin
  declared, such as `RT`.
- `score`: the rating on a 0-100 scale.
- `display`: the score on the source's own scale, formatted: `8.5`, `93%`,
  `4.2`.

Clients render each entry as its mark followed by `display`, in list order.
The list holds at most three entries, the first in display order, so every
client shows the same three ratings on one line.
Marks are plain text, never a source's logo artwork, with one exception: TMDB's
approved logo may stand in for the `TMDB` mark, as TMDB's terms allow. Clients
do not recompute the list from the `rating_*` members.

IMDb and TMDB are always in the list when the title has them. Every other
source is one an enabled metadata plugin declares, and appears only after an
administrator turns it on in the `catalog.extra_rating_sources` server
setting, a comma-separated list of source names that is empty by default,
because the owners of those scores restrict how others may display them.
IMDb, TMDB and Rotten Tomatoes come from the `rating_*` members, the same
numbers poster badges and browse sorting use; the other sources come from
`rating_sources`.

Cards follow the same choice: every v2 card leaves out `rating_rt_critic` and
`rating_rt_audience` unless a plugin declares that source and the
administrator turned it on, so poster
badges show a Rotten Tomatoes score only where title pages do. Item detail
does the same, and its `rating_sources` lists only the sources clients show.
The exception is a viewer who may curate the item's metadata (an admin, or an
account with the metadata curation permission): their item detail, and the
detail `updateAdminItemMetadata` returns, keep every stored value for the
metadata editor. Frozen v1 responses are unchanged.

Browse follows the choice too. A v2 catalog browse sorted by
`rating_rt_critic` or `rating_rt_audience` while that source is hidden orders
as if no sort was given: the saved or default order, reported as
`effective_sort`.

`GET /api/v2/capabilities/ratings` (`getRatingsCapability`) is the feature
check. It answers `state: available` on a server whose item detail carries
`ratings`, and `sources` lists the sources title pages and cards show, in
display order, each with its `source` and `name`. Clients use it to decide
whether to render `ratings` and which rating sorts and badges to offer. A
server without the operation predates `ratings`.

A card-sized summary such as a home hero shows one rating, IMDb or, without an
IMDb score, TMDB, as its mark and score. Cards carry raw `rating_*` numbers,
so a client that formats one itself rounds halves away from zero, as the
server's `display` does: a stored IMDb 7.35 reads `7.4`.

## Rating sources

The v2 item detail of a movie or series may carry `rating_sources`, a list of
per-source ratings metadata providers reported, limited to the sources title
pages show. Each entry has:

- `source`: `imdb`, `tmdb`, or a name a metadata plugin declared (below).
  Ignore a name you do not recognize.
- `score`: the rating on a 0-100 scale, whatever scale the source uses itself.
- `votes`: how many votes produced the score, omitted when the source does not
  report it.

Entries follow the order of `ratings`, at most one per source. A stored score
of a source no enabled plugin declares, or one the administrator has not
turned on, is left out, so a score a plugin reported before it stopped
declaring its source is never served; a viewer who curates the item's
metadata still gets every stored source (see "Ratings on title pages"). The
member is absent when no entry is left. It is detail-only: list and section
cards do not carry it. A title page renders `ratings`, not this list.

The four `rating_imdb`, `rating_tmdb`, `rating_rt_critic` and
`rating_rt_audience` members are unchanged, keep their own scales, and remain
the only ratings browse can sort or filter by.

Rating sources follow the same refresh and lock rules as those four members. A
scheduled refresh only adds sources the item lacks, a manual refresh overwrites
the sources the providers report, and locking the rating field freezes all of
them. A refresh never removes a source a provider stopped reporting. Identify
is the exception: it matches the item to a different title, so the sources the
new match reports replace the stored set, and a source it does not report is
removed.

Plugins send them under `ratings.sources` in a metadata item, as
`{"<source>": {"score": 0-100, "votes": n}}`. The server keeps `imdb`, `tmdb`
and the sources the sending plugin declared, and drops any other name, a score
outside 0-100, and a vote count that is not a whole, non-negative number while
keeping its score.

Silo itself names only IMDb and TMDB. Every other rating comes from a metadata
provider that declares it. The built-in NFO provider declares `rt_critic` and
`rt_audience`, the Rotten Tomatoes scores it reads from local `.nfo` files. A
plugin declares its ratings in its capability's manifest metadata, at the top
level or inside the SDK's `metadata` envelope:

```json
"rating_sources": [
  {"id": "rt_critic", "name": "RT", "label": "Rotten Tomatoes critics", "scale": 100, "percent": true},
  {"id": "kinopoisk", "name": "Kinopoisk", "scale": 10}
]
```

- `id`: the source name, matching `^[a-z][a-z0-9_]{0,31}$`, other than `imdb`
  and `tmdb`. `rt_critic` and `rt_audience` name the Rotten Tomatoes scores the
  plugin sends as the flat `rt_critic` and `rt_audience` ratings, which fill the
  `rating_rt_*` members. The server keeps those flat scores only from a plugin
  that declares them, and declaring them lets title pages and poster badges
  show them. They are always percentages: `scale` and `percent` are ignored.
- `name`: the plain-text mark clients show next to the score, at most 24
  characters.
- `label`: optional; the source's full name in the administrator's list, at
  most 60 characters. `name` stands in when it is absent.
- `scale`: the top of the source's own scale, above 0 and at most 100. The
  plugin still sends a 0-100 `score`; a `scale` of 10 shows 72 as `7.2`.
- `percent`: optional; `true` shows the 0-100 score as a percentage, and the
  scale is 100 whatever `scale` says, so `scale` may be left out.

An entry that breaks a rule is dropped on its own, a repeated `id` keeps the
first, and a capability keeps at most eight. When two enabled plugins declare
the same `id`, the first by installation order names and scales it. A declared
source is stored like the others and is shown only after an administrator adds
its `id` to `catalog.extra_rating_sources`; title pages list it after Silo's
own sources. `GET /api/v2/admin/rating-sources` lists every source an
administrator can show, with the plugin that declared it;
`GET /api/v2/admin/rating-sources/capabilities` reports
`plugin_declared_sources: true` on a server that has that list.

Frozen v1 responses do not expose this member.

## Local theme songs, V2

Movies, series, and seasons can own local theme audio. Place `theme.mp3`
(or `.m4a`, `.m4b`, `.flac`, `.ogg`, `.opus`, `.wav`, `.aac`) in the item's directory,
or put audio files in its `theme-music/` directory. Scans honor the library's
ignore rules. Theme audio is separate from media files, extras, metadata
matching, and watch progress. Files must contain audio without video tracks.
Symlinked audio files and symlinked `theme-music` directories are excluded.

Ownership follows current video-file associations. A movie can use its
canonical directory or the directory containing its video. A series uses its
canonical root; flat episode files can use their containing directory when every
video there belongs to that series. A season uses directories below that root whose
videos belong to that season alone, including season directories above disc subdirectories.
Ambiguous directories do not grant a theme to multiple movies, series, or seasons.
Metadata rematching changes ownership without copying theme rows.
Theme-file update events reconcile the owner's audio without importing the library
again. Failed audio probes preserve only that file's cached record, and deleted
videos retain their themes until the scanner removes the missing video rows.

The V2 item detail document includes `themes` with `owner_id` and an ordered
`items` array. Each theme has `id`, `title`, `duration_seconds`, and `container`.
Paths are never exposed. A season without themes inherits its series' set;
an episode uses its season's set, then its series' set. Resolution selects one
owner's set and applies the viewer's library, rating, and quality restrictions.
An empty set retains the requested item's ID and an empty array.
Seasons derived from episode groups use the same ownership and inheritance rules.
If the optional theme lookup fails, item detail still succeeds and omits `themes`.

| Method and path | Result |
| --- | --- |
| `GET /api/v2/catalog/themes/capabilities` | Shared capability document with `delivery: routed`, `transcode`, `cluster_routing`, and `grant_lifetime_seconds` |
| `POST /api/v2/catalog/items/{id}/themes/{theme_id}/playback` | `url`, `expires_at`, `delivery`, and `content_type` for an authenticated login session and verified profile |
| `GET\|HEAD /api/v2/catalog/items/{id}/themes/{theme_id}/audio?token=...` | Theme audio this API node serves, authorized by the playback grant |

The optional playback request body lists what the client decodes as
`accepted_formats`, pairs of `container` and `audio_codec` (for example
`{"container": "ogg", "audio_codec": "vorbis"}`; an empty codec accepts any
codec in the container). The server sends the original when it matches.
Otherwise it sends a conversion to AAC in progressive audio-only MP4
(`delivery: converted`, `content_type: audio/mp4`) when an `mp4` or `m4a`
entry accepts `aac`. A client that decodes neither gets `406 not_acceptable`.
A request without a body receives the original, as before conversion existed.
`transcode` reports whether any conversion route exists on this deployment.

Themes follow the playback routing policy, like video. Original audio follows
`playback.routing.direct_play_egress`: with the default `prefer_proxy`, a proxy
node serves it and the API serves it only when no proxy can. A conversion
follows `playback.routing.remux_execution` and `playback.routing.remux_egress`:
by default a transcode node converts it and a proxy relays it, falling back to
a proxy, then to the API, as far as the policy allows. `proxy_only` and
`worker_only` are never crossed; when no route satisfies the policy, the
playback request answers `503 dependency_unavailable`. Only workers that
advertise `theme_audio_egress_v1` (proxies) or `theme_audio_execution_v1`
(transcode nodes) are chosen, so a mixed-version cluster never hands a theme to
a worker that cannot serve it.

`url` is either this server's audio route or an absolute URL on a proxy's
origin. Both expire after at most five minutes, bounded by the login token's
remaining lifetime. The API grant binds the account, profile, login session,
policy revision, owner, theme file ID, size, and modification time, and each
audio request rechecks the current account, login session, profile,
permissions, ownership, and file. A proxy URL is checked against the same
authority when it is issued and is then authorized by its signed token alone
for its lifetime, as video stream tokens are; it names the theme file, its
size and modification time, the serving proxy, and, for a conversion, the
transcode node. Grants use a separate signing key derived from the server
secret and cannot be used as an account or ordinary playback token. Clients
must not log or persist signed URLs. Grant responses and audio use
`Cache-Control: no-store`.

Original audio supports byte ranges, HEAD, ETags, and HTTP read preconditions.
Authorization happens before a conditional response. A converted stream has no
length and no byte ranges; replay it with a new grant. The node serving a
theme must read the theme directory at the path the scanner recorded, as it
must for library media. Missing local files or changed bytes require a rescan.

The web preferences `ui.theme_music_enabled` and `ui.theme_music_loop` default
to `false` and support profile and profile-device scope on the web platform.
The web player fades theme audio, preserves its position across details with
the same owner, suspends while a navigation destination is unresolved, and
stops on normal playback, logout, or profile change. It handles browser autoplay
rejection and retries a failed audio URL once with a fresh grant. Playback
progress resets that retry budget for a later expiry.

The web player reports its formats with `canPlayType`. It loops original audio
with the element's `loop`, and replays a converted theme with a fresh grant when
it ends.

Apple and Android do not advertise this feature initially, as specified in
issue #937. They need V2 discovery and fixtures, settings, playback, and lifecycle
support before enabling it. Provider downloads, theme videos, uploads, remote
URLs, and HLS theme transcoding are outside this local-file capability. No V1
route or V1 item-detail shape changes.
