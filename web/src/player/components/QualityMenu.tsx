import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Check, RefreshCw, Settings } from "lucide-react";
import { IndexerReleaseList } from "@/components/streaming/IndexerReleaseList";
import { QualityRankingSummary } from "@/components/streaming/QualityRankingSummary";
import { useIndexerReleaseRequests, useVirtualLibraryCapability } from "@/hooks/useIndexerReleases";
import { useVersionListRefresh, REFRESH_VERSIONS_ERROR } from "@/hooks/useVersionListRefresh";
import { useVersionSortPreference } from "@/hooks/useVersionSortPreference";
import { sortVersionsByCriteria, type VersionSortable } from "@/lib/qualityRanking";
import { resolveActiveQualityOptionId } from "../playback-info";
import { deriveVersionHealth } from "@/lib/versionHealth";
import type { EffectiveRecipeV3 } from "../protocol-v3";
import type { PlayerIndexerRelease, QualityOption } from "../types";
import { PlayerMenuSurface } from "./PlayerMenuSurface";
import {
  formatScoreBadgeLabel,
  formatScoreTitle,
  hasFormatScore,
  serverRankingFromVersions,
} from "@/pages/ItemDetail/components/versionFormatUtils";

export interface VersionInfo {
  fileId: number;
  label: string;
  releaseName?: string;
  /** Release + structured size + source hint, shared with the item-page picker.
   *  Preferred over `releaseName` when present so both menus show the same. */
  detail?: string;
  /** Display-ready audio language labels ("English/French"). */
  audioLanguages?: string[];
  /** Display-ready subtitle language labels ("English"). */
  subtitleLanguages?: string[];
  /** Quality-profile label the candidate was ranked under (`?profile=`). */
  profileLabel?: string | null;
  /** The `?profile=`/`virtual_ranking` source for the list's ranking. */
  filePath?: string;
  virtualRanking?: unknown;
  /** Payload values the display re-order compares. */
  sortable?: VersionSortable;
  /** Custom-format score the server ranked this candidate with. Absent (or
   *  zero) for local and otherwise unscored rows, which show no badge. */
  formatScore?: number;
  isCurrentSource: boolean;
  isRequestedSource: boolean;
  failed?: boolean;
  /** The catalog currently reports this version's source as gone. It stays
   *  selectable because a play can force a re-link and retry it. */
  unavailable?: boolean;
}

interface QualityMenuProps {
  options: QualityOption[];
  activeId: string;
  /** The plan's effective recipe, which shows when a bitrate cap reduced the source. */
  deliveredRecipe?: EffectiveRecipeV3;
  isTranscoding: boolean;
  error: string | null;
  onSelect: (id: string) => void;
  versions?: VersionInfo[];
  /** Releases on the indexers that are not downloaded on the provider yet. */
  indexerReleases?: PlayerIndexerRelease[];
  /** The item id, so the menu can request an indexer release. */
  contentId?: string;
  versionLocked?: boolean;
  onSwitchVersion?: (fileId: number) => void;
  /**
   * Selects automatic version fallback. When provided, the version list shows
   * an Auto row that arms fallback (and recovers a dead source). Omitted in
   * surfaces that cannot toggle it.
   */
  onSelectAutoVersion?: () => void;
  /** Whether automatic version fallback is currently armed. */
  autoFallback?: boolean;
  /**
   * Re-lists the title's video candidates. Resolves once the new list has been
   * applied upstream; rejecting means the list could not be refreshed and the
   * rows already on screen stay as they are.
   */
  onRefreshVersions?: () => Promise<void>;
  /**
   * Cancels the refresh started by `onRefreshVersions` when the row is pressed
   * again while the job is still running. Omitted when the surface cannot
   * cancel.
   */
  onCancelRefresh?: () => Promise<void> | void;
}

export { REFRESH_VERSIONS_ERROR };

