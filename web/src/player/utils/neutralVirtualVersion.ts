/**
 * Recognises a neutral `virtual://series/…` catalog row.
 *
 * Provider-backed episodes carry one collapsed row per episode next to the
 * concrete candidates the provider result picker materialises. The collapsed
 * row names no release (`virtual://series/<id>/<season>/<episode>`, with no
 * `?result=` pick) and publishes no probe inventory, so the server resolves it
 * to whatever candidate its ranking prefers. Requesting it is not a request for
 * a *specific* file, which is why resume state that points at it cannot be
 * honoured as if the viewer had chosen that release.
 *
 * The test is deliberately conservative: a concrete provider result (which
 * carries `?result=`) and a local file (which is not virtual) never match. Any
 * resolution, codec or track list is probe evidence and also disqualifies the
 * row.
 */
export interface NeutralVirtualVersionShape {
  container?: string;
  file_path?: string;
  resolution?: string;
  codec_video?: string;
  codec_audio?: string;
  video_tracks?: readonly unknown[];
  audio_tracks?: readonly unknown[];
  subtitle_tracks?: readonly unknown[];
}

const SERIES_VIRTUAL_PREFIX = "virtual://series/";
// A concrete provider pick, whether it is the first query parameter or a later
// one (`?profile=1080p&result=…`). `results=` is a different selector and does
// not match.
const PROVIDER_RESULT_MARKER = /[?&]result=/;

function hasText(value: string | undefined): boolean {
  return value != null && value.trim() !== "";
}

function hasItems(value: readonly unknown[] | undefined): boolean {
  return value != null && value.length > 0;
}

export function isNeutralVirtualVersion(version: NeutralVirtualVersionShape): boolean {
  if (version.container !== "virtual") return false;

  const filePath = (version.file_path ?? "").toLowerCase();
  if (!filePath.startsWith(SERIES_VIRTUAL_PREFIX)) return false;
  if (PROVIDER_RESULT_MARKER.test(filePath)) return false;

  return (
    !hasText(version.resolution) &&
    !hasText(version.codec_video) &&
    !hasText(version.codec_audio) &&
    !hasItems(version.video_tracks) &&
    !hasItems(version.audio_tracks) &&
    !hasItems(version.subtitle_tracks)
  );
}
