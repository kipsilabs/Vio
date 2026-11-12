import { useState, useCallback, useRef } from "react";
import { AudioLines, Check } from "lucide-react";
import type { PlayerAudioTrack } from "../types";
import { audioTrackLabel, formatChannels } from "@/lib/mediaFormat";
<<<<<<< HEAD
import {
  audioTitle,
  compactAudioMeta,
  formatLanguageName,
} from "@/pages/ItemDetail/components/versionFormatUtils";
import { dedupeAudioTracks } from "../utils/trackDedupe";
=======
import { formatLanguageName } from "@/pages/ItemDetail/components/versionFormatUtils";
import { useT, type Translate } from "@/i18n";
>>>>>>> upstream/main
import { PlayerMenuSurface } from "./PlayerMenuSurface";
import { ProvisionalTrackBadge } from "./ProvisionalTrackBadge";
import { DiscoveryRetryButton } from "./DiscoveryRetryButton";
import { TrackBadge } from "./TrackBadge";
import { useProvisionalInventoryExpiry } from "../hooks/useProvisionalInventoryExpiry";

interface AudioTrackMenuProps {
  tracks: PlayerAudioTrack[];
  /** The applied row, or null when no row matched the committed selection. */
  activeIndex: number | null;
  onSelect: (index: number, currentPosition: number) => void;
  currentPosition: number;
  open?: boolean;
  onOpenChange?: (open: boolean) => void;
  hideTrigger?: boolean;
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
  /**
   * True while the deferred track enumeration is still running.
   */
  pending?: boolean;
  /**
   * True when track discovery terminally failed or timed out.
   */
  failed?: boolean;
  /**
   * Re-runs track discovery. Shown as a retry once the provisional window
   * lapses or discovery fails, so a stuck menu is actionable.
   */
  onRetryDiscovery?: () => void;
}

/**
 * Per-track descriptor used for the menu rows.
 *  - `title` — human-readable primary label (falls back gracefully).
 *  - `meta`  — "codec · layout · language".
 *
 * This mirrors what the item detail page surfaces so the audio track names
 * stay consistent across the app instead of collapsing to an opaque embedded
 * label like `SyncUP` or `????`.
 */
interface TrackDescriptor {
  title: string;
  meta: string;
  isDefault: boolean;
}

<<<<<<< HEAD
function describeTrack(track: PlayerAudioTrack, index: number): TrackDescriptor {
  const title = audioTitle(track) || `Track ${index + 1}`;
  const language = track.languages?.length
    ? track.languages.map(formatLanguageName).filter(Boolean).join("/")
    : formatLanguageName(track.language ?? "");
=======
/** A stream title that only restates the format ("DCA (DTS Coherent Acoustics)", "Stereo"). */
const CODEC_TITLE =
  /^(?:[\s()·.,-]|dca|dts(?:-hd)?|ma|coherent|acoustics|ac-?3|e-?ac-?3|truehd|atmos|aac|flac|opus|mp3|pcm|dolby|digital|plus|surround|stereo|mono|[257]\.[01])+$/i;

function describeTrack(t: Translate, track: PlayerAudioTrack, index: number): TrackDescriptor {
  const language = formatLanguageName(track.language ?? "");
  // Only a title the stream carries: without one, the language leads rather
  // than a run-together "en aac 5.1".
  const streamTitle = track.title || track.embedded_title || "";
  const title =
    (streamTitle && !(language && CODEC_TITLE.test(streamTitle)) ? streamTitle : language) ||
    t("web.player.audioMenu.trackFallback", { number: index + 1 });
  // Codec and layout are what tell two tracks in the same language apart; the
  // bitrate and sample detail stay in Playback info.
>>>>>>> upstream/main
  const metaParts = [
    audioTrackLabel(track),
    formatChannels(track.channels),
    language && language.toLowerCase() !== title.toLowerCase() ? language : "",
  ].filter(Boolean);
  return {
    title,
    meta: metaParts.join(" \u00B7 "),
    isDefault: Boolean(track.default),
  };
}

export function AudioTrackMenu({
  tracks,
  activeIndex,
  onSelect,
  currentPosition,
  open: controlledOpen,
  onOpenChange,
  hideTrigger = false,
  locked = false,
  provisional = false,
  pending = false,
  failed = false,
  onRetryDiscovery,
}: AudioTrackMenuProps) {
  const t = useT();
  const [uncontrolledOpen, setUncontrolledOpen] = useState(false);
  const rawOpen = controlledOpen ?? uncontrolledOpen;
  // A replace or replan swaps the inventory out from under the menu. The raw
  // open state is kept so the menu returns once the replacement lands, but the
  // trigger and surface stay shut until then so a pick cannot name a slot the
  // outgoing plan owned.
  const open = rawOpen && !locked;
  const setOpen = useCallback(
    (value: boolean | ((previous: boolean) => boolean)) => {
      const next = typeof value === "function" ? value(rawOpen) : value;
      if (controlledOpen === undefined) setUncontrolledOpen(next);
      onOpenChange?.(next);
    },
    [controlledOpen, onOpenChange, rawOpen],
  );
  const menuRef = useRef<HTMLDivElement>(null);

  const handleSelect = useCallback(
    (index: number) => {
      if (locked) return;
      onSelect(index, currentPosition);
      setOpen(false);
    },
    [currentPosition, locked, onSelect, setOpen],
  );

  const handleBlur = useCallback(
    (e: React.FocusEvent) => {
      if (!menuRef.current?.contains(e.relatedTarget as Node)) {
        setOpen(false);
      }
<<<<<<< HEAD
    };
    document.addEventListener("keydown", handleKeyDown);
    return () => document.removeEventListener("keydown", handleKeyDown);
  }, [open, setOpen]);

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
=======
    },
    [setOpen],
  );
