import { useState, useCallback, useEffect, useRef, useMemo } from "react";
import { createPortal } from "react-dom";
import {
  Captions,
  CaptionsOff,
  Check,
  Languages,
  Minus,
  Plus,
  SlidersHorizontal,
} from "lucide-react";
import { useT, type MessageKey } from "@/i18n";
import type { PlayerAudioTrack, PlayerSubtitleInfo } from "../types";
import type { PlayerConfig } from "../context/PlayerConfigContext";
import { SubtitleSearchModal } from "./SubtitleSearchModal";
import { SubtitleTranslateModal } from "./SubtitleTranslateModal";
import { SubtitleAppearancePanel } from "./SubtitleAppearancePanel";
import { playerV2 } from "../player-v2";
import { getLanguageName } from "../utils/languageNames";
import { sortSubtitlesBySource } from "../utils/subtitleSort";
import { getSubtitleFormatLabel, isSubtitleFormatLabel } from "../utils/subtitleCodecs";
import { dedupeSubtitleTracks } from "../utils/trackDedupe";
import { isTranslatableSource } from "./subtitleTranslateRequest";
import { PlayerMenuSurface } from "./PlayerMenuSurface";
import { ProvisionalTrackBadge } from "./ProvisionalTrackBadge";
import { DiscoveryRetryButton } from "./DiscoveryRetryButton";
import { TrackBadge } from "./TrackBadge";
import { useProvisionalInventoryExpiry } from "../hooks/useProvisionalInventoryExpiry";
import type { SubtitleSync, SubtitleSyncEntry } from "../hooks/useSubtitleSync";
import {
  describeTiming,
  isIdentityTiming,
  isSyncInProgress,
  syncFailureMessage,
  syncKeyOf,
  syncPhaseLabel,
  syncProgressPercent,
  syncStatusLabel,
} from "../utils/subtitleSync";

interface SubtitleMenuProps {
  tracks: PlayerSubtitleInfo[];
  activeIndex: number | null;
  onSelect: (index: number | null) => void;
  delayMs: number;
  onDelayChange: (ms: number) => void;
  preferredSubtitleLanguage?: string | null;
  mediaFileId?: number;
  playerConfig?: PlayerConfig;
  onRefreshSubtitles?: () => void;
  onSubtitleJobAccepted?: (jobId: string, resultSubtitleId?: string) => void;
  sessionId?: string;
  getSubtitleStartPosition?: () => number;
  audioTracks?: PlayerAudioTrack[];
  /**
   * True while the session is replacing its plan or replanning, when the
   * rendered inventory belongs to the outgoing plan. Keeps the menu shut so a
   * pick cannot name a stale slot.
   */
  locked?: boolean;
  /**
   * True while the server's inventory is declared metadata rather than probe
   * evidence, so the menu says so instead of presenting every row as final.
   */
  provisional?: boolean;
  /** True while the deferred track enumeration is still running. */
  pending?: boolean;
  /** True when track discovery terminally failed or timed out. */
  failed?: boolean;
  /**
   * Re-runs track discovery. Shown as a retry once the provisional window
   * lapses or discovery fails, so a stuck menu is actionable.
   */
  onRetryDiscovery?: () => void;
  /** Timing and sync state of the file's syncable tracks (stored and sidecar files). */
  subtitleSync?: SubtitleSync;
}

const DELAY_STEP_MS = 100;
const DELAY_MAX_MS = 10_000;

const SOURCE_LABELS: Record<string, MessageKey> = {
  external: "web.subtitles.menu.source.external",
  embedded: "web.subtitles.menu.source.embedded",
  downloaded: "web.subtitles.menu.source.downloaded",
};

/** The signed delay number the "{delay} ms" message shows. */
function formatDelay(ms: number): string {
  if (ms === 0) return "0";
  const sign = ms > 0 ? "+" : "−";
  return `${sign}${Math.abs(ms)}`;
}

