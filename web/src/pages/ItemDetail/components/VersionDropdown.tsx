import { memo, useMemo, useState } from "react";
import { Check, ChevronDown, Disc3, Layers3, RefreshCw } from "lucide-react";

import type { FileVersion, PlaybackVariant } from "@/api/types";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { IndexerReleaseList } from "@/components/streaming/IndexerReleaseList";
import { QualityRankingSummary } from "@/components/streaming/QualityRankingSummary";
import { useIndexerReleaseRequests, useVirtualLibraryCapability } from "@/hooks/useIndexerReleases";
import { useWatchDetail } from "@/hooks/queries/items";
import { useVersionListRefresh } from "@/hooks/useVersionListRefresh";
import { useVersionSortPreference } from "@/hooks/useVersionSortPreference";
import { sortVersionsByCriteria, versionSortableFromFile } from "@/lib/qualityRanking";
import { deriveVersionHealth } from "@/lib/versionHealth";
import { videoRangeLabel } from "@/lib/videoRange";
import DetailPopover from "./DetailPopover";
import { sortPlaybackVariantsByEditionPreference } from "./versionRankingUtils";
import {
  audioLanguageLabels,
  formatScoreBadgeLabel,
  formatScoreTitle,
  hasFormatScore,
  profileLabelFromFilePath,
  serverRankingFromVersions,
  subtitleLanguageLabels,
} from "./versionFormatUtils";
import { buildDetailLine, buildQualitySummary, sortByResolution } from "./VersionFlyout";
import { isVersionUnavailable, useVersionVisibility } from "./versionAvailability";

interface VersionDropdownProps {
  versions: FileVersion[];
  playbackVariants?: PlaybackVariant[];
  selectedVersion: FileVersion | null;
  onSelectVersion: (version: FileVersion) => void;
  /**
   * The item content id. When present, opening the picker reads the watch
   * detail so rows carry the custom-format score and the real server ranking,
   * exactly as the in-player menu does. Omitted in isolated tests.
   */
  contentId?: string;
  /** Fired whenever a picker popover opens or closes (open=true on open). */
  onOpenChange?: (open: boolean) => void;
  /**
   * Re-lists the title's video candidates for the version menu. Resolves once
   * the refreshed list has been applied to the page; rejecting keeps the rows
   * already on screen. Omitted when the page cannot refresh.
   */
  onRefreshVersions?: () => Promise<void>;
  /**
   * Cancels the refresh started by `onRefreshVersions` when the control is
   * pressed a second time while the job is still running. Omitted when the
   * surface cannot cancel.
   */
  onCancelRefresh?: () => Promise<void> | void;
}

interface EditionOption {
  id: string;
  label: string;
  variant: PlaybackVariant;
  defaultVersion: FileVersion;
  versions: FileVersion[];
}

