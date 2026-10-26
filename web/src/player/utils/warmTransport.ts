import type { PlanV3 } from "../protocol-v3";

/**
 * Identity of the bytes a plan produces and the route that serves them, with
 * every position-dependent field left out.
 *
 * The player warms the next stream by asking for a small prefix before the
 * element loads it, so the server-side remux starts early. That warm request
 * is a second connection to the same transport, and a remux seek spawns a
 * separate server-side process per connection: warming a plain seek therefore
 * duplicated the seek's work instead of preparing it. Two plans that differ
 * only in where playback starts produce the same key here, so the warm is
 * reserved for route changes (a different file, delivery, recipe, audio track
 * or burn-in subtitle) where a genuinely new transport has to start.
 */
export function buildWarmTransportRouteKey(plan: PlanV3): string {
  const recipe = plan.effective_recipe;
  const burnInSubtitle = plan.subtitle.mode === "burn_in" ? (plan.subtitle.track_id ?? "") : "";
  return [
    plan.delivery,
    plan.requested_media_file_id,
    plan.effective_media_file_id,
    plan.effective_virtual_uri ?? "",
    plan.stream.protocol,
    plan.stream.container ?? "",
    plan.stream.mime_type ?? "",
    plan.selected_tracks.audio?.index ?? "",
    burnInSubtitle,
    recipe.video_codec ?? "",
    recipe.audio_codec ?? "",
    recipe.width ?? "",
    recipe.height ?? "",
    recipe.bitrate_kbps ?? "",
    recipe.dynamic_range ?? "",
    recipe.audio_channels ?? "",
  ].join("|");
}