>>>>>>> upstream/main

  // A spinner with no bound is an indefinite lie about progress. Once the
  // provisional window lapses, the menu offers the same retry a terminal
  // failure does instead of spinning forever.
  const { expired: discoveryExpired, retry: restartDiscovery } =
    useProvisionalInventoryExpiry(provisional);
  const handleRetryDiscovery = useCallback(() => {
    restartDiscovery();
    onRetryDiscovery?.();
  }, [onRetryDiscovery, restartDiscovery]);

<<<<<<< HEAD
  if (tracks.length === 0 && !provisional && !pending && !failed && !open) return null;

  // Probed inventories can repeat the same stream at several container
  // indexes; the menu shows one row per distinct descriptor. The retained
  // entry keeps its original inventory position so selection still names the
  // track the server has at that slot.
  const dedupedTracks = dedupeAudioTracks(tracks);
  // Declared or pending rows are placeholders for the effective file, so they
  // stay out of the menu until probe evidence lands. A terminal failure keeps
  // the list under its explicit "discovery did not complete" notice instead of
  // leaving the menu empty.
  const displayedTracks = provisional && !failed ? [] : dedupedTracks;
=======
  const disabled = tracks.length <= 1;
  // The rail names the track playing ("English 5.1") where it has room.
  const activeTrack = tracks[activeIndex];
  const valueLabel = activeTrack
    ? [formatLanguageName(activeTrack.language ?? ""), formatChannels(activeTrack.channels)]
        .filter(Boolean)
        .join(" ")
    : "";
