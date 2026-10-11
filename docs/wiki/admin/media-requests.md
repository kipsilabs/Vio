---
title: Media Requests
description: Let household members request missing titles and fulfill them automatically.
summary: How to configure request servers and routing, how approval works, how the Virtual Library fallback fits, and how to troubleshoot.
tags:
  - vio
  - docs
  - wiki
  - requests
  - sonarr
  - radarr
  - admin
audience:
  - operator
last_reviewed: 2026-10-11
related:
  - ./stremio-integration.md
  - ./collections-guide.md
  - ../../architecture/media-requests.md
---

# Media Requests

Requests let people on your server ask for movies and series Vio doesn't
have yet. A request flows through approval (automatic or by an admin), is
sent to a backend that can supply it, and completes when the title lands in
the library.

Backends are either **external download servers** (Sonarr for series,
Radarr for movies) or Vio's own **Virtual Library (Core Streaming)** router,
which registers the title from the Stremio provider withzero local storage.
You can run virtual-only, Sonarr/Radarr-only, or a mix — routing rules
decide which requests go where.

## Prerequisites

- Requests enabled: **Admin → Settings → Requests → Allow requests**
  (`requests_enabled`).
- For external fulfillment: reachable **Sonarr v3/v4 and/or Radarr**
  instances, each with its **API key, base URL, root folder, and quality
  profile**. Keys come from the *arr app's own Settings → General.
- For virtual fulfillment: a working
  [Stremio integration](stremio-integration.md) (manifest URL + target
  libraries). The Virtual Library router is offered as
  **Virtual Library (Core Streaming)** (`virtual-library-requests`).

## Integration setup (servers)

1. Open **Admin → Requests** and use the tabs: **Queue** (approve /
   decline / retry / cancel, with a route preview per request), **Settings**
   (allow requests, approval mode, limits, 4K, watchlist auto-request),
   **Integrations** (backend servers), **User Overrides** (per-account and
   per-group approval/limits).
2. In the **Integrations** tab, **Add** a server and pick the **Server
   type** — `Virtual Library (Core Streaming)` for core, or the installed
   Sonarr/Radarr request-router plugin for external boxes. (The type picker
   appears when more than one option exists.)
3. For Sonarr/Radarr fill in **Name**, **URL**
   (e.g. `http://192.168.1.10:8989`), and **API key**. Virtual Library rows
   use managed defaults instead: URL `virtual://streaming`, API key
   `core-managed` — leave them as-is.
4. Toggle **Enabled** per server, and mark the **4K server** where
   applicable (with Standard routing, 4K versions go there, everything else
   to the other server of that type).
5. New installs also meet this in the **setup wizard** (`RequestsStep`) —
   same fields, asked once during onboarding.

Use the connection's **Test** action before saving: it catches wrong URLs,
wrong keys, and unreachable hosts without touching the queue.

## Routing: where requests go

**Admin → Settings → Requests → Where requests go** (`RequestRouting`;
also reachable from the Requests page) controls which server handles each
approved request:

- **Standard mode** — pick one movie server and one series server (plus
  optional 4K partners). Simple and enough for most homes.
- **Advanced mode** — ordered rules per media type, first match wins.
  Match on genres, years/decades, original language, network/studio, anime,
  or content rating; anything no rule matches falls to **Everything else**.
  Changes save immediately.

Routing resolves **one installation per request**: the first eligible
connection pins the backend, and only connections on that same backend
receive the request's credentials. A second backend's keys are never handed
to the first.

## Approval policies and automation

In the **Settings** tab (**General** group) of **Admin → Requests**:

- **Allow requests**: master switch — people can ask for movies and series
  the server does not have (`requests_enabled`).

- **Approval**: *Approve automatically* sends every request straight to
  routing; *An admin approves* parks new requests in the **Queue** tab
  (`AdminRequests` / `RequestQueue`) until an admin approves, declines,
  retries, or cancels them.
- **Request limit / window**: how many titles one account may request per N
  days. Declined and failed requests don't count; `0` means only accounts
  with their own group/account limit can request. Per-group and per-account
  overrides live under **Access groups** and **Users** — account beats
  group beats default.
- **Also request a 4K version of every title** (`force_dual_quality`):
  normally 4K is requested only for profiles allowed 4K playback and only
  when a server takes 4K; this forces a 4K target on every request.
- **Request titles added to a watchlist** (`requests.watchlist_auto_request`):
  adding a missing title to a watchlist files a request automatically, as if
  the profile pressed Request. Each profile can opt itself out under its
  own settings.

Related knobs: **Request notifications** (Discord/webhook announcements for
submitted/approved/declined/fulfilled; linked from the Settings tab) and
**Autoscan** (reuse these servers to import finished downloads immediately;
linked from the Settings tab).

## User lifecycle: what requesters see

1. In **Discover**, **Browse**, or on a title page, a missing title shows a
   **Request** button (`Requests.tsx`, `RequestBrowse`,
   `RequestActionBar`). Pressing it creates the request.