function VersionDropdown({
  versions,
  playbackVariants,
  selectedVersion,
  onSelectVersion,
  contentId,
  onOpenChange,
  onRefreshVersions,
  onCancelRefresh,
}: VersionDropdownProps) {
  const [editionOpen, setEditionOpen] = useState(false);
  const [versionOpen, setVersionOpen] = useState(false);
  const {
    refreshing: refreshingVersions,
    cancelable: cancelableVersions,
    error: refreshVersionsError,
    refresh: handleRefreshVersions,
  } = useVersionListRefresh(onRefreshVersions, onCancelRefresh);

  const handleEditionOpenChange = (open: boolean) => {
    setEditionOpen(open);
    onOpenChange?.(open);
  };
  const handleVersionOpenChange = (open: boolean) => {
    setVersionOpen(open);
    onOpenChange?.(open);
  };

  const sorted = useMemo(() => sortByResolution(versions), [versions]);
  const editionOptions = useMemo(
    () => buildEditionOptions(playbackVariants, versions),
    [playbackVariants, versions],
  );

  const hasNamedEditions = editionOptions.some((option) => option.variant.edition_key);
  const showEditionDropdown =
    editionOptions.length > 1 &&
    new Set(editionOptions.map((option) => option.label.toLowerCase())).size > 1 &&
    hasNamedEditions;

  const selectedEdition = showEditionDropdown
    ? resolveSelectedEditionOption(editionOptions, selectedVersion)
    : null;
  const activeVersions = selectedEdition?.versions ?? sorted;
  const activeVersion =
    selectedVersion ?? selectedEdition?.defaultVersion ?? activeVersions[0] ?? null;

  // The viewer's per-profile display order. It only re-orders the list below;
  // the server's auto-pick is untouched.
  const { criteria: userCriteria, apply: applySort, reset: resetSort } = useVersionSortPreference();
  // The catalog item detail carries neither the custom-format score nor the
  // virtual ranking; both live on the watch detail. Read it lazily when the
  // picker opens (the same query the player uses) and merge by file id, so the
  // rows show the score and the ranking the server actually applied.
  const { data: watch } = useWatchDetail(contentId, undefined, undefined, {
    enabled: versionOpen && !!contentId,
  });
  // Indexer releases are read from the same watch detail the picker already
  // loads for the score/ranking. They are gated on the server's capability so a
  // server that cannot request releases shows no indexer UI at all.
  const { indexerRequest } = useVirtualLibraryCapability({ enabled: !!contentId });
  const indexerRequests = useIndexerReleaseRequests(contentId);
  const indexerReleases = indexerRequest ? (watch?.indexer_releases ?? []) : [];
  // A title with nothing but indexer releases still gets the picker, so the
  // viewer can request one even before any playable version exists.
  const showVersionDropdown = activeVersions.length > 1 || indexerReleases.length > 0;
  const serverRanking = useMemo(
    () =>
      serverRankingFromVersions(
        activeVersions.map((version) => ({
          file_path: version.file_path,
          virtual_ranking: watch?.virtual_ranking,
        })),
      ),
    [activeVersions, watch],
  );
  const effectiveCriteria = userCriteria.length > 0 ? userCriteria : serverRanking.criteria;
  const orderedVersions = useMemo(
    () => sortVersionsByCriteria(activeVersions, effectiveCriteria, versionSortableFromFile),
    [activeVersions, effectiveCriteria],
  );
  const watchVersionByFileId = useMemo(() => {
    const versions = new Map<number, FileVersion>();
    for (const version of watch?.versions ?? []) {
      versions.set(version.file_id, version);
    }
    return versions;
  }, [watch]);
  const renderedVersions = useMemo(
    () =>
      orderedVersions.map((version) => {
        const watchVersion = watchVersionByFileId.get(version.file_id);
        // The catalog item detail lacks the custom-format score and may omit
        // the track inventories; both live on the watch detail. Merge per
        // field, falling back to the watch row's tracks, so the rows show the
        // score and language badges the in-player menu shows.
        const score = watchVersion?.format_score;
        const needsScore = score != null && version.format_score == null;
        const needsAudio = !version.audio_tracks?.length && watchVersion?.audio_tracks?.length;
        const needsSubtitle =
          !version.subtitle_tracks?.length && watchVersion?.subtitle_tracks?.length;
        if (!needsScore && !needsAudio && !needsSubtitle) {
          return version;
        }
        return {
          ...version,
          ...(needsScore ? { format_score: score } : {}),
          ...(needsAudio ? { audio_tracks: watchVersion?.audio_tracks } : {}),
          ...(needsSubtitle ? { subtitle_tracks: watchVersion?.subtitle_tracks } : {}),
        };
      }),
    [orderedVersions, watchVersionByFileId],
  );

  const { visibleVersions, hiddenUnavailableCount, setShowUnavailable } = useVersionVisibility(
    renderedVersions,
    activeVersion?.file_id,
  );

  if (!showEditionDropdown && !showVersionDropdown) {
    return null;
  }

  return (
    <>
      {showEditionDropdown && selectedEdition ? (
        <DetailPopover
          open={editionOpen}
          onOpenChange={handleEditionOpenChange}
          contentClassName="w-[30rem] p-1.5"
          trigger={
            <Button
              variant="glass"
              className="h-8 max-w-full min-w-0 shrink gap-1.5 rounded-full px-3 text-xs font-medium"
            >
              <Layers3 className="size-3.5" />
              Edition
              <span className="text-muted-foreground max-w-44 truncate text-[11px] font-normal sm:max-w-64">
                {selectedEdition.label}
              </span>
              <ChevronDown className="text-muted-foreground size-3" />
            </Button>
          }
        >
          <div className="space-y-0.5">
            {editionOptions.map((option) => {
              const isSelected = option.id === selectedEdition.id;
              const detail = buildEditionDetail(option);

              return (
                <button
                  key={option.id}
                  type="button"
                  onClick={() => {
                    onSelectVersion(option.defaultVersion);
                    setEditionOpen(false);
                    setVersionOpen(false);
                  }}
                  className={`flex w-full items-center gap-3 rounded-lg px-3 py-2.5 text-left transition-colors ${
                    isSelected ? "bg-accent text-accent-foreground" : "hover:bg-accent/50"
                  }`}
                >
                  <div className="min-w-0 flex-1">
                    <div className="truncate text-sm font-medium">{option.label}</div>
                    {detail && <div className="text-muted-foreground text-xs">{detail}</div>}
                  </div>
                  {isSelected && <Check className="text-primary size-4 shrink-0" />}
                </button>
              );
            })}
          </div>
        </DetailPopover>
      ) : null}

      {showVersionDropdown ? (
        <DetailPopover
          open={versionOpen}
          onOpenChange={handleVersionOpenChange}
          contentClassName="w-[30rem] p-1.5"
          trigger={
            <Button
              variant="glass"
              className="h-8 max-w-full min-w-0 shrink gap-1.5 rounded-full px-3 text-xs font-medium"
            >
              <Disc3 className="size-3.5" />
              Version
              <span className="text-muted-foreground max-w-44 truncate text-[11px] font-normal sm:max-w-64">
                {activeVersion ? buildVersionTriggerSummary(activeVersion) : ""}
              </span>
              <ChevronDown className="text-muted-foreground size-3" />
            </Button>
          }
        >
          <div className="space-y-0.5">
            <QualityRankingSummary
              serverRanking={serverRanking}
              userCriteria={userCriteria}
              effectiveCriteria={effectiveCriteria}
              onApply={applySort}
              onReset={resetSort}
              className="px-3 pt-1.5 pb-0.5"
            />
            {onRefreshVersions ? (
              <VersionRefreshRow
                refreshing={refreshingVersions}
                cancelable={cancelableVersions}
                error={refreshVersionsError}
                onPress={handleRefreshVersions}
              />
            ) : null}
            {visibleVersions.map((version) => {
              const isSelected = version.file_id === activeVersion?.file_id;
              const summary = buildQualitySummary(version);
              const detail = buildDetailLine(version);
              const rangeLabel = videoRangeLabel(version);
              const unavailable = isVersionUnavailable(version);
              const health = deriveVersionHealth(version);
              const versionProfileLabel = profileLabelFromFilePath(version.file_path);

              return (
                <button
                  key={version.file_id}
                  type="button"
                  onClick={() => {
                    onSelectVersion(version);
                    setVersionOpen(false);
                  }}
                  className={`flex w-full items-center gap-3 rounded-lg px-3 py-2.5 text-left transition-colors ${
                    isSelected ? "bg-accent text-accent-foreground" : "hover:bg-accent/50"
                  } ${unavailable && !isSelected ? "opacity-80" : ""}`}
                >
                  <div className="min-w-0 flex-1">
                    <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
                      <div className="flex items-center gap-2">
                        <span className="text-sm font-medium">{summary || "Video version"}</span>
                        {rangeLabel ? (
                          <Badge variant="secondary" className="px-1.5 py-0 text-[10px] uppercase">
                            {rangeLabel}
                          </Badge>
                        ) : null}
                        {health ? (
                          <Badge
                            variant="outline"
                            title={health.title}
                            className={
                              health.tone === "danger"
                                ? "border-red-500/30 bg-red-500/15 px-1.5 py-0 text-[10px] font-medium text-red-600 dark:text-red-300"
                                : "border-amber-500/30 bg-amber-500/15 px-1.5 py-0 text-[10px] font-medium text-amber-600 dark:text-amber-300"
                            }
                          >
                            {health.label}
                          </Badge>
                        ) : null}
                        {hasFormatScore(version.format_score) ? (
                          <Badge
                            variant="outline"
                            className="text-muted-foreground bg-muted/40 px-1.5 py-0 font-mono text-[10px] font-medium"
                            title={formatScoreTitle(version.format_score, versionProfileLabel)}
                          >
                            {formatScoreBadgeLabel(version.format_score)}
                          </Badge>
                        ) : null}
                      </div>
                      <div className="flex flex-wrap gap-1">
                        {audioLanguageLabels(version.audio_tracks).map((lang) => (
                          <Badge
                            key={lang}
                            variant="outline"
                            className="border-blue-500/20 bg-blue-500/10 px-1 py-0 text-[10px] font-medium text-blue-400"
                          >
                            <span className="mr-0.5 opacity-70">🔊</span>
                            {lang}
                          </Badge>
                        ))}
                      </div>
                    </div>
                    {detail && (
                      <div className="mt-1 flex flex-wrap items-center gap-x-2 gap-y-1">
                        <span className="text-muted-foreground text-xs">{detail}</span>
                        <div className="flex flex-wrap gap-1">
                          {subtitleLanguageLabels(version.subtitle_tracks).map((lang) => (
                            <Badge
                              key={lang}
                              variant="outline"
                              className="border-amber-500/20 bg-amber-500/10 px-1 py-0 text-[10px] font-medium text-amber-400"
                            >
                              <span className="mr-0.5 opacity-70">CC</span>
                              {lang}
                            </Badge>
                          ))}
                        </div>
                      </div>
                    )}
                  </div>
                  {isSelected && <Check className="text-primary size-4 shrink-0" />}
                </button>
              );
            })}
            {hiddenUnavailableCount > 0 && (
              <button
                type="button"
                onClick={() => setShowUnavailable(true)}
                className="text-muted-foreground hover:bg-accent/50 hover:text-foreground w-full rounded-lg px-3 py-2 text-left text-xs font-medium transition-colors"
              >
                Show {hiddenUnavailableCount} unavailable{" "}
                {hiddenUnavailableCount === 1 ? "version" : "versions"}
              </button>
            )}
            {/* Releases that exist on the indexers but are not downloaded on
                the provider. Kept below the playable rows and visually
                secondary (no play affordance). */}
            {indexerReleases.length > 0 && (
              <div className="border-border/60 mt-0.5 border-t pt-0.5">
                <IndexerReleaseList releases={indexerReleases} requests={indexerRequests} />
              </div>
            )}
            {onRefreshVersions ? (
              <VersionRefreshRow
                refreshing={refreshingVersions}
                cancelable={cancelableVersions}
                error={refreshVersionsError}
                onPress={handleRefreshVersions}
              />
            ) : null}
          </div>
        </DetailPopover>
      ) : null}
    </>
  );
}