>>>>>>> upstream/main

  return (
    <div ref={menuRef} className="relative" onBlur={handleBlur}>
      {!hideTrigger && (
        <button
          type="button"
<<<<<<< HEAD
          className="player-utility-btn disabled:cursor-not-allowed disabled:opacity-40"
          onClick={() => setOpen((v) => !v)}
          aria-label="Audio tracks"
=======
          className="hud-icon-btn hud-rail-btn"
          onClick={disabled ? undefined : () => setOpen((v) => !v)}
          aria-label={
            valueLabel
              ? t("web.player.audioMenu.triggerWithTrack", { track: valueLabel })
              : t("web.player.audioMenu.trigger")
          }
          title={
            disabled ? t("web.player.audioMenu.onlyOneTrack") : t("web.player.audioMenu.trigger")
          }
>>>>>>> upstream/main
          aria-expanded={open}
          aria-haspopup="menu"
          aria-disabled={locked || undefined}
          disabled={locked}
        >
          <AudioLines />
          {valueLabel ? <span className="hud-rail-label">{valueLabel}</span> : null}
        </button>
      )}

      {open && (
        <PlayerMenuSurface
          title={t("web.player.audioMenu.heading")}
          anchorRef={menuRef}
          className="hud-popover w-[27rem] max-w-[calc(100vw-2rem)]"
          onClose={() => setOpen(false)}
        >
<<<<<<< HEAD
          <div className="flex items-center justify-between gap-2 px-3 py-1.5">
            <span className="text-xs font-medium tracking-wide text-white/50 uppercase">Audio</span>
            {provisional && <ProvisionalTrackBadge />}
          </div>
          {/* The failure notice only qualifies a still-unverified list. A menu
              that already renders confident probe evidence (verified push or a
              catalog/plan list) is not a failed discovery, so a timed-out poll
              must never draw "discovery did not complete" above a correct,
              populated list. `provisional` is the menu's own evidence state. */}
          {failed && provisional && dedupedTracks.length === 0 && (
            <div className="flex items-center justify-between gap-2 px-3 py-1 text-xs text-amber-300/80">
              <span>Track discovery failed</span>
              <DiscoveryRetryButton onRetry={handleRetryDiscovery} buttonRef={retryButtonRef} />
            </div>
          )}
          {failed && provisional && dedupedTracks.length > 0 && (
            <div className="flex items-center justify-between gap-2 px-3 py-1 text-xs text-white/50">
              <span>Showing available tracks — discovery did not complete</span>
              <DiscoveryRetryButton onRetry={handleRetryDiscovery} buttonRef={retryButtonRef} />
            </div>
          )}
          {!failed && provisional && !discoveryExpired && (
            <div className="flex items-center gap-2 px-3 py-1 text-xs text-white/50">
              <span className="h-3 w-3 animate-spin rounded-full border border-white/20 border-t-white/80" />
              <span>Discovering audio tracks…</span>
            </div>
          )}
          {!failed && provisional && discoveryExpired && (
            <div className="flex items-center justify-between gap-2 px-3 py-1 text-xs text-white/50">
              <span>Audio tracks unavailable.</span>
              <DiscoveryRetryButton onRetry={handleRetryDiscovery} buttonRef={retryButtonRef} />
            </div>
          )}
          {dedupedTracks.length === 0 && !provisional && (
            <div className="px-3 py-2 text-xs text-white/40">No audio tracks available</div>
          )}
          {displayedTracks.map(({ track, index }) => {
            const descriptor = describeTrack(track, index);
            const isActive = index === activeIndex;
            return (
              <button
                key={index}
                ref={(el) => {
                  menuItemsRef.current[index] = el;
                }}
                role="menuitem"
                type="button"
                className={`flex w-full items-start gap-2 px-3 py-2 text-left transition-colors hover:bg-white/10 focus-visible:ring-2 focus-visible:ring-white/70 focus-visible:outline-none ${
                  isActive ? "text-blue-400" : "text-white/85"
                }`}
                onClick={() => handleSelect(index)}
              >
                <span className="mt-[3px] w-4 shrink-0 text-center text-sm leading-none">
                  {isActive ? "\u2713" : ""}
                </span>
                <span className="flex min-w-0 flex-1 flex-col gap-1">
                  <span className="flex flex-wrap items-center gap-1.5">
                    <span className="truncate text-sm font-medium">{descriptor.title}</span>
                    {descriptor.codecLabel && <TrackBadge>{descriptor.codecLabel}</TrackBadge>}
                    {descriptor.channelsLabel && (
                      <TrackBadge>{descriptor.channelsLabel}</TrackBadge>
                    )}
                    {descriptor.isDefault && <TrackBadge variant="outline">Default</TrackBadge>}
=======
          <div className="hud-scroll-list">
            {tracks.map((track, index) => {
              const descriptor = describeTrack(t, track, index);
              const isActive = index === activeIndex;
              return (
                <button
                  key={index}
                  role="menuitemradio"
                  type="button"
                  aria-checked={isActive}
                  className="hud-item items-start"
                  onClick={() => handleSelect(index)}
                >
                  <span aria-hidden="true" className="hud-check pt-0.5">
                    {isActive ? <Check /> : null}
>>>>>>> upstream/main
                  </span>
                  <span className="flex min-w-0 flex-1 flex-col gap-0.5">
                    <span className="line-clamp-2" title={descriptor.title}>
                      {descriptor.title}
                    </span>
                    {descriptor.meta || descriptor.isDefault ? (
                      <span className="hud-item-secondary flex flex-wrap items-center gap-x-2 gap-y-1">
                        {descriptor.meta ? <span>{descriptor.meta}</span> : null}
                        {descriptor.isDefault ? (
                          <span className="hud-chip">{t("web.player.audioMenu.defaultBadge")}</span>
                        ) : null}
                      </span>
                    ) : null}
                  </span>
                </button>
              );
            })}
          </div>
        </PlayerMenuSurface>
      )}
    </div>
  );
}
