import { describe, expect, it } from "vitest";

import { deriveVersionHealth } from "./versionHealth";

describe("deriveVersionHealth", () => {
  it("returns no badge while the server has reported no verdict", () => {
    expect(deriveVersionHealth({})).toBeNull();
  });

  it("keeps a server-reported live version clean", () => {
    expect(deriveVersionHealth({ available: true })).toBeNull();
  });

  it("warns for a version the server's liveness check could not resolve", () => {
    expect(deriveVersionHealth({ available: false })).toMatchObject({
      key: "unavailable",
      label: "Will retry on play",
      tone: "warn",
    });
  });

  it("marks a server-failed candidate as failed, over the liveness verdict", () => {
    expect(deriveVersionHealth({ available: false, failed: true })).toMatchObject({
      key: "failed",
      label: "Failed",
      tone: "danger",
    });
  });

  it("surfaces a probed source mismatch distinctly from an unprobed row", () => {
    expect(
      deriveVersionHealth({
        available: true,
        source_mismatch: { served_bytes: 7_427_636_945, declared_bytes: 8_449_999_999 },
      }),
    ).toMatchObject({
      key: "source_mismatch",
      label: "Source mismatch — served 6.9 GB of declared 7.9 GB",
      tone: "warn",
    });
  });

  it("keeps failed precedence over a source mismatch", () => {
    expect(
      deriveVersionHealth({
        failed: true,
        source_mismatch: { served_bytes: 7_000, declared_bytes: 8_000 },
      }),
    ).toMatchObject({ key: "failed", tone: "danger" });
  });

  it("does not describe a mismatch as failed or as unavailable", () => {
    const badge = deriveVersionHealth({
      available: false,
      source_mismatch: { served_bytes: 512, declared_bytes: 1024 },
    });
    expect(badge?.key).toBe("source_mismatch");
    expect(badge?.label).toBe("Source mismatch — served 512 B of declared 1.0 KB");
    expect(badge?.title).not.toContain("failed");
  });
});