export function SubtitleMenu({
  tracks,
  activeIndex,
  onSelect,
  delayMs,
  onDelayChange,
  preferredSubtitleLanguage,
  mediaFileId,
  playerConfig,
  onRefreshSubtitles,
  onSubtitleJobAccepted,
  sessionId,
  getSubtitleStartPosition,
  audioTracks,
  locked = false,
  provisional = false,
  pending = false,
  failed = false,
  onRetryDiscovery,
  subtitleSync,
}: SubtitleMenuProps) {
  const t = useT();
  const [open, setOpen] = useState(false);
  // A replace or replan swaps the inventory out from under the menu. The open
  // state is kept so the menu returns once the replacement lands, but the
  // trigger and surface stay shut until then so a pick cannot name a slot the
  // outgoing plan owned.
  const menuOpen = open && !locked;
  const [searchOpen, setSearchOpen] = useState(false);
  const [translateOpen, setTranslateOpen] = useState(false);
  const [aiEnabled, setAiEnabled] = useState(false);
  const [aiTranscribeEnabled, setAiTranscribeEnabled] = useState(false);
  const [onlineSearchEnabled, setOnlineSearchEnabled] = useState(true);
  const [appearanceOpen, setAppearanceOpen] = useState(false);
  const menuRef = useRef<HTMLDivElement>(null);

<<<<<<< HEAD
  // Probed inventories can repeat the same subtitle stream at several container
  // indexes; the menu shows one row per distinct descriptor, keeping the first
  // ordinal for selection. Differing forced/hearing-impaired/flag rows stay.
  const sortedTracks = useMemo(() => dedupeSubtitleTracks(sortSubtitlesBySource(tracks)), [tracks]);
  // Declared or pending rows are placeholders for the effective file, so they
  // stay out of the menu until probe evidence lands. A terminal failure keeps
  // the list under its explicit "discovery did not complete" notice instead of
  // leaving the menu empty.
  const displayedTracks = provisional && !failed ? [] : sortedTracks;
  // A spinner with no bound is an indefinite lie about progress. Once the
  // provisional window lapses, the menu offers the same retry a terminal
  // failure does instead of spinning forever.
  const { expired: discoveryExpired, retry: restartDiscovery } =
    useProvisionalInventoryExpiry(provisional);
  const handleRetryDiscovery = useCallback(() => {
    restartDiscovery();
    onRetryDiscovery?.();
  }, [onRetryDiscovery, restartDiscovery]);
=======
  const [pendingDownload, setPendingDownload] = useState<{
    id: string;
    generation: number;
  } | null>(null);
  const downloadGeneration = useRef(0);
  const downloadSelectionGeneration = useRef(0);
  const profileId = playerConfig?.getProfileId();
  const profileToken = playerConfig?.getProfileToken?.();
  const authContext = playerConfig?.getAuthContext?.();

  // A completed download may precede its inventory update. Never carry that
  // selection into another viewing or over a newer subtitle choice.
  useEffect(() => {
    downloadGeneration.current++;
    // eslint-disable-next-line react-hooks/set-state-in-effect -- a new viewing or subtitle choice drops a download still pending from the old one
    setPendingDownload(null);
  }, [mediaFileId, sessionId, playerConfig, profileId, profileToken, authContext, activeIndex]);

  useEffect(() => {
    if (!pendingDownload || pendingDownload.generation !== downloadGeneration.current) return;
    const track = tracks.find((candidate) =>
      candidate.stored_subtitle_id
        ? String(candidate.stored_subtitle_id) === pendingDownload.id
        : syncKeyOf(candidate) === `stored-${pendingDownload.id}`,
    );
    if (!track) return;
    // eslint-disable-next-line react-hooks/set-state-in-effect -- the download's track exists only once the inventory refresh lands
    setPendingDownload(null);
    onSelect(track.index);
  }, [pendingDownload, tracks, onSelect]);

  const sortedTracks = useMemo(() => sortSubtitlesBySource(tracks), [tracks]);
>>>>>>> upstream/main

  // Discover whether the server has AI subtitle translation configured, so we
  // only surface the entry point when it can actually do something. This is a
  // server-wide capability, so we fetch it once per session (keyed on the stable
  // playerConfig) rather than re-checking on every file change.
  useEffect(() => {
    if (!playerConfig) return;
    let cancelled = false;
    playerV2(playerConfig, "GET /api/v2/subtitles/ai/status", {})
      .then((res) => {
        if (cancelled) return;
        setAiEnabled(Boolean(res?.enabled));
        setAiTranscribeEnabled(Boolean(res?.transcribe_enabled));
      })
      .catch(() => {
        if (cancelled) return;
        setAiEnabled(false);
        setAiTranscribeEnabled(false);
      });
    return () => {
      cancelled = true;
    };
  }, [playerConfig]);

  // Online subtitle search is likewise a server-wide capability. Unlike AI this
  // fails open: only an explicit enabled:false hides it, so older servers and
  // probe failures keep today's behavior. Manual upload never depends on it.
  useEffect(() => {
    if (!playerConfig) return;
    // A previous server's answer must not hide search on this one.
    // eslint-disable-next-line react-hooks/set-state-in-effect -- a new server starts from the fail-open default until its probe answers
    setOnlineSearchEnabled(true);
    let cancelled = false;
    playerV2(playerConfig, "GET /api/v2/subtitles/providers/status", {})
      .then((res) => {
        if (cancelled) return;
        setOnlineSearchEnabled(res?.enabled !== false);
      })
      .catch(() => {
        if (cancelled) return;
        setOnlineSearchEnabled(true);
      });
    return () => {
      cancelled = true;
    };
  }, [playerConfig]);

  const clampedDelay = useCallback(
    (ms: number) => Math.max(-DELAY_MAX_MS, Math.min(DELAY_MAX_MS, ms)),
    [],
  );
  const nudgeDelay = useCallback(
    (deltaMs: number) => onDelayChange(clampedDelay(delayMs + deltaMs)),
    [delayMs, onDelayChange, clampedDelay],
  );
  const resetDelay = useCallback(() => onDelayChange(0), [onDelayChange]);

  const delayDisabled = activeIndex === null;

  const syncEntryOf = (track: PlayerSubtitleInfo | null | undefined) => {
    const key = syncKeyOf(track);
    return key ? subtitleSync?.entries[key] : undefined;
  };
  const activeSyncEntry = syncEntryOf(
    activeIndex !== null ? tracks.find((track) => track.index === activeIndex) : null,
  );

  const handleSelect = useCallback(
    (index: number | null) => {
<<<<<<< HEAD
      if (locked) return;
=======
      setPendingDownload(null);
>>>>>>> upstream/main
      onSelect(index);
      setOpen(false);
    },
    [locked, onSelect],
  );

  const handleBlur = useCallback((e: React.FocusEvent) => {
    if (!menuRef.current?.contains(e.relatedTarget as Node)) {
      setOpen(false);
    }
  }, []);

<<<<<<< HEAD
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
  const retryButtonRef = useRef<HTMLButtonElement | null>(null);

  const handleMenuKeyDown = useCallback((e: React.KeyboardEvent) => {
    const retryBtn = retryButtonRef.current;
    const trackButtons = menuItemsRef.current.filter(Boolean) as HTMLButtonElement[];
    const items = (
      retryBtn && !retryBtn.disabled ? [retryBtn, ...trackButtons] : trackButtons
    ).filter((btn) => !btn.disabled);
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

  if (tracks.length === 0 && !mediaFileId && !provisional && !pending && !failed && !menuOpen)
    return null;
=======
  if (tracks.length === 0 && !mediaFileId) return null;
>>>>>>> upstream/main

  const canAddSubtitles = Boolean(mediaFileId && playerConfig);
  // The rail names the subtitles shown ("English", "Off") where it has room.
  const activeTrack =
    activeIndex !== null ? tracks.find((track) => track.index === activeIndex) : null;
  const valueLabel = activeTrack
    ? getLanguageName(activeTrack.language)
    : t("web.subtitles.menu.off");
  const canTranslate =
    canAddSubtitles &&
    ((aiEnabled && tracks.some(isTranslatableSource)) ||
      (aiTranscribeEnabled && (audioTracks?.length ?? 0) > 0));

  return (
    <div ref={menuRef} className="relative" onBlur={handleBlur}>
      <button
        type="button"
<<<<<<< HEAD
        className="player-utility-btn disabled:cursor-not-allowed disabled:opacity-40"
=======
        className="hud-icon-btn hud-rail-btn"
>>>>>>> upstream/main
        data-active={activeIndex !== null ? "true" : "false"}
        onClick={() => {
          // Opening re-reads sync state so a job that finished meanwhile shows.
          if (!open) subtitleSync?.reload();
          setOpen((v) => !v);
        }}
<<<<<<< HEAD
        aria-label={activeIndex !== null ? "Disable captions" : "Enable captions"}
        aria-expanded={menuOpen}
=======
        aria-label={t("web.subtitles.menu.triggerWithValue", { value: valueLabel })}
        title={
          activeIndex !== null
            ? t("web.subtitles.menu.subtitlesShortcut")
            : t("web.subtitles.menu.subtitlesOffShortcut")
        }
        aria-expanded={open}
>>>>>>> upstream/main
        aria-haspopup="menu"
        aria-disabled={locked || undefined}
        disabled={locked}
      >
        {activeIndex !== null ? <Captions /> : <CaptionsOff />}
        <span className="hud-rail-label">{valueLabel}</span>
      </button>

      {menuOpen && (
        <PlayerMenuSurface
          title={t("web.subtitles.menu.heading")}
          role="dialog"
          anchorRef={menuRef}
          className="hud-popover w-[24rem] max-w-[calc(100vw-2rem)]"
          onClose={() => setOpen(false)}
        >
<<<<<<< HEAD
          <div className="flex shrink-0 items-center gap-2 py-1 pr-3">
=======
          <div
            role="menu"
            aria-label={t("web.subtitles.menu.tracksLabel")}
            className="hud-scroll-list max-h-[min(22.5rem,calc(100cqh-23rem))] min-h-[7.5rem] shrink"
          >
>>>>>>> upstream/main
            <button
              role="menuitemradio"
              type="button"
<<<<<<< HEAD
              className={`flex flex-1 items-center gap-2 px-3 py-2 text-left text-sm hover:bg-white/10 focus-visible:ring-2 focus-visible:ring-white/70 focus-visible:outline-none ${
                activeIndex === null ? "bg-white/5 text-white" : "text-white/70"
              }`}
=======
              aria-checked={activeIndex === null}
              className="hud-item"
>>>>>>> upstream/main
              onClick={() => handleSelect(null)}
            >
              <span aria-hidden="true" className="hud-check">
                {activeIndex === null ? <Check /> : null}
              </span>
              {t("web.subtitles.menu.off")}
            </button>
<<<<<<< HEAD
            {provisional && <ProvisionalTrackBadge />}
          </div>
          <div className="max-h-[60vh] overflow-y-auto py-1">
            {/* The failure notice only qualifies a still-unverified list. A menu
                that already renders confident probe evidence (verified push or a
                catalog/plan list) is not a failed discovery, so a timed-out poll
                must never draw "discovery did not complete" above a correct,
                populated list. `provisional` is the menu's own evidence state. */}
            {failed && provisional && sortedTracks.length === 0 && (
              <div className="flex items-center justify-between gap-2 px-3 py-1 text-xs text-amber-300/80">
                <span>Subtitle discovery failed</span>
                <DiscoveryRetryButton onRetry={handleRetryDiscovery} buttonRef={retryButtonRef} />
              </div>
            )}
            {failed && provisional && sortedTracks.length > 0 && (
              <div className="flex items-center justify-between gap-2 px-3 py-1 text-xs text-white/50">
                <span>Showing available subtitles — discovery did not complete</span>
                <DiscoveryRetryButton onRetry={handleRetryDiscovery} buttonRef={retryButtonRef} />
              </div>
            )}
            {!failed && provisional && !discoveryExpired && (
              <div className="flex items-center gap-2 px-3 py-1 text-xs text-white/50">
                <span className="h-3 w-3 animate-spin rounded-full border border-white/20 border-t-white/80" />
                <span>Discovering subtitles…</span>
              </div>
            )}
            {!failed && provisional && discoveryExpired && (
              <div className="flex items-center justify-between gap-2 px-3 py-1 text-xs text-white/50">
                <span>Subtitles unavailable.</span>
                <DiscoveryRetryButton onRetry={handleRetryDiscovery} buttonRef={retryButtonRef} />
              </div>
            )}
            {sortedTracks.length === 0 && !provisional && (
              <div className="px-3 py-2 text-xs text-white/40">No subtitles available</div>
            )}
            {displayedTracks.map((track) => {
=======
            {sortedTracks.map((track) => {
>>>>>>> upstream/main
              const isActive = track.index === activeIndex;
              const languageName = getLanguageName(track.language);
              const source = track.source ?? "embedded";
              const detail =
                track.label &&
                track.label !== track.language &&
                track.label !== languageName &&
                !isSubtitleFormatLabel(track.label, track.codec)
                  ? track.label
                  : null;
              const badges = [
                getSubtitleFormatLabel(track.codec),
                t(SOURCE_LABELS[source] ?? "web.subtitles.menu.source.embedded"),
              ]
                .filter(Boolean)
                .join(" · ");
              const syncEntry = syncEntryOf(track);
              const syncLabel = syncEntry ? syncStatusLabel(syncEntry.state) : null;

              return (
                <button
                  key={track.index}
                  role="menuitemradio"
                  type="button"
                  aria-checked={isActive}
                  className="hud-item"
                  onClick={() => handleSelect(track.index)}
                >
                  <span aria-hidden="true" className="hud-check">
                    {isActive ? <Check /> : null}
                  </span>
<<<<<<< HEAD
                  <span className="flex min-w-0 flex-1 flex-col">
                    <span className="flex w-full items-center justify-between gap-3">
                      <span className="flex min-w-0 items-center gap-1.5">
                        <span className="truncate text-sm">{languageName}</span>
                        {track.default && <TrackBadge variant="outline">Default</TrackBadge>}
                      </span>
                      <span className="flex shrink-0 items-center gap-1">
                        {formatLabel && (
                          <span className="rounded bg-white/10 px-1.5 py-0.5 text-[10px] tracking-wide text-white/50 uppercase">
                            {formatLabel}
                          </span>
                        )}
                        <span className="rounded bg-white/10 px-1.5 py-0.5 text-[10px] tracking-wide text-white/50 uppercase">
                          {sourceLabel}
                        </span>
                      </span>
=======
                  <span className="flex min-w-0 flex-1 flex-col gap-0.5">
                    {/* The title gets its own line: it can be a long file name,
                        and truncating it must not hide the format and source. */}
                    <span className="flex min-w-0 items-baseline justify-between gap-3">
                      <span className="min-w-0 truncate">{languageName}</span>
                      <span className="hud-item-secondary shrink-0">{badges}</span>
>>>>>>> upstream/main
                    </span>
                    {detail && (
                      <span className="hud-item-secondary truncate" title={detail}>
                        {detail}
                      </span>
                    )}
                    {syncLabel && (
                      <span
                        className="hud-item-secondary truncate"
                        title={syncLabel}
                        data-testid="subtitle-sync-status"
                      >
                        {syncLabel}
                      </span>
                    )}
                  </span>
                </button>
              );
            })}
          </div>
          <div className="hud-panel-divider" />
          <div className="flex shrink-0 items-center justify-between gap-3 px-4 py-1">
            <span className="text-sm">{t("web.subtitles.menu.delay")}</span>
            <div className="flex items-center gap-1.5">
              <button
                type="button"
                className="hud-mini-btn"
                onClick={() => nudgeDelay(-DELAY_STEP_MS)}
                disabled={delayDisabled || delayMs <= -DELAY_MAX_MS}
                aria-label={t("web.subtitles.menu.delayEarlier", { step: DELAY_STEP_MS })}
              >
                <Minus />
              </button>
              <span className="min-w-[4.25rem] text-center text-sm font-semibold tabular-nums">
                {t("web.subtitles.menu.delayValue", { delay: formatDelay(delayMs) })}
              </span>
              <button
                type="button"
                className="hud-mini-btn"
                onClick={() => nudgeDelay(DELAY_STEP_MS)}
                disabled={delayDisabled || delayMs >= DELAY_MAX_MS}
                aria-label={t("web.subtitles.menu.delayLater", { step: DELAY_STEP_MS })}
              >
                <Plus />
              </button>
              <button
                type="button"
                className="hud-mini-btn"
                onClick={resetDelay}
                disabled={delayDisabled || delayMs === 0}
                aria-label={t("web.subtitles.menu.resetDelayLabel")}
              >
                {t("web.subtitles.menu.resetDelay")}
              </button>
            </div>
          </div>
          {subtitleSync && activeSyncEntry && (
            <TimingControls sync={subtitleSync} entry={activeSyncEntry} />
          )}
          <div className="hud-panel-divider" />
          <div role="menu" aria-label={t("web.subtitles.menu.toolsLabel")} className="shrink-0">
            {canAddSubtitles && (
              <button
                role="menuitem"
                type="button"
                className="hud-item"
                onClick={() => {
                  setSearchOpen(true);
                  setOpen(false);
                }}
              >
                <span aria-hidden="true" className="hud-item-icon">
                  <Plus />
                </span>
                {t("web.subtitles.menu.addSubtitles")}
              </button>
            )}
            {canTranslate && (
              <button
                role="menuitem"
                type="button"
                className="hud-item"
                onClick={() => {
                  setTranslateOpen(true);
                  setOpen(false);
                }}
              >
                <span aria-hidden="true" className="hud-item-icon">
                  <Languages />
                </span>
                {t("web.subtitles.menu.translateWithAi")}
              </button>
            )}
            <button
              role="menuitem"
              type="button"
              className="hud-item"
              onClick={() => {
                setAppearanceOpen(true);
                setOpen(false);
              }}
            >
              <span aria-hidden="true" className="hud-item-icon">
                <SlidersHorizontal />
              </span>
              {t("web.subtitles.menu.appearance")}
            </button>
          </div>
        </PlayerMenuSurface>
      )}

      <SubtitleAppearancePanel open={appearanceOpen} onClose={() => setAppearanceOpen(false)} />

      {searchOpen &&
        mediaFileId &&
        playerConfig &&
        createPortal(
          <SubtitleSearchModal
            key={sessionId}
            mediaFileId={mediaFileId}
            playerConfig={playerConfig}
            isOpen={searchOpen}
            onlineSearchEnabled={onlineSearchEnabled}
            onClose={() => setSearchOpen(false)}
            onDownloadStarted={() => {
              downloadSelectionGeneration.current = downloadGeneration.current;
            }}
            onSubtitleDownloaded={(subtitle, source) => {
              setSearchOpen(false);
              if (subtitle) {
                subtitleSync?.remember(subtitle);
                if (
                  source === "download" &&
                  downloadSelectionGeneration.current === downloadGeneration.current
                ) {
                  setPendingDownload({
                    id: subtitle.id,
                    generation: downloadSelectionGeneration.current,
                  });
                }
              }
              onRefreshSubtitles?.();
            }}
          />,
          document.body,
        )}

      {translateOpen && mediaFileId && playerConfig && (
        <SubtitleTranslateModal
          mediaFileId={mediaFileId}
          playerConfig={playerConfig}
          tracks={tracks}
          preferredSubtitleLanguage={preferredSubtitleLanguage}
          audioTracks={audioTracks}
          translateEnabled={aiEnabled}
          transcribeEnabled={aiTranscribeEnabled}
          isOpen={translateOpen}
          sessionId={sessionId}
          getStartPosition={getSubtitleStartPosition}
          onSubtitleJobAccepted={onSubtitleJobAccepted}
          onClose={() => setTranslateOpen(false)}
        />
      )}
    </div>
  );
}

const TIMING_BUTTON_CLASS = "hud-mini-btn";

/**
 * "Sync to audio" and "Reset timing" for the selected track, stored or a file
 * next to the media, with a running sync's progress and the last result.
 * Anyone who can play the file may retime it; a refusal (demo mode) replaces
 * the actions with a short explanation.
 */
function TimingControls({ sync, entry }: { sync: SubtitleSync; entry: SubtitleSyncEntry }) {
  const t = useT();
  const key = entry.state.key;
  const job = entry.state.sync;
  const inProgress = isSyncInProgress(job?.status);
  const percent = syncProgressPercent(job);
  const canSync = sync.syncAvailable && !entry.unsupported;
  const canReset = !isIdentityTiming(entry.state.timing);
  if (!entry.forbidden && !canSync && !canReset && !entry.error) return null;

  let result: { text: string; warning?: boolean } | null = null;
  if (!inProgress && !entry.error) {
    switch (job?.status) {
      case "synced":
        if (!isIdentityTiming(entry.state.timing)) {
          result = {
            text: t("web.subtitles.menu.syncedResult", {
              timing: describeTiming(entry.state.timing),
            }),
          };
        }
        break;
      case "already_synced":
        result = { text: t("web.subtitles.menu.alreadyMatches") };
        break;
      case "no_match":
        result = {
          text: t("web.subtitles.menu.noMatch"),
          warning: true,
        };
        break;
      case "failed":
        result = { text: syncFailureMessage(job.failure), warning: true };
        break;
    }
  }

  return (
    <div className="shrink-0 px-4 py-1.5" data-testid="subtitle-timing">
      <div className="flex items-center justify-between gap-3">
        <span className="text-sm">{t("web.subtitles.menu.timing")}</span>
        {!entry.forbidden && (
          <div className="flex items-center gap-1">
            {canSync && (
              <button
                type="button"
                className={TIMING_BUTTON_CLASS}
                disabled={entry.busy || inProgress}
                onClick={() => void sync.requestSync(key)}
              >
                {inProgress ? t("web.subtitles.menu.syncing") : t("web.subtitles.menu.syncToAudio")}
              </button>
            )}
            {canReset && (
              <button
                type="button"
                className={TIMING_BUTTON_CLASS}
                disabled={entry.busy || inProgress}
                onClick={() => void sync.resetTiming(key)}
              >
                {t("web.subtitles.menu.resetTiming")}
              </button>
            )}
          </div>
        )}
      </div>
      {inProgress && (
        <div className="mt-2 flex flex-col gap-1">
          <span
            role="progressbar"
            aria-label={t("web.subtitles.sync.progressLabel")}
            aria-valuemin={0}
            aria-valuemax={100}
            aria-valuenow={percent ?? 0}
            className="h-1 overflow-hidden rounded-full bg-white/20"
          >
            <span
              className="block h-full rounded-full bg-[var(--hud-lamp)] transition-[width] duration-500"
              style={{ width: `${percent ?? 0}%` }}
            />
          </span>
          <span className="flex justify-between text-xs text-[var(--hud-ash)]">
            <span>{syncPhaseLabel(job)}</span>
            <span className="tabular-nums">
              {t("web.subtitles.sync.percent", { percent: percent ?? 0 })}
            </span>
          </span>
        </div>
      )}
      {result && (
        <p
          className={`mt-1 text-xs ${result.warning ? "text-amber-200" : "text-[var(--hud-ash)]"}`}
        >
          {result.text}
        </p>
      )}
      {!entry.forbidden && canSync && !inProgress && !result && (
        <p className="mt-1 text-xs text-[var(--hud-ash)]">
          {entry.state.source === "external"
            ? t("web.subtitles.menu.syncHintExternal")
            : t("web.subtitles.menu.syncHint")}
        </p>
      )}
      {entry.forbidden && (
        <p className="mt-1 text-xs text-[var(--hud-ash)]">
          {t("web.subtitles.menu.timingForbidden")}
        </p>
      )}
      {entry.error && (
        <p role="alert" className="mt-1 text-xs text-red-300">
          {entry.error}
        </p>
      )}
    </div>
  );
}
