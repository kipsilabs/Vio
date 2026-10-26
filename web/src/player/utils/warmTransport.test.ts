import { describe, expect, it } from "vitest";

import { fixturePlanV3 } from "../protocol-v3.fixtures";
import { buildWarmTransportRouteKey } from "./warmTransport";

const remuxPlan = fixturePlanV3({
  delivery: "server_remux_progressive",
  stream: {
    url: "/stream/session-1?seek=100",
    protocol: "http_progressive",
    container: "mp4",
    mime_type: "video/mp4",
    headers: {},
    header_refresh: "none",
  },
});

describe("buildWarmTransportRouteKey", () => {
  it("ignores a new position on the same route", () => {
    const seeked = fixturePlanV3({
      ...remuxPlan,
      plan_id: "plan:seeked",
      plan_attempt_key: "v3:seeked",
      stream: { ...remuxPlan.stream, url: "/stream/session-1?seek=900" },
    });
    expect(buildWarmTransportRouteKey(seeked)).toBe(buildWarmTransportRouteKey(remuxPlan));
  });

  it("changes when the delivery, source or produced bytes change", () => {
    const base = buildWarmTransportRouteKey(remuxPlan);
    const direct = fixturePlanV3({ ...remuxPlan, delivery: "original_http" });
    const switched = fixturePlanV3({
      ...remuxPlan,
      effective_media_file_id: remuxPlan.effective_media_file_id + 1,
    });
    const downscaled = fixturePlanV3({
      ...remuxPlan,
      effective_recipe: { ...remuxPlan.effective_recipe, height: 720 },
    });
    expect(buildWarmTransportRouteKey(direct)).not.toBe(base);
    expect(buildWarmTransportRouteKey(switched)).not.toBe(base);
    expect(buildWarmTransportRouteKey(downscaled)).not.toBe(base);
  });

  it("changes when the burned-in subtitle changes", () => {
    const burnIn = fixturePlanV3({
      ...remuxPlan,
      subtitle: { ...remuxPlan.subtitle, mode: "burn_in", track_id: "file:1:subtitle:0" },
    });
    const otherBurnIn = fixturePlanV3({
      ...remuxPlan,
      subtitle: { ...remuxPlan.subtitle, mode: "burn_in", track_id: "file:1:subtitle:1" },
    });
    expect(buildWarmTransportRouteKey(burnIn)).not.toBe(buildWarmTransportRouteKey(otherBurnIn));
  });
});