interface VersionRefreshRowProps {
  refreshing: boolean;
  cancelable: boolean;
  error: string | null;
  onPress: () => void;
}

/**
 * The version picker's "Refresh List" row, rendered both above and below the
 * version list so the control is reachable without scrolling past a long list.
 * Both instances read the same `useVersionListRefresh` state, so one refresh
 * locks and spins both.
 */
function VersionRefreshRow({ refreshing, cancelable, error, onPress }: VersionRefreshRowProps) {
  return (
    <button
      type="button"
      disabled={refreshing && !cancelable}
      aria-busy={refreshing || undefined}
      onClick={onPress}
      className="text-muted-foreground hover:bg-accent/50 hover:text-foreground flex w-full items-center gap-2 rounded-lg px-3 py-2 text-left text-xs font-medium transition-colors disabled:cursor-not-allowed disabled:opacity-60"
    >
      <RefreshCw
        className={`size-3.5 shrink-0 ${refreshing ? "animate-spin" : ""}`}
        aria-hidden="true"
      />
      <span className="flex min-w-0 flex-col">
        <span>{refreshing && cancelable ? "Cancel refresh" : "Refresh List"}</span>
        {error ? <span className="text-destructive text-[10px] leading-tight">{error}</span> : null}
      </span>
    </button>
  );
}