export function QualityMenu({
  options,
  activeId,
  deliveredRecipe,
  isTranscoding,
  error,
  onSelect,
  versions,
  indexerReleases = [],
  contentId,
  versionLocked,
  onSwitchVersion,
  onSelectAutoVersion,
  autoFallback,
  onRefreshVersions,
  onCancelRefresh,
}: QualityMenuProps) {
  const [open, setOpen] = useState(false);
  const {
    refreshing: refreshingVersions,
    cancelable: cancelableVersions,
    error: refreshVersionsError,
    refresh: handleRefreshVersions,
  } = useVersionListRefresh(onRefreshVersions, onCancelRefresh);
  // Indexer releases are gated on the server's capability so a server that
  // cannot request releases shows no indexer UI.
  const { indexerRequest } = useVirtualLibraryCapability({ enabled: !!contentId });
  const indexerRequests = useIndexerReleaseRequests(contentId);
  const visibleIndexerReleases = indexerRequest ? indexerReleases : [];
  // The viewer's per-profile display order. It only re-orders the list below;
  // the server's auto-pick is untouched.
  const { criteria: userCriteria, apply: applySort, reset: resetSort } = useVersionSortPreference();
  const serverRanking = useMemo(
    () =>
      serverRankingFromVersions(
        (versions ?? []).map((version) => ({
          file_path: version.filePath,
          virtual_ranking: version.virtualRanking,
        })),
      ),
    [versions],
  );
  const effectiveCriteria = userCriteria.length > 0 ? userCriteria : serverRanking.criteria;
  const orderedVersions = useMemo(
    () =>
      sortVersionsByCriteria(
        versions ?? [],
        effectiveCriteria,
        (version) => version.sortable ?? {},
      ),
    [versions, effectiveCriteria],
  );
  const menuRef = useRef<HTMLDivElement>(null);

  const handleSelect = useCallback(
    (id: string) => {
      onSelect(id);
      setOpen(false);
    },
    [onSelect],
  );

  // Close on outside click.
  const handleBlur = useCallback((e: React.FocusEvent) => {
    if (!menuRef.current?.contains(e.relatedTarget as Node)) {
      setOpen(false);
    }
  }, []);

  // Close on Escape.
  useEffect(() => {
    if (!open) return;
    const handleKeyDown = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        setOpen(false);
      }
    };
    document.addEventListener("keydown", handleKeyDown);
    return () => document.removeEventListener("keydown", handleKeyDown);
  }, [open]);

  const menuItemsRef = useRef<(HTMLButtonElement | null)[]>([]);

  // Stable so React does not detach a row's ref (writing `null` into the
  // roving-focus list) on every re-render.
  const registerMenuRow = useCallback((index: number, el: HTMLButtonElement | null) => {
    menuItemsRef.current[index] = el;
  }, []);

  const handleMenuKeyDown = useCallback((e: React.KeyboardEvent) => {
    const items = menuItemsRef.current.filter(Boolean) as HTMLButtonElement[];
    if (items.length === 0) return;
    const currentIndex = items.indexOf(document.activeElement as HTMLButtonElement);
    let nextIndex: number | null = null;

    switch (e.key) {
      case "ArrowDown":
        nextIndex = currentIndex < items.length - 1 ? currentIndex + 1 : 0;
        break;
      case "ArrowUp":
        nextIndex = currentIndex > 0 ? currentIndex - 1 : items.length - 1;
        break;
      case "Home":
        nextIndex = 0;
        break;
      case "End":
        nextIndex = items.length - 1;
        break;
      case "Escape":
        setOpen(false);
        return;
      default:
        return;
    }
    e.preventDefault();
    items[nextIndex]?.focus();
  }, []);

  if (options.length === 0) return null;

  const resolvedActiveId = resolveActiveQualityOptionId(options, activeId, deliveredRecipe);
  const activeOption = options.find((option) => option.id === resolvedActiveId);
  // Explicit roving-focus slots per group. Mixing a render-time counter with the
  // refresh rows' commit-time `ref` increments let one overwrite another; naming
  // each group's start removes the ambiguity. The refresh action is rendered at
  // both the top and the bottom of the version list, so it owns a slot in each
  // place. Indexer rows are selectable menu items and get slots between the
  // version rows and the bottom refresh action so Arrow Up/Down follow visual
  // order.
  const versionRowsRendered =
    !versionLocked && Boolean(versions && versions.length > 1 && onSwitchVersion);
  const autoRowRendered = versionRowsRendered && Boolean(onSelectAutoVersion);
  const indexerRowsRendered = !versionLocked && visibleIndexerReleases.length > 0;
  // Mirrors the render condition of the Version/Quality header block: the
  // refresh action lives inside it, so it renders only when this is true.
  const menuBlockRendered = versionRowsRendered || indexerRowsRendered;
  const topRefreshRowIndex = 0;
  const autoRowIndex = topRefreshRowIndex + (onRefreshVersions ? 1 : 0);
  const versionRowStart = autoRowIndex + (autoRowRendered ? 1 : 0);
  const indexerRowStart = versionRowStart + (versionRowsRendered ? orderedVersions.length : 0);
  const bottomRefreshRowIndex =
    indexerRowStart + (indexerRowsRendered ? visibleIndexerReleases.length : 0);
  const qualityRowStart = menuBlockRendered
    ? bottomRefreshRowIndex + (onRefreshVersions ? 1 : 0)
    : 0;

  return (
    <div ref={menuRef} className="relative" onBlur={handleBlur}>
      <button
        type="button"
        className="player-quality-trigger player-utility-btn sm:w-auto sm:gap-1.5 sm:px-3"
        onClick={() => setOpen((v) => !v)}
        aria-label="Quality"
        aria-expanded={open}
        aria-haspopup="menu"
      >
        <Settings className="h-[18px] w-[18px]" />
        <span className="hidden text-[11px] font-medium tracking-wide sm:inline">
          {isTranscoding ? "…" : (activeOption?.label ?? "Quality")}
        </span>
      </button>

      {open && (
        <PlayerMenuSurface
          anchorRef={menuRef}
          className="absolute right-0 bottom-full z-30 mb-2 min-w-[200px] rounded-lg bg-black/90 py-1 shadow-lg backdrop-blur"
          onClose={() => setOpen(false)}
          onKeyDown={handleMenuKeyDown}
        >
          {error && <div className="px-3 py-1 text-xs text-red-400">{error}</div>}
          {versionLocked && (
            <p className="max-w-64 px-3 py-2 text-xs text-white/60">
              Watch Party keeps everyone on the same version. You can adjust your streaming quality
              below.
            </p>
          )}
          {/* Version switching (multiple file versions) and indexer releases */}
          {!versionLocked &&
            ((versions && versions.length > 1 && onSwitchVersion) ||
              visibleIndexerReleases.length > 0) && (
              <>
                <div className="px-3 py-1 text-xs tracking-wider text-white/40 uppercase">
                  Version
                </div>
                {onRefreshVersions && (
                  <VersionRefreshRow
                    rowIndex={topRefreshRowIndex}
                    registerRow={registerMenuRow}
                    refreshing={refreshingVersions}
                    cancelable={cancelableVersions}
                    error={refreshVersionsError}
                    onPress={handleRefreshVersions}
                  />
                )}
                {versions && versions.length > 1 && onSwitchVersion && (
                  <>
                    {autoRowRendered && (
                      <button
                        ref={(el) => {
                          menuItemsRef.current[autoRowIndex] = el;
                        }}
                        role="menuitemradio"
                        aria-checked={autoFallback === true}
                        type="button"
                        className={`flex w-full px-3 py-2 text-left text-sm hover:bg-white/10 focus-visible:ring-2 focus-visible:ring-white/70 focus-visible:outline-none ${
                          autoFallback ? "text-white" : "text-white/70"
                        }`}
                        onClick={() => {
                          onSelectAutoVersion?.();
                          setOpen(false);
                        }}
                      >
                        <span className="flex min-w-0 items-center gap-2">
                          <Check
                            className={`h-4 w-4 shrink-0 ${autoFallback ? "opacity-100" : "opacity-0"}`}
                          />
                          <span className="min-w-0">
                            <span className="block truncate">Auto</span>
                            <span className="block truncate text-[11px] text-white/50">
                              Best playable version; fall back if this one dies
                            </span>
                          </span>
                        </span>
                      </button>
                    )}
                    <QualityRankingSummary
                      serverRanking={serverRanking}
                      userCriteria={userCriteria}
                      effectiveCriteria={effectiveCriteria}
                      onApply={applySort}
                      onReset={resetSort}
                      tone="dark"
                      className="px-3 pb-1"
                    />
                    {orderedVersions.map((v, versionIndex) => {
                      const idx = versionRowStart + versionIndex;
                      const statusLabels = buildVersionStatusLabels(v);
                      const health = versionHealthOf(v);
                      const hasScore = hasFormatScore(v.formatScore);
                      // One detail derivation for both lists; `detail` carries the
                      // shared release+size+hint line, and `releaseName` is only a
                      // fallback for callers that supply no detail at all.
                      const detailLine = v.detail || v.releaseName;
                      const audioLanguages = v.audioLanguages ?? [];
                      const subtitleLanguages = v.subtitleLanguages ?? [];
                      const hasBadges =
                        hasScore ||
                        statusLabels.length > 0 ||
                        audioLanguages.length > 0 ||
                        subtitleLanguages.length > 0;
                      return (
                        <button
                          key={v.fileId}
                          ref={(el) => {
                            menuItemsRef.current[idx] = el;
                          }}
                          role="menuitem"
                          type="button"
                          className={`flex w-full px-3 py-2 text-left text-sm hover:bg-white/10 focus-visible:ring-2 focus-visible:ring-white/70 focus-visible:outline-none ${
                            v.isCurrentSource ? "text-white" : "text-white/70"
                          }`}
                          onClick={() => {
                            onSwitchVersion(v.fileId);
                            setOpen(false);
                          }}
                        >
                          <span className="flex min-w-0 items-center gap-2">
                            <span className="min-w-0">
                              <span className="block truncate">{v.label}</span>
                              {detailLine && (
                                <span className="block truncate text-[11px] text-white/50">
                                  {detailLine}
                                </span>
                              )}
                            </span>
                            {hasBadges && (
                              <span className="flex flex-wrap gap-1">
                                {hasScore && (
                                  <span
                                    className="rounded border border-white/15 bg-white/10 px-1.5 py-0.5 text-[10px] leading-none text-white/60"
                                    title={formatScoreTitle(v.formatScore!, v.profileLabel)}
                                  >
                                    {formatScoreBadgeLabel(v.formatScore!)}
                                  </span>
                                )}
                                {statusLabels.map((status) => (
                                  <span
                                    key={status}
                                    title={
                                      health && health.label === status ? health.title : undefined
                                    }
                                    className={`rounded border border-white/15 px-1.5 py-0.5 text-[10px] leading-none ${
                                      health && health.label === status && health.tone === "danger"
                                        ? "border-red-500/30 bg-red-500/20 text-red-400"
                                        : health &&
                                            health.label === status &&
                                            health.tone === "warn"
                                          ? "border-amber-500/30 bg-amber-500/15 text-amber-400"
                                          : "bg-white/10 text-white/70"
                                    }`}
                                  >
                                    {status}
                                  </span>
                                ))}
                                {audioLanguages.map((language) => (
                                  <span
                                    key={`audio-${language}`}
                                    className="rounded border border-blue-500/20 bg-blue-500/10 px-1.5 py-0.5 text-[10px] leading-none text-blue-400"
                                  >
                                    <span className="mr-0.5 opacity-70">🔊</span>
                                    {language}
                                  </span>
                                ))}
                                {subtitleLanguages.map((language) => (
                                  <span
                                    key={`subtitle-${language}`}
                                    className="rounded border border-amber-500/20 bg-amber-500/10 px-1.5 py-0.5 text-[10px] leading-none text-amber-400"
                                  >
                                    <span className="mr-0.5 opacity-70">CC</span>
                                    {language}
                                  </span>
                                ))}
                              </span>
                            )}
                          </span>
                        </button>
                      );
                    })}
                  </>
                )}
                {/* Releases that exist on the indexers but are not downloaded
                    on the provider, below the playable versions. Registered in
                    the menu's roving-focus list so Arrow keys reach them. */}
                {visibleIndexerReleases.length > 0 && (
                  <IndexerReleaseList
                    releases={visibleIndexerReleases}
                    requests={indexerRequests}
                    tone="dark"
                    rowRole="menuitem"
                    registerRow={registerMenuRow}
                    rowIndexStart={indexerRowStart}
                    className="border-t border-white/10 pt-0.5"
                  />
                )}
                {onRefreshVersions && (
                  <VersionRefreshRow
                    rowIndex={bottomRefreshRowIndex}
                    registerRow={registerMenuRow}
                    refreshing={refreshingVersions}
                    cancelable={cancelableVersions}
                    error={refreshVersionsError}
                    onPress={handleRefreshVersions}
                  />
                )}
                <div className="my-1 border-t border-white/10" />
                <div className="px-3 py-1 text-xs tracking-wider text-white/40 uppercase">
                  Quality
                </div>
              </>
            )}
          {options.map((opt, optionIndex) => {
            const idx = qualityRowStart + optionIndex;
            return (
              <button
                key={opt.id}
                ref={(el) => {
                  menuItemsRef.current[idx] = el;
                }}
                role="menuitem"
                type="button"
                className={`flex w-full items-center justify-between px-3 py-2 text-left text-sm hover:bg-white/10 focus-visible:ring-2 focus-visible:ring-white/70 focus-visible:outline-none ${
                  opt.id === resolvedActiveId ? "text-white" : "text-white/70"
                }`}
                aria-current={opt.id === resolvedActiveId ? "true" : undefined}
                onClick={() => handleSelect(opt.id)}
              >
                <span>{opt.label}</span>
                <span className="flex items-center gap-2">
                  <span className="text-xs text-white/40">{opt.sublabel}</span>
                  {opt.id === resolvedActiveId && (
                    <span className="flex size-4 shrink-0 items-center justify-center rounded-full bg-white text-black">
                      <Check className="size-3" strokeWidth={3} aria-hidden="true" />
                      <span className="sr-only">Selected</span>
                    </span>
                  )}
                </span>
              </button>
            );
          })}
        </PlayerMenuSurface>
      )}
    </div>
  );
}

