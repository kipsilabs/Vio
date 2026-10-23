import { useSyncExternalStore } from "react";
import type { FileVersion } from "@/api/types";
import type { PlayerAudioTrack, PlayerSubtitleInfo } from "./types";

/** The last inventory the player actually folded for one file. */
export interface PlaybackInventorySnapshot {
  fileId: number;
  audioTracks: PlayerAudioTrack[];
  subtitleTracks: PlayerSubtitleInfo[];
  updatedAt: number;
}

const snapshots = new Map<number, PlaybackInventorySnapshot>();
const listeners = new Set<() => void>();

function emit() {
  for (const listener of listeners) listener();
}

/**
 * Records the player's authoritative audio/subtitle inventory for a file.
 * The item page reads this after playback so its menus don't fall back to a
 * stale catalog row while the player's probe evidence is newer.
 */
export function publishPlaybackInventory(
  fileId: number | null | undefined,
  audioTracks: readonly PlayerAudioTrack[] | null | undefined,
  subtitleTracks: readonly PlayerSubtitleInfo[] | null | undefined,
) {
  if (!fileId || fileId <= 0) return;
  const audio = (audioTracks ?? []).map((track) => ({ ...track }));
  const subtitles = (subtitleTracks ?? []).map((track) => ({ ...track }));
  const previous = snapshots.get(fileId);
  if (
    previous &&
    previous.audioTracks.length === audio.length &&
    previous.subtitleTracks.length === subtitles.length &&
    JSON.stringify(previous.audioTracks) === JSON.stringify(audio) &&
    JSON.stringify(previous.subtitleTracks) === JSON.stringify(subtitles)
  ) {
    return;
  }
  snapshots.set(fileId, {
    fileId,
    audioTracks: audio,
    subtitleTracks: subtitles,
    updatedAt: Date.now(),
  });
  emit();
}

function subscribe(listener: () => void) {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

function getSnapshot(fileId: number | null | undefined) {
  return fileId ? (snapshots.get(fileId) ?? null) : null;
}

/** Reads the player's last inventory for a file, if it has played. */
export function usePlaybackInventoryMirror(fileId: number | null | undefined) {
  return useSyncExternalStore(
    subscribe,
    () => getSnapshot(fileId),
    () => null,
  );
}

/** Overlays a player inventory snapshot on a catalog/watch-detail version. */
export function mergePlaybackInventory(
  version: FileVersion,
  snapshot: PlaybackInventorySnapshot | null | undefined,
): FileVersion {
  if (!snapshot) return version;
  return {
    ...version,
    audio_tracks:
      snapshot.audioTracks.length > 0
        ? snapshot.audioTracks.map((track) => ({
            title: track.title,
            embedded_title: track.embedded_title,
            language: track.language,
            languages: track.languages,
            codec: track.codec,
            profile: track.profile,
            layout: track.layout,
            channels: track.channels,
            bitrate: track.bitrate,
            sample_rate: track.sample_rate,
            bit_depth: track.bit_depth,
            default: track.default,
          }))
        : version.audio_tracks,
    subtitle_tracks:
      snapshot.subtitleTracks.length > 0
        ? snapshot.subtitleTracks.map((track) => ({
            index: track.index,
            language: track.language,
            codec: track.codec,
            title: track.label,
            embedded_title: track.label,
            forced: track.forced,
            hearing_impaired: track.hearing_impaired,
            external: track.source === "external",
          }))
        : version.subtitle_tracks,
  };
}