export default memo(VersionDropdown);

function buildVersionTriggerSummary(version: FileVersion): string {
  return buildQualitySummary(version) || buildDetailLine(version) || "Video version";
}

function buildEditionOptions(
  playbackVariants: PlaybackVariant[] | undefined,
  versions: FileVersion[],
): EditionOption[] {
  if (!playbackVariants || playbackVariants.length === 0) {
    return [];
  }

  const orderedVariants = sortPlaybackVariantsByEditionPreference(playbackVariants);
  const hasNamedEditions = orderedVariants.some((variant) => variant.edition_key);

  return orderedVariants
    .map((variant) => {
      const firstPart = [...(variant.parts ?? [])].sort((a, b) => a.part_index - b.part_index)[0];
      if (!firstPart) {
        return null;
      }

      const partVersions = sortByResolution([...(firstPart.versions ?? [])]);
      const defaultVersion =
        (firstPart.default_file_id != null
          ? versions.find((version) => version.file_id === firstPart.default_file_id)
          : undefined) ?? partVersions[0];
      if (!defaultVersion) {
        return null;
      }

      return {
        id: variant.variant_id,
        label: buildEditionLabel(variant, hasNamedEditions),
        variant,
        defaultVersion,
        versions: partVersions,
      };
    })
    .filter((entry): entry is EditionOption => !!entry);
}