export function buildVersionStatusLabels(version: VersionInfo): string[] {
  const labels: string[] = [];
  if (version.isCurrentSource) {
    labels.push("Playing");
  }
  if (version.isRequestedSource && !version.isCurrentSource) {
    labels.push("Requested");
  }
  // One health badge, chosen by the server: a failed candidate takes precedence
  // over the liveness verdict. Both come from server-published state only.
  const health = versionHealthOf(version);
  if (health) {
    labels.push(health.label);
  }
  return labels;
}

/**
 * Adapts the menu's `VersionInfo` onto the shared health derivation. The menu
 * models the liveness verdict as `unavailable` (the inverse of the server's
 * `available`), so map it back without inventing a second server flag.
 */
function versionHealthOf(version: VersionInfo) {
  return deriveVersionHealth({
    failed: version.failed,
    available: version.unavailable === true ? false : undefined,
  });
}

interface VersionRefreshRowProps {
  rowIndex: number;
  registerRow: (index: number, el: HTMLButtonElement | null) => void;
  refreshing: boolean;
  cancelable: boolean;
  error: string | null;
  onPress: () => void;
}

/**
 * The player menu's "Refresh List" row, rendered both above and below the
 * version list so the control is reachable without scrolling past a long list.
 * Both instances read the same `useVersionListRefresh` state, so one refresh
 * locks and spins both, and each has its own roving-focus slot.
 */
function VersionRefreshRow({
  rowIndex,
  registerRow,
  refreshing,
  cancelable,
  error,
  onPress,
}: VersionRefreshRowProps) {
  return (
    <button
      ref={(el) => {
        registerRow(rowIndex, el);
      }}
      role="menuitem"
      type="button"
      className="flex w-full items-center gap-2 px-3 py-2 text-left text-sm text-white/70 hover:bg-white/10 focus-visible:ring-2 focus-visible:ring-white/70 focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-60"
      disabled={refreshing && !cancelable}
      aria-busy={refreshing || undefined}
      onClick={onPress}
    >
      {refreshing ? (
        <span
          className="h-3.5 w-3.5 shrink-0 animate-spin rounded-full border-2 border-white/30 border-t-white"
          aria-hidden="true"
        />
      ) : (
        <RefreshCw className="h-3.5 w-3.5 shrink-0 text-white/50" aria-hidden="true" />
      )}
      <span className="flex min-w-0 flex-col">
        <span>{refreshing && cancelable ? "Cancel refresh" : "Refresh List"}</span>
        {error && <span className="text-[11px] leading-tight text-red-400">{error}</span>}
      </span>
    </button>
  );
}
