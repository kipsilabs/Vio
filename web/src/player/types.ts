/**
 * Self-contained types for the player module.
 * Zero imports from app-specific code.
 */

/** Subtitle display mode. */
export type SubtitleMode = "off" | "auto" | "always";

/** How the video frame is sized within the player viewport. */
export type VideoFitMode = "contain" | "cover";

/** What the player does when it enters a detected intro. */
export type IntroSkipMode = "never" | "ask" | "always";

export interface PlayerVersionSubtitleTrack {
  index?: number;
  language?: string;
  codec?: string;
  title?: string;
  embedded_title?: string;
  forced?: boolean;
  hearing_impaired?: boolean;
  external?: boolean;
  file_name?: string;
  /** Opaque stable hash of an external sidecar's full path; distinguishes two same-basename sidecars. */
  path_key?: string;
}

/** A file version available for playback. */
export interface PlayerFileVersion {
  file_id: number;
  file_name?: string;
  /**
   * The catalog's on-disk path for this version. A virtual requested row is
   * resolved by the server to a concrete candidate; the plan publishes that
   * candidate's path as `effective_virtual_uri`, which menus match here.
   */
  file_path?: string;
  resolution: string;
  codec_video: string;
  codec_audio: string;
  hdr: boolean;
  container: string;
  file_size: number;
  duration: number;
  bitrate: number;
  /** Custom-format score the server ranked this candidate with; absent when
   *  the version is unscored (a local file, or no ranking ran). */
  format_score?: number;
  failed?: boolean;
  /**
   * The catalog's liveness verdict for this version, stamped from the server's
   * `POST /catalog/versions/check` result before the rows reach the player.
   * Absent means unknown (no result yet); `false` means the server's last check
   * could not resolve the row, which the version menu warns about. It is never
   * derived from the client's own playback failures.
   */
  available?: boolean;
  edition_key?: string;
  release_name?: string;
  /** Provider display label for a virtual candidate; the wire's release-name
   *  fallback. Present on watch-detail rows even though it is not modelled. */
  edition_raw?: string;
  release_group?: string;
  presentation_kind?: string;
  presentation_group_key?: string;
  presentation_part_index?: number;
  effective_audio_track_index?: number;
  effective_audio_language?: string;
  audio_channels?: number;
  video_tracks?: PlayerVideoTrack[];
  audio_tracks?: PlayerAudioTrack[];
  /** Per-version subtitle tracks */
  subtitle_tracks?: PlayerVersionSubtitleTrack[];
  chapters?: PlayerChapter[];
  intro?: PlayerTimeRange | null;
  credits?: PlayerTimeRange | null;
  recap?: PlayerTimeRange | null;
  preview?: PlayerTimeRange | null;
  marker_segments?: PlayerMarkerSegment[];
  /** Seek-bar previews are published for this file. */
  trickplay_available?: boolean;
}

/**
 * The ranking the server applied to a virtual item's version order. The watch
 * detail carries it; the version menu scans the version list for it, so the
 * player forwards the detail-level block rather than storing it per file.
 */
export interface PlayerVirtualRanking {
  profile_label?: string;
  source?: "profile" | "default";
  criteria?: { attribute: string; direction: "asc" | "desc" }[];
}

/**
 * A release that exists on the indexers but is not downloaded on the provider.
 * Carried additively on the watch detail below the playable versions; the
 * in-player version menu renders these as "needs a fetch" rows.
 */
export interface PlayerIndexerRelease {
  release_id: string;
  title: string;
  resolution?: string;
  codec_video?: string;
  codec_audio?: string;
  hdr?: boolean;
  size_bytes?: number;
  indexer?: string;
  published_at?: string;
  format_score?: number;
  protocol?: string;
  download_state: "not_downloaded" | "queued" | "failed";
}

export interface PlayerPlaybackVariantPart {
  part_index: number;
  default_file_id?: number;
  total_duration?: number;
  versions: PlayerFileVersion[];
}

export interface PlayerPlaybackVariant {
  variant_id: string;
  edition_raw?: string;
  edition_key?: string;
  presentation_kind?: string;
  presentation_group_key?: string;
  part_count: number;
  total_duration?: number;
  default_file_id?: number;
  parts: PlayerPlaybackVariantPart[];
}

export interface PlayerChapter {
  index: number;
  title: string;
  start_seconds: number;
  end_seconds: number;
  source: string;
  thumbnail_url?: string;
  thumbnail_thumbhash?: string;
}

export interface PlayerVideoTrack {
  title?: string;
  codec?: string;
  dolby_vision?: string;
  profile?: string;
  level?: number;
  width?: number;
  height?: number;
  aspect_ratio?: string;
  interlaced?: boolean;
  frame_rate?: string;
  bitrate?: number;
  video_range?: string;
  color_range?: string;
  color_primaries?: string;
  color_space?: string;
  color_transfer?: string;
  bit_depth?: number;
  pixel_format?: string;
  reference_frames?: number;
}

