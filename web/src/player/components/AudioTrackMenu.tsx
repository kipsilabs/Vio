import { useState, useCallback, useEffect, useRef } from "react";
import { AudioLines } from "lucide-react";
import type { PlayerAudioTrack } from "../types";
import { formatChannels, mapAudioLabel } from "@/lib/mediaFormat";
import {
  audioTitle,
  compactAudioMeta,
  formatLanguageName,
} from "@/pages/ItemDetail/components/versionFormatUtils";
import { dedupeAudioTracks } from "../utils/trackDedupe";
import { PlayerMenuSurface } from "./PlayerMenuSurface";
import { ProvisionalTrackBadge } from "./ProvisionalTrackBadge";

interface AudioTrackMenuProps {
  tracks: PlayerAudioTrack[];
  activeIndex: number;
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
}

/**
 * Rich per-track descriptor used for the menu rows.
 *  - `title`   — human-readable primary label (falls back gracefully).
 *  - `meta`    — "Language · layout · bitrate · sample-rate · bit-depth".
 *  - `badges`  — codec / channel / default pills.
 *
 * This mirrors what the item detail page surfaces so the audio track names
 * stay consistent across the app instead of collapsing to an opaque embedded
 * label like `SyncUP` or `????`.
 */
interface TrackDescriptor {
  title: string;
  meta: string;
  codecLabel: string;
  channelsLabel: string;
  isDefault: boolean;
}

function describeTrack(track: PlayerAudioTrack, index: number): TrackDescriptor {
  const title = audioTitle(track) || `Track ${index + 1}`;
  const language = track.languages?.length
    ? track.languages.map(formatLanguageName).filter(Boolean).join("/")
    : formatLanguageName(track.language ?? "");
  const metaParts = [
    language && language.toLowerCase() !== title.toLowerCase() ? language : "",
    compactAudioMeta(track),
  ].filter(Boolean);
  return {
    title,
    meta: metaParts.join(" \u00B7 "),
    codecLabel: track.codec ? mapAudioLabel(track.codec) : "",
    channelsLabel: formatChannels(track.channels),
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
}: AudioTrackMenuProps) {
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
  }, [open, setOpen]);

  const menuItemsRef = useRef<(HTMLButtonElement | null)[]>([]);

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

  if (tracks.length === 0 && !provisional && !pending && !failed && !open) return null;

  // Probed inventories can repeat the same stream at several container
  // indexes; the menu shows one row per distinct descriptor. The retained
  // entry keeps its original inventory position so selection still names the
  // track the server has at that slot.
  const dedupedTracks = dedupeAudioTracks(tracks);

  return (
    <div ref={menuRef} className="relative" onBlur={handleBlur}>
      {!hideTrigger && (
        <button
          type="button"
          className="player-utility-btn disabled:cursor-not-allowed disabled:opacity-40"
          onClick={() => setOpen((v) => !v)}
          aria-label="Audio tracks"
          aria-expanded={open}
          aria-haspopup="menu"
          aria-disabled={locked || undefined}
          disabled={locked}
        >
          <AudioLines className="h-[18px] w-[18px]" />
        </button>
      )}

      {open && (
        <PlayerMenuSurface
          anchorRef={menuRef}
          className="absolute right-0 bottom-full z-30 mb-2 max-w-[min(360px,calc(100vw-1rem))] min-w-[280px] rounded-lg bg-black/90 py-1.5 shadow-xl backdrop-blur-sm"
          onClose={() => setOpen(false)}
          onKeyDown={handleMenuKeyDown}
        >
          <div className="flex items-center justify-between gap-2 px-3 py-1.5">
            <span className="text-xs font-medium tracking-wide text-white/50 uppercase">Audio</span>
            {provisional && <ProvisionalTrackBadge />}
          </div>
          {failed && dedupedTracks.length === 0 && (
            <div className="px-3 py-1 text-xs text-amber-300/80">Track discovery failed</div>
          )}
          {failed && dedupedTracks.length > 0 && (
            <div className="px-3 py-1 text-xs text-white/50">
              Showing available tracks — discovery did not complete
            </div>
          )}
          {!failed && (pending || provisional) && (
            <div className="flex items-center gap-2 px-3 py-1 text-xs text-white/50">
              <span className="h-3 w-3 animate-spin rounded-full border border-white/20 border-t-white/80" />
              <span>Discovering audio tracks…</span>
            </div>
          )}
          {dedupedTracks.length === 0 && !failed && !pending && !provisional && (
            <div className="px-3 py-2 text-xs text-white/40">No audio tracks available</div>
          )}
          {dedupedTracks.map(({ track, index }) => {
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
                  </span>
                  {descriptor.meta && (
                    <span className="truncate text-[11px] leading-snug text-white/55">
                      {descriptor.meta}
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

/** Compact pill used to tag tracks with codec/channel/default metadata. The
 *  `outline` variant is used for the DEFAULT marker so it reads as a
 *  qualifier rather than another content facet. */
function TrackBadge({
  children,
  variant = "solid",
}: {
  children: React.ReactNode;
  variant?: "solid" | "outline";
}) {
  const base =
    "inline-flex items-center rounded px-1.5 py-[1px] text-[9.5px] font-semibold tracking-wide whitespace-nowrap uppercase leading-4";
  const skin =
    variant === "outline" ? "border border-white/25 text-white/70" : "bg-white/10 text-white/75";
  return <span className={`${base} ${skin}`}>{children}</span>;
}