2. The request moves through statuses:

   ```text
   pending → approved → queued → downloading → completed
   ```

   `pending` waits for approval (skipped under auto-approve); `approved` is
   submitted to the backend; `queued`/`downloading` track the backend's
   progress; `completed` means the title reached the library.
   `declined`/`failed`/`cancelled` are terminal — failed requests keep
   their `last_error` so admins can see why.
3. Requesters watch progress on the **Requests** page (grouped Mine buckets
   + status guide); admins work the **Queue** tab with approve/decline,
   bulk-decline, retry, cancel, and a route preview per request.

Season nuance: a series already in the library can still be requested for
its missing seasons; routers that support seasons fetch just the gap,
others would re-add the whole series and are skipped for such requests.

## How requests reach the library

After approval (or immediately under auto-approve), Vio submits the request
to the routed backend and then reconciles on a cadence — it polls backend
status and flips request targets forward. Two background passes do this
(`reconcile_requests` and `refresh_request_downloads`); a freshly approved
request is also submitted inline rather than waiting for the next tick.

Fulfillment differs by backend:

- **Sonarr/Radarr**: the title is grabbed, downloaded, and imported; the
  request completes when the file lands and the library scan picks it up
  (Autoscan shortens this).
- **Virtual Library**: the title is registered from the Stremio provider as
  a zero-storage entry in the mapped movie/series library — typically
  immediate for already-released titles, monitored until release for
  upcoming ones.

If the title reaches the library by *any* route (another request, a manual
import, virtual registration), pending/approved duplicates complete
without re-submitting.

## Automatic Virtual Library fallback

When the Virtual Library core router is active **and no configured router
connection resolves for a request**, Vio synthesizes an implicit
`core-virtual-library` connection (`virtual://streaming`, `core-managed`)
and fulfills through it. Concretely:

- It triggers at **routing time** when zero connections resolve — not as
  failover after Sonarr/Radarr rejects or fails a submission.
- A **misconfigured** backend row (enabled but unbound, or missing its API
  key) does *not* fall back: the request fails with a submission error
  (`request backend connection is not bound…` / `…has no API key…`) so you
  notice and fix it, instead of silently succeeding elsewhere.
- With no router connections configured at all, virtual fulfillment just
  works out of the box once the Stremio provider is set — no integration
  row required.
- When rows *are* configured, virtual competes as one more eligible
  backend under the normal routing rules; explicit rules beat the implicit
  fallback.

> [!NOTE]
> "Active" here means the Virtual Library service exists with a manifest
> configured — it is not a live health check of the provider. A reachable
> manifest with cached streams is what makes fulfillment actually succeed.
> There is no separate on/off switch for "requests may use Virtual Library":
> to stop requests from using it, remove/disable its integration rows **and**
> note the implicit fallback still applies while the core router is active;
> to stop that too, disable streaming (`virtual_library.enabled`) itself.

## Administrator setup: enable, select, disable

- **Virtual-only**: configure the Stremio provider, leave the Servers list
  empty. Every approved request takes the implicit fallback.
- **External-only**: add Sonarr/Radarr servers, write routing rules that
  cover everything, and disable streaming (`virtual_library.enabled =
  false`) if you want virtual fully out of the picture.
- **Mixed**: add both, then use Advanced rules to steer (e.g. 4K remuxes to
  Radarr, everything else to Virtual Library). The first matching rule
  decides; **Everything else** catches the rest.

To verify: submit a test request for an obscure released title, watch the
queue's route preview to confirm the chosen backend, and confirm the title
appears in the expected library.

## Troubleshooting

| Symptom | Likely cause / fix |
| --- | --- |
| Requests page says "not available on this server" | `requests_enabled` is off, or the account/group has no request allowance. |
| New submissions fail: "not bound to a plugin installation; re-save it in admin" | Router row predates the plugin install — open the server, re-pick its type, save. |
| "…has no API key; add it in admin" | Paste the *arr API key (from the *arr app's Settings → General), save, Test. |
| Request stuck `approved`, never `queued` | No backend took it: check the route preview, server Enabled toggles, and media-type coverage. Unrouted requests wait for the library instead of failing. |
| Request stuck `queued`/`downloading` | Backend accepted it but hasn't finished: check the *arr queue/activity, root-folder space, and quality-profile availability. |
| Expected Virtual Library but *arr got it | An explicit rule matched first, or the fallback didn't trigger because a (broken) row resolved. Check rule order and row health. |
| Expected *arr but virtual got it | No *arr row resolved for that media type (wrong type flags, disabled, or keyless-but-skipped) and the implicit fallback fired. |
| Season request re-adds the whole series | That backend's router doesn't declare season support — route season gaps to one that does, or to Virtual Library. |
| Watchlist keeps filing requests | Turn off **Request titles added to a watchlist** globally, or per-profile in the profile's own settings. |
| Need internals | Normative design: [Media requests architecture](../../architecture/media-requests.md). Server logs under the `requests` logger carry submission/reconcile errors. |
