import { describe, expect, it } from "vitest";

import {
  isServerManifestNotReady,
  MANIFEST_NOT_READY_CODE,
  MANIFEST_NOT_READY_DEFAULT_DELAY_MS,
  manifestRetryAfterMs,
} from "./manifest-not-ready";

describe("isServerManifestNotReady", () => {
  it("detects the retryable code on a 503 body", () => {
    expect(
      isServerManifestNotReady({
        response: {
          code: 503,
          data: JSON.stringify({
            error: MANIFEST_NOT_READY_CODE,
            message: "Transcode manifest not ready yet",
          }),
        },
      }),
    ).toBe(true);
  });

  it("rejects a bare 503 without the code", () => {
    expect(isServerManifestNotReady({ response: { code: 503 } })).toBe(false);
  });

  it("rejects the code on a non-503 status", () => {
    expect(
      isServerManifestNotReady({
        response: { code: 502, data: { error: MANIFEST_NOT_READY_CODE } },
      }),
    ).toBe(false);
  });
});

describe("manifestRetryAfterMs", () => {
  it("honors Retry-After within the cap", () => {
    expect(
      manifestRetryAfterMs({
        response: { code: 503 },
        networkDetails: { headers: { get: () => "2" } },
      }),
    ).toBe(2000);
  });

  it("falls back to the default without the header", () => {
    expect(manifestRetryAfterMs({ response: { code: 503 } })).toBe(
      MANIFEST_NOT_READY_DEFAULT_DELAY_MS,
    );
  });
});
