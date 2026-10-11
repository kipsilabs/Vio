---
title: Collections
description: Curated shelves, smart lists, and synced feeds for your libraries.
summary: Library vs personal collections, smart queries, TMDB/Trakt/MDBList sync, template gallery and bundles, and troubleshooting.
tags:
  - vio
  - docs
  - wiki
  - collections
  - admin
audience:
  - operator
last_reviewed: 2026-10-11
related:
  - ./collection-templates.md
  - ./stremio-integration.md
  - ./media-requests.md
  - ../../collections-api.md
---

# Collections

Collections are named shelves of titles — franchises, genres, award
winners, seasonal picks, or anything you curate. Vio has two layers:

- **Library collections** (admin, server-wide): visible to every profile
  with access to that library. The place for "everyone should see this"
  shelves.
- **Personal collections** (per-profile): owned by one profile
  (`creator_profile_id`), private by default, optionally shared with the
  household (`is_shared`) and optionally surfaced on the server Library tab
  (`include_in_server_collections`).

Either layer can be **manual** (hand-picked titles), **smart** (a
metadata query that stays fresh on its own), or **synced** (mirrors an
external TMDB, Trakt, or MDBList feed on a schedule). Admins also get a
**template gallery and bundles** that seed dozens of proven shelves in one
action.

## Prerequisites

- At least one library with scanned content — collections match against
  what Vio knows.
- For TMDB-backed sources: a **TMDB API key** in Settings
  (`tmdb.api_key`, encrypted, restart-required).
- For Trakt-backed sources: a **Trakt API app** (Client ID/Secret)
  server-wide in Settings; "Recommended" templates additionally need the
  target profile's Trakt account connected under **Settings → Watch
  Providers**.
- For MDBList: nothing required to *use* a public list (sync fetches the
  public `/json` feed); an MDBList API key in Settings additionally unlocks
  authenticated, paginated syncs plus in-app list search/browse.

## Collection types

### Manual collections

Hand-curated lists. Add/remove titles yourself; order is yours to keep.
Best for franchises, staff picks, "kids' weekend" — anything taste beats
rules for.

### Smart collections

Rule-based queries over your catalog (genres, actors, years, ratings,
studios, …). They update themselves as the library grows — add a qualifying
movie and it appears with no further action. Best for "all 90s sci-fi above
7.5" style shelves that should never go stale.

### Synced lists (TMDB / Trakt / MDBList)

A collection wired to an external feed that re-syncs on a schedule:

| Source | Needs | Notes |
| --- | --- | --- |
| TMDB preset (`trending`, `popular`, `top_rated`, `now_playing`, `upcoming`, `airing_today`, `on_the_air`) | TMDB API key | Same shapes the TMDB import endpoint accepts. |
| TMDB discover / TMDB franchise (`tmdb_discover`, `tmdb_collection`) | TMDB API key | Filter sets / franchise IDs ship from the server catalog; applied via bundles (see below). |
| Trakt (`trending`, `popular`, `recommended`) | Trakt app; `recommended` also needs the profile's Trakt account | Recommended collections are scoped to that profile. |
| MDBList (`mdblist.com/lists/…`) | Nothing (public feed); API key upgrades sync | Public `/json` caps at ~2000 entries per page, but sync pages through `limit`/`offset` until exhausted, so large lists import whole. |

Sync cadence is per-collection with a floor: admins pick any schedule
(built-in templates already stagger sensibly — trending every 6h, popular
and streaming services daily, top-rated/editorial/awards/seasonal weekly);
**personal** collections can't sync more often than every
`MinSyncIntervalHours` (24h) to bound API quota across many users.

### Virtual playback in collections

Collection items matched *outside* the selected libraries can be kept as
zero-storage virtual entries that the
[Virtual Library](stremio-integration.md) resolves at playback time
(`virtual_playback` on by default; only an explicit `false` disables it).
Leave it on for "shelf shows everything, plays via streaming"; turn it off
to limit the shelf to titles already in those libraries.

## Setting up collections

### Admin: library collections

**Admin → Collections → Add Collection**, then pick the source:

1. **Manual** — name it, add titles by search.
2. **Smart** — name it, build the query (genres, years, ratings, …), preview
   the matches.