export interface PlayerAudioTrack {
  /**
   * Absolute container stream index as ffprobe reported it, when the server
   * publishes one. This is metadata, NOT the selection ordinal: the player
   * selects audio by the track's position in the inventory. A synthesized
   * virtual track carries no index.
   */
  index?: number;
  title?: string;
  embedded_title?: string;
  language?: string;
  languages?: string[];
  codec?: string;
  layout?: string;
  channels?: number;
  bitrate?: number;
  sample_rate?: number;
  bit_depth?: number;
  default?: boolean;
}

/** Subtitle track information. */
export interface PlayerSubtitleInfo {
  /**
   * The server's combined ordinal for this track, copied verbatim from the
   * plan's subtitle inventory. It is the identity echoed back on a track
   * change; it is never derived by counting or summing track arrays.
   */
  index: number;
  /** Media file whose subtitle inventory assigned this track index. */
  media_file_id?: number;
  /**
   * The server publishes this track but cannot deliver it as a sidecar, so
   * selecting it asks the server to composite it into the video instead of
   * rendering it in the browser.
   */
  burn_in_only?: boolean;
  /** Server-assigned track identity, echoed back on a track change. */
  track_id?: string;
  language: string;
  codec?: string;
  label: string;
  source?: "external" | "embedded" | "downloaded";
  forced?: boolean;
  hearing_impaired?: boolean;
  url: string;
  /**
   * Optional endpoint returning base64-encoded container font attachments for
   * ASS/SSA rendering. Present for embedded ASS tracks when the backend can
   * extract attachments from the source media file.
   */
  font_bundle_url?: string;
  /**
   * When true, this is an in-progress AI translation whose cues arrive over the
   * realtime websocket rather than from `url`. `useSubtitleTracks` feeds it from
   * the `liveCues` source instead of fetching.
   */
  live?: boolean;
}

export interface PlayerSubtitleTrackSignature {
  source?: "external" | "embedded" | "downloaded";
  language?: string;
  codec?: string;
  label?: string;
  forced?: boolean;
  hearing_impaired?: boolean;
}

export interface PrePlaySubtitleSelection {
  source: "embedded" | "external" | "downloaded";
  language?: string;
  codec?: string;
  label?: string;
  forced?: boolean;
  hearing_impaired?: boolean;
  external_subtitle_path?: string;
  downloaded_subtitle_id?: number;
  /** Backend track index, carried so the selection can persist as a preference. */
  track_index?: number;
}

/** A time range (intro start/end or credits start/end). */
export interface PlayerTimeRange {
  start: number;
  end: number;
}

/**
 * The four editable marker/segment kinds. Note "credits" is what the
 * Jellyfin-compatible API exposes as "Outro" — there is no separate outro kind.
 */
export type MarkerKind = "intro" | "recap" | "credits" | "preview";

/** Every occurrence in the v2 marker inventory. An empty array means no markers. */
export interface PlayerMarkerSegment {
  kind: MarkerKind;
  start_seconds: number;
  end_seconds: number;
}

/** A full set of editable marker ranges for one file (null = no marker). */
export interface MarkerDraft {
  intro: PlayerTimeRange | null;
  recap: PlayerTimeRange | null;
  credits: PlayerTimeRange | null;
  preview: PlayerTimeRange | null;
}

/** A marker range positioned on the seek bar, tagged with its kind for color. */
export interface MarkerRegionView {
  kind: MarkerKind;
  start: number;
  end: number;
}

/** Hints from the last playback session for version selection on resume. */
export interface ResumeHints {
  lastFileId?: number;
  lastResolution?: string;
  lastHDR?: boolean;
  lastCodecVideo?: string;
  lastEditionKey?: string;
}

/** Local playback snapshot captured when the user exits the player. */
export interface PlaybackExitState {
  positionSeconds: number;
  durationSeconds?: number;
  lastFileId?: number | null;
  lastResolution?: string;
  lastHDR?: boolean;
  lastCodecVideo?: string;
  lastEditionKey?: string;
  destinationHref?: string;
}

export type PlayerDisplayMode = "foreground" | "detached" | "postroll";

export interface PlayerPictureInPictureChange {
  active: boolean;
  playbackContinues: boolean;
}

export interface PlayerPlaybackStateChange {
  currentTime: number;
  duration: number;
  playing: boolean;
}

export interface PlayerPlaybackTransport {
  playPause: () => void | Promise<void>;
  seekBy: (secondsDelta: number) => void;
  skipBack: () => void;
  skipForward: () => void;
  seekTo: (seconds: number) => void;
  togglePictureInPicture: () => void | Promise<void>;
}

/**
 * What started a playback. `viewer`: the viewer asked for it here, with a Play
 * button, a card, an episode pick or the next-episode prompt's Play Now.
 * `automatic`: the app started it on its own, for a Watch Party selection, an
 * autoplay countdown or the next part of a multi-part file. Only a viewer's
 * start times `first_frame_ms`; an automatic one still reports `first_frame`.
 */
