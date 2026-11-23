import { useMemo } from "react";

import {
  buildClientCapabilitiesV3,
  buildClientPlaybackContextV3,
} from "@/player/client-context-v3";
import { useBrowserEngineCapabilities } from "@/player/hooks/useBrowserEngine";
import { useCodecDetection } from "@/player/hooks/useCodecDetection";
import { VIDEO_CLIENT_FEATURES_V3 } from "@/player/playback-session-wire-v3";
import type { VersionPlayabilityClient } from "./versionLiveness";

export interface VersionPlayabilityClientState {
  client: VersionPlayabilityClient;
  /** False until the decoder probe and the browser decode engine have answered. */
  ready: boolean;
}

/**
 * The client half of a playback start, for a versions check that judges each
 * row with the production planner before the viewer picks it.
 *
 * One derivation serves every version list — the item page's picker and the
 * player's version menu — so a row judged unplayable in one is unplayable in
 * the other. It is the same capability surface the start sends: the web decode
 * engine adds codecs and a managed HDR range to the `original_http` class, so
 * omitting it could call a row unplayable that the player would decode.
 *
 * `ready` stays false until both probes settle: a verdict computed from an
 * unfinished probe is not evidence about the device.
 */
export function useVersionPlayabilityClient(): VersionPlayabilityClientState {
  const capabilityProbe = useCodecDetection();
  const browserEngine = useBrowserEngineCapabilities();
  const client = useMemo(
    () => ({
      features: VIDEO_CLIENT_FEATURES_V3,
      capabilities: buildClientCapabilitiesV3(capabilityProbe, browserEngine.capabilities),
      playbackContext: buildClientPlaybackContextV3(capabilityProbe, browserEngine.capabilities),
    }),
    [capabilityProbe, browserEngine.capabilities],
  );
  return { client, ready: capabilityProbe.settled && browserEngine.settled };
}