3. **Import** — paste a TMDB/Trakt/MDBList URL or pick a preset, set **Max
   items**, **Default sort**, **Sync schedule**, **Featured** (home/library
   hero), and **Virtual playback**.
4. **Browse Templates** — skip hand-configuring and start from the gallery
   (next section).

Manage from the same page: **Sync now**, show/hide, bulk delete, and
per-collection edit. Bulk actions support sync/show/hide/delete across a
selection. Without any import configured, a synced list's sync can only
fail — the UI doesn't offer Sync for it.

### Templates and bundles (one-click shelves)

**Admin → Collections → Browse Templates** (also inside Add Collection)
opens the gallery: 100+ built-in blueprints across Trending, Popular,
Streaming Services, Top Rated, In Theaters/Upcoming/On Air, Editorial
(awards, yearly bests, seasonal, studios, franchises), and Custom
(bring-your-own MDBList URL). Picking a `tmdb`/`trakt`/`mdblist` template
opens the standard confirmation drawer (libraries, title/description,
poster, max items, sort, schedule, featured, virtual playback) and creates
an identical collection to the manual import flow. `tmdb_discover` and
`tmdb_collection` templates are **bundle-only** — the gallery shows a
read-only summary and points you at bundles.

**Template Bundles** apply curated sets at once (Core Defaults, Streaming
Originals, Awards & Yearly Picks, Seasonal, Studios & Labels, Popular
Genres, Top Rated Genres, Franchise Collections, plus an auto-generated All
Defaults union). The apply view picks target **libraries** (filtered per
template by movie/series kind), optional **Featured** heroes, a
**Delete existing** reset toggle, and a **Preview** dry run; **Apply
Defaults** queues a background job since large bundles create dozens of
collections. Re-applying is safe — existing collections match by slugified
title and are skipped, not duplicated. The franchise placeholder ships with
`collection_id: 0`: edit the created collection with a real TMDB collection
ID before its first sync.

Full reference (categories, sources, API, extending the registry):
[Collection Templates](collection-templates.md).

### Users: personal collections

Any profile creates its own from the **Collections** area: name it
(`is_shared: false` private default in the API), add
titles manually or wire one of the same external sources
(`mdblist_json`, `tmdb_preset`, `tmdb_list`, `trakt_preset`). Sync keeps
only titles that profile can access (its libraries + rating limit), and
reads still filter per viewer — sharing a collection never leaks titles
past someone's parental controls. Sharing controls per collection:

- Private (default) — only the owner sees it.
- **Show to other profiles** (`is_shared: true` — "Show the collection to
  every profile on the login") — household can open it.
- **Include in server collections** (`include_in_server_collections`) —
  also surfaces on the Library tab.

## Management and troubleshooting

| Symptom | Likely cause / fix |
| --- | --- |
| Sync fails immediately | Missing key for that source (TMDB/Trakt app), bad list URL (MDBList must be an `mdblist.com …/lists/…` page), or no network to the provider. Check the collection's last-sync message. |
| Trakt Recommended won't create | The chosen profile has no Trakt account under Settings → Watch Providers — connect it first. |
| Shelf is empty after sync | Nothing matched *and* virtual playback is off (limits to owned titles), or the owner profile can't access the matches (wrong libraries/rating limit). Enable virtual playback or widen access. |
| Personal sync won't go below daily | Intended: `MinSyncIntervalHours = 24` bounds quota. Admin library collections can schedule tighter. |
| Rate-limited by provider | Spread sync schedules (templates already stagger: trending 6h, popular/services daily, top-rated/editorial weekly) and lower Max items. |
| Smart query matches nothing | Loosen one facet at a time with the preview; check genre spelling, year bounds (`gte ≤ lte`), and 2-letter language codes. |
| Duplicates after re-applying a bundle | Shouldn't happen (slug match skips existing). If it did, the title was edited — delete the stray copy and re-apply. |
| Franchise template never syncs | `collection_id: 0` placeholder — edit the collection's source config with the real TMDB collection ID. |
| Poster missing for a template | Posters ship under `web/public/images/collection-templates/{template_id}.jpg`; the `internal/collections/templates` tests assert this — file an issue with the template ID. |
| Need internals | [Collections API](../../collections-api.md), [Collection templates](collection-templates.md), and architecture notes (`docs/architecture/collection-*.md`). |
