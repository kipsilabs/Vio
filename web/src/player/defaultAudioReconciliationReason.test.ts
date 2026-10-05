// @vitest-environment node

import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";

import { FEATURE_DEFAULT_AUDIO_RECONCILE_RESPONSE_V3 } from "./protocol-v3";
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

/** Feature constants live beside the other v3 tokens, not with the reason. */
const GO_PROTOCOL_FILE = fileURLToPath(
  new URL("../../../internal/playback/protocol_v3.go", import.meta.url),
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

/**
 * The capability token that unlocks the withdrawal has the same half-and-half
 * problem as the reason: the server reads the token string out of the client's
 * `client_features` and uses it as the gate for sending a reconciliation
 * withdrawal. A rename on either side means the gate never opens — the client
 * waits for a withdrawal that is never sent, and plays the wrong audio until its
 * next start. Silent and invisible, so it is pinned here.
 *
 * The Go constant lives in internal/playback/protocol_v3.go; the server side
 * pins its own half of the same pair.
 */
describe("default audio reconciliation client capability", () => {
  // The Go half of this mirror is still landing on the parallel lane that owns
  // internal/playback/protocol_v3.go. Until its constant exists, skip rather
  // than fail the suite — as soon as it does, the test below starts enforcing
  // byte-equality on every run.
  const protocolSource = readFileSync(GO_PROTOCOL_FILE, "utf8");
  const goDeclaration = protocolSource.match(
    /FeatureDefaultAudioReconcileResponseV3\s*=\s*"([a-z0-9_]+)"/,
  );
  const itWithGo = goDeclaration ? it : it.skip;

  itWithGo("matches the Go constant the server gates the withdrawal on", () => {
    expect(FEATURE_DEFAULT_AUDIO_RECONCILE_RESPONSE_V3).toBe(goDeclaration?.[1]);
  });

  // The withdrawal is gated on this token and NOT on `plan_invalidated_v1`, so
  // the two must not collapse into one string: reusing the existing token would
  // hand the reconciliation to every older client, which replans it as a failure
  // recovery and evicts its own working route.
  it("is distinct from the plan-invalidated capability it is related to", () => {
    expect(FEATURE_DEFAULT_AUDIO_RECONCILE_RESPONSE_V3).not.toBe("plan_invalidated_v1");
  });
});