export type PlaybackStartTrigger = "viewer" | "automatic";

/** Props for the top-level WatchPage component. */
export interface WatchPageProps {
  contentId: string;
  title: string;
  year?: number;
  fileId?: number;
  libraryId?: number;
  versions: PlayerFileVersion[];
  /**
   * The liveness verdicts the parent already stamped onto `versions`, from
   * `POST /catalog/versions/check`. The watch detail does not carry them, so a
   * fresh detail read (the realtime reconnect reconcile) has to re-apply the
   * same map to keep the `available: false` / failed badges on the rows it
   * replaces.
   */
  versionLiveness?: Map<number, boolean>;
  playbackVariants?: PlayerPlaybackVariant[];
  /**
   * Releases that exist on the indexers but are not downloaded on the provider.
   * Shown below the playable versions in the version menu; empty hides the UI.
   */
  indexerReleases?: PlayerIndexerRelease[];
  /**
   * The ranking the server applied to the version list, from the watch detail.
   * Absent for local content, where the version menu keeps its `?profile=`
   * label fallback.
   */
  virtualRanking?: PlayerVirtualRanking;
  subtitles: PlayerSubtitleInfo[];
  initialPosition?: number;
  forceInitialPosition?: boolean;
  qualityPreference?: string | null;
  /** Bandwidth cap in kbps from playback.max_bitrate_kbps; null/undefined is uncapped. */
  maxBitrateKbps?: number | null;
  explicitAudioTrackIndex?: number | null;
  /** True when the initial `fileId` was explicitly chosen by the viewer; the
   * server must not silently substitute another version. */
  explicitFileSelection?: boolean;
  /** When true, the server should force a re-link/re-query of the virtual file
   *  on this start attempt. Only set when the viewer explicitly picks an
   *  unavailable version. */
  forceRelink?: boolean;
  /** Initial server subtitle ordinal keyed by file ID. Missing entries mean subtitles start off. */
  initialSubtitleTrackIndexByFileId?: Record<number, number>;
  /**
   * The subset of initial subtitle ordinals that require bitmap burn-in. A
   * refused initial start is retried without these tracks so playback remains
   * available when the server cannot perform the required video conversion.
   */
  initialBitmapSubtitleTrackIndexByFileId?: Record<number, number>;
  preferredSubtitleLanguage?: string | null;
  preferredSubtitleTrackSignature?: PlayerSubtitleTrackSignature | null;
  subtitleMode?: SubtitleMode;
  showForcedSubtitles?: boolean;
  profileLanguage?: string | null;
  intro: PlayerTimeRange | null;
  /** null while the connected server's answer is still unknown; see VideoPlayer. */
  introSkipMode?: IntroSkipMode | null;
  credits: PlayerTimeRange | null;
  recap?: PlayerTimeRange | null;
  preview?: PlayerTimeRange | null;
  autoSkipRecap?: boolean;
  autoPlayNextPreview?: boolean;
  canEditMarkers?: boolean;
  seriesContext?: SeriesContext;
  onNavigateEpisode?: (contentId: string, trigger: PlaybackStartTrigger) => void;
  onEnded?: (state?: PlaybackExitState) => void | Promise<void>;
  onExit: (state?: PlaybackExitState) => void | Promise<void>;
  onMinimize?: (state?: PlaybackExitState) => void | Promise<void>;
  resumeHints?: ResumeHints;
  playbackRequestKey?: string;
  watchTogetherRoomId?: string | null;
  watchTogetherRoomToken?: string | null;
  /** Resolved profile intervals; the contract defaults on servers without shared seek settings. */
  seekIntervals: { back: number; forward: number };
  displayMode?: PlayerDisplayMode;
  onPictureInPictureChange?: (change: PlayerPictureInPictureChange) => void;
  autoEnterPictureInPicture?: boolean;
  onPlaybackStateChange?: (state: PlayerPlaybackStateChange) => void;
  onPlaybackTransportReady?: (transport: PlayerPlaybackTransport | null) => void;
  onReturnFromPostRoll?: () => void;
}

/** A quality option shown in the player settings menu. */
export interface QualityOption {
  id: string;
  label: string;
  sublabel: string;
  resolution: string;
  bitrateKbps: number;
  isOriginal: boolean;
}

/** Context for series playback (episode navigation). */
export interface SeriesContext {
  seriesId: string;
  seriesTitle?: string;
  currentSeason: number;
  currentEpisode: number;
  episodes: EpisodeRef[];
}

/** Minimal episode reference for the player. */
export interface EpisodeRef {
  contentId: string;
  seasonNumber: number;
  episodeNumber: number;
  title: string;
  runtime: number;
  /** Optional display fields for post-roll / next-episode UI. */
  overview?: string;
  stillUrl?: string;
  stillThumbhash?: string;
  airDate?: string | null;
}
