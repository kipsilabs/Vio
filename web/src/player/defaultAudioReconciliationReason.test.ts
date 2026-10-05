// @vitest-environment node

import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";

import { PLAN_INVALIDATED_DEFAULT_AUDIO_RECONCILIATION } from "./realtime-protocol";

/**
 * The `default_audio_reconciliation` withdrawal reason exists on both sides of
 * the wire, and nothing but these two checks keeps them together: the browser
 * cannot import the Go constant, so a rename on either side silently degrades
 * a healthy session into a failure recovery that excludes its own working route
 * (see `invalidatePlan` in hooks/usePlaybackSession.ts). The failure is invisible
 * — playback merely degrades — so it is worth a test in each repository rather
 * than a comment.
 *
 * The Go constant lives in internal/playback/realtime.go and is pinned from the
 * server side by TestPlanInvalidatedDefaultAudioReconciliationReasonMatchesClient;
 * the client half is pinned here. Documented in
 * docs/architecture/playback-protocol-v3.md §6.1.1.
 */
const GO_CONSTANT_FILE = fileURLToPath(
  new URL("../../../internal/playback/realtime.go", import.meta.url),
);

describe("default audio reconciliation invalidation reason", () => {
  it("matches the Go constant that names it on the wire", () => {
    const source = readFileSync(GO_CONSTANT_FILE, "utf8");
    const declaration = source.match(/PlanInvalidatedDefaultAudioReconciliation\s*=\s*"([a-z_]+)"/);
    expect(declaration).not.toBeNull();
    expect(PLAN_INVALIDATED_DEFAULT_AUDIO_RECONCILIATION).toBe(declaration?.[1]);
  });

  it("is distinct from the copy-safety reason, which is a real route failure", () => {
    // Both reasons arrive on the same command and take different replan paths,
    // so the constants must not collapse into one another.
    expect(PLAN_INVALIDATED_DEFAULT_AUDIO_RECONCILIATION).not.toBe("video_copy_unsafe");
  });
});
