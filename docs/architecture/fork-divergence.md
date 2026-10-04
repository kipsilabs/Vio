# Fork divergence (Vio)

This repo tracks upstream `Silo-Server/silo-server` with deliberate Vio
divergences. Read this before merging upstream — an upstream change that looks
innocent can silently undo one of these. The merge procedure below plus
`scripts/check-fork-invariants.sh` exist so both humans and AI agents catch
that before it ships.

## Owned divergences

| Area | Divergence | Key files | Pinned by |
| --- | --- | --- | --- |
| Client identity | Web client identifies as `Vio Web` (dual `X-Vio-*` / `X-Silo-*` headers); telemetry labels `vio *` alongside `silo *` | `web/src/api/v2/request.ts`, `web/src/api/client.ts`, `web/src/lib/plexAuth.ts`, `internal/apiv2/observe.go` | `request.test.ts`, `observe_test.go`, invariant script |
| Virtual routing | All virtual-library traffic resolves through core (`internal/virtuallibrary`); no virtual→plugin dispatch anywhere | `internal/api/router.go`, `cmd/silo/main.go`, `internal/catalog/item_repo.go`, `internal/catalog/library_collection_repo.go` | catalog integration tests, `router_virtual_library_core_test.go`, invariant script |
| Virtual network permissions | `virtual_library.allow_insecure_http` covers HTTP manifests on private/local hosts only; `virtual_library.allow_private_streams` separately gates private stream destinations (SSRF bypass on resolved URLs, relay transport, redirects, probes). Both default false; enabling the former does not imply the latter | `internal/virtuallibrary/service.go`, `internal/virtuallibrary/resolve.go`, `internal/api/router.go`, `internal/plugins/virtual_provenance.go`, `cmd/silo/main.go`, `web/src/pages/admin-settings/StreamingSettings.tsx` | `resolve_test.go` boundary cases, `split_allow_private_streams_migration_test.go` |
| Request integrations | `installation_id` 0 (core virtual router) validates; virtual-dormant saves answer 422, not 500 | `internal/apiv2/admin_requests.go`, `internal/requests/service.go` | `service_test.go` |
| Collection sync | Missing virtual provider answers 503, not 500 | `internal/apiv2/admin_collections.go` | `admin_collections_test.go` |
| Onboarding | Requests step, TMDB/TVDB auto-install section, format scoring card | `web/src/pages/setup-wizard/` | step tests |
| Collections | `virtual_playback` defaults on when omitted at the collection-creation API boundary; add and template forms default on | `internal/api/handlers/library_collections.go`, `internal/api/handlers/admin_collections_service.go`, `internal/adminjob/template_bundle_apply.go`, `web/src/pages/adminCollectionsShared.tsx`, `web/src/components/CollectionTemplateGallery/`, `web/src/pages/AdminRequests.tsx` | `internal/api/handlers/library_collections_test.go`, `internal/adminjob/template_bundle_apply_test.go`, `scripts/check-fork-invariants.sh`, step tests |
| Settings secrets | Indexer keys use `SecretField` (configured indicator + clear) | `web/src/pages/admin-settings/StreamingSettings.tsx` | `StreamingSettings.test.tsx` |
| Settings checks | `remuxdb` + `virtual_library` check kinds; safe messages | `internal/api/handlers/admin_settings_checks*.go` | `admin_settings_check_service_test.go` |
| Monitor registrar | Library-scoped registration, episode URIs, plugin-path fallbacks | `internal/virtuallibrary/registrar.go`, `monitor/monitor.go` | virtuallibrary suites |
| Plugin SDK | Upstream SDK v0.21.0, no replacement; fork virtual plugin RPCs retired; virtual registration/resolution remain core-owned (`internal/virtuallibrary`, `internal/catalog/virtual_media.go`); network-access provider support follows upstream. | `go.mod`, `go.sum` | requests suites |
| V3 transcode planner | Vio's ladder/HEVC/output-check planner (`targetVideoSelectionV3`, `CappedRungHeightV3` shared with the virtual picker, QSV `-1` width, 2160p min-clamp, 420p/328p rungs) instead of upstream's shared-ladder rework (#1659: `bitrateLadder`, decoder-bounded H264, generalized `scaleTargetHeight` switches). Portable #1659 pieces taken: `scaleTargetHeight`/`vaapiNV12Filter` helpers, `softwareEncode` rename, server-cap audio reserve, `anchorRef` menu wiring, exact ladder-fit heights (`800p` bubbles, `heightLabel`) in the quality resolver. The hardware scalers (`vaapiScaleFilter`, `qsvScaleFilterWithMapMode`, `qsvVPPInputScaleFilter`) scale exact heights in their defaults — a plan/filer mismatch here encodes at source size while the plan advertises the fit, so any future filter must parse `scaleTargetHeight` too. Removed #1659 planner-behavior tests are replaced by Vio equivalents: `TestHardwareScaleFiltersShareExactLadderHeights`, `TestScopeFilmAutoPlanScalesOnHardwareFilters` plus the retained HEVC fallback/decoder tests in `hevc_transcode_v3_test.go`. | `internal/playback/plan_v3.go`, `internal/playback/transcode.go`, `internal/api/handlers/playback_virtual.go` | playback suites (Vio ladder/HEVC/output-check tests) |
| Audio selection | MULTi track awareness (`track.Languages`, `trackHasLanguage`, `trackLanguageRank`), cross-version language preservation, and fine-grained regional/script discrimination | `internal/playback/audio_select.go`, `internal/lang/lang.go` | `internal/playback/audio_select_test.go`, `internal/api/handlers/playback_virtual_languages_test.go`, `internal/jellycompat/jelly_gap_test.go` |
| Image builds | Decoupled `node-base` stage for BuildKit frontend pruning, unshadowed Go module layer caching, persisted Go compiler cache (`buildkit-cache-dance`), deduplicated `workflow_dispatch` frontend builds, base images pinned to patch/dated tags, skipped image attestations, and publishing default-branch images as `:dev` with `:latest` reserved for stable releases or manual promotion | `Dockerfile`, `.github/workflows/docker.yml`, `docker-compose.yml`, `.env.example` | invariant script |
| Upstream non-goals | Not adopted: the fork may explore remote/debrid-backed library integrations (Live TV/IPTV/DVR, `.strm`-style remote-URL playback) that upstream permanently rejects. Upstream non-goals changes must not silently reimpose scope here; the Apple/Google store risk stands and any shipped integration owns it | `AGENTS.md` (Non-goals section) | merge-procedure review |
| Virtual transcode worker dispatch | Virtual library sources (`virtual://`) resolve to pre-resolved stream relay URLs (`/source/<token>/...`) and dispatch to pooled remote transcode worker nodes for video transcoding, while direct play and progressive remux remain restricted to the integrated API path. The transcode node path authorizer permits approved relay stream URLs | `internal/api/handlers/playback_v3.go`, `internal/api/handlers/playback_transport.go`, `internal/jellycompat/virtual_playback.go`, `internal/jellycompat/streams.go`, `internal/transcodenode/path_authorizer.go` | transcodenode, jellycompat, and api handlers playback suites |

## Merge procedure

1. `git fetch upstream` and review every commit: subjects plus `git show
   --stat` for each. Pay special attention to dropped or renamed operations,
   parameters, settings keys, and anything touching the files above.
2. Red flags that need a decision, not a blind resolve:
   - A removed API operation: grep `web/src` for its operation ID — if the
     fork's UI calls it, keep calling code working (adapt or vendor a
     replacement) before merging.
   - New hunks in `internal/api/router.go` or `cmd/silo/main.go` near virtual
     dispatch: upstream may reintroduce plugin fallbacks. The invariant script
     catches callers; keep routing core-only.
   - `internal/config/admin_settings.go` changes: check for key renames or
     removed fork-used keys.
   - `internal/playback/audio_select.go` changes: upstream lacks MULTi track
     awareness (`track.Languages`, `trackHasLanguage`) and fine-grained
     regional/script discrimination. Do not adopt upstream `audio_select.go`
     wholesale.
   - `contracts/api/v2/*` and `web/src/api/v2/{operations,schema}.ts` churn:
     expected when upstream changes the contract. These are generated —
     resolve by regenerating (`make apiv2-*`), never by hand-editing.
   - `Dockerfile`, `docker-compose.yml`, `.env.example`, and `.github/workflows/docker.yml` hunks: upstream lacks the
     decoupled `node-base` stage (which enables BuildKit frontend pruning when
     prebuilt assets are injected), shadows Go module layers with cache mounts,
     lacks `buildkit-cache-dance` to persist Go compiler caches, duplicates
     manual frontend builds across runners, and continuously overwrites the `:latest`
     tag on every main branch commit. Do not let upstream merges
     overwrite Vio's optimized build pipeline or revert `:dev` defaults to `:latest`. Note that `.github/workflows/docker.yml`
     is preserved automatically via `.gitattributes` (`merge=ours`, configured by
     `make install-hooks`), while `Dockerfile`, `docker-compose.yml`, and `.env.example` hunks must be reviewed to keep
     the decoupled `node-base` stage, unshadowed module layer caching, the
     pinned base image versions, and the `:dev` image tags. Floating base image tags (`node:22-slim`,
      `golang:1.26`, `debian:trixie-slim`) bump silently and invalidate the
      layer cache and the Go build cache, so the fork pins them to patch/dated
      tags and bumps them as a deliberate change. CI and
      `make verify-fork-invariants` enforce both.
3. Merge with a merge commit so fork history stays readable. Never rebase
   pushed history.
4. Verify before pushing: `scripts/check-fork-invariants.sh` (includes gofmt
   and prettier gates — the repo pre-commit hook does not check formatting),
   `go build ./...`, the focused suites for touched areas (`internal/apiv2`
   including the fixtures/document tests that pin the contract,
   `internal/catalog` virtual suites against a disposable DB, `internal/api`
   router tests, web typecheck plus touched step tests).
5. Push. The image pipeline builds from the merge.