function resolveSelectedEditionOption(
  editionOptions: EditionOption[],
  selectedVersion: FileVersion | null,
): EditionOption | null {
  if (editionOptions.length === 0) {
    return null;
  }

  if (selectedVersion) {
    const matching = editionOptions.find((option) =>
      option.variant.parts.some((part) =>
        part.versions.some((candidate) => candidate.file_id === selectedVersion.file_id),
      ),
    );
    if (matching) {
      return matching;
    }
  }

  return editionOptions[0] ?? null;
}

function buildEditionLabel(variant: PlaybackVariant, hasNamedEditions: boolean): string {
  if (variant.edition_raw?.trim()) {
    return variant.edition_raw.trim();
  }
  if (variant.edition_key?.trim()) {
    return humanizeEditionKey(variant.edition_key);
  }
  return hasNamedEditions ? "Standard" : "Edition";
}

function humanizeEditionKey(value: string): string {
  return value
    .split(/[_-]+/)
    .filter(Boolean)
    .map((part) => {
      if (part.toLowerCase() === "imax") {
        return "IMAX";
      }
      return `${part.charAt(0).toUpperCase()}${part.slice(1)}`;
    })
    .join(" ");
}

function buildEditionDetail(option: EditionOption): string {
  const parts: string[] = [];
  if (option.versions.length > 1) {
    parts.push(`${option.versions.length} versions`);
  }

  const defaultSummary = buildQualitySummary(option.defaultVersion);
  if (defaultSummary) {
    parts.push(defaultSummary);
  }

  return parts.join(" · ");
}
