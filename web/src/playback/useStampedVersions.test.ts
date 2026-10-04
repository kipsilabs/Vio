import { renderHook } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { useStampedVersions } from "./useStampedVersions";

// Rows without a probed `available` flag always differ from a `false`
// verdict, so stamping builds a fresh array on every call — the identity
// churn that used to re-fire the player's reconnect reconcile (≈1 Hz
// refetch of the whole watch payload) on every parent render.
const ROW = { file_id: 7, file_path: "virtual://movie/x" };
const LIVENESS = new Map([[7, false]]);

describe("useStampedVersions", () => {
  it("keeps the stamped identity stable across re-renders with unchanged inputs", () => {
    // One array object, as the query cache returns while its content is
    // unchanged (structural sharing).
    const versions = [ROW];
    const { result, rerender } = renderHook(
      ({ versions: v, liveness: l }) => useStampedVersions(v, l),
      { initialProps: { versions, liveness: LIVENESS } },
    );
    const first = result.current;
    expect(first[0]).toMatchObject({ file_id: 7, available: false });

    // Same input references: no recompute, no new identity.
    rerender({ versions, liveness: LIVENESS });
    expect(result.current).toBe(first);
  });

  it("moves when the rows actually change", () => {
    const { result, rerender } = renderHook(
      ({ versions, liveness }) => useStampedVersions(versions, liveness),
      { initialProps: { versions: [ROW], liveness: LIVENESS } },
    );
    const first = result.current;

    rerender({
      versions: [{ ...ROW }, { file_id: 9, file_path: "virtual://movie/y" }],
      liveness: LIVENESS,
    });
    expect(result.current).not.toBe(first);
    expect(result.current).toHaveLength(2);
    expect(result.current[0]).toMatchObject({ file_id: 7, available: false });
  });

  it("moves when the verdicts actually change", () => {
    const { result, rerender } = renderHook(
      ({ versions, liveness }) => useStampedVersions(versions, liveness),
      { initialProps: { versions: [ROW], liveness: LIVENESS } },
    );
    const first = result.current;

    rerender({ versions: [ROW], liveness: new Map([[7, true]]) });
    expect(result.current).not.toBe(first);
    expect(result.current[0]).toMatchObject({ file_id: 7, available: true });
  });

  it("returns a stable empty array when rows are absent", () => {
    const { result, rerender } = renderHook(
      ({ versions, liveness }) => useStampedVersions(versions, liveness),
      {
        initialProps: {
          versions: undefined as { file_id: number; file_path: string }[] | undefined,
          liveness: LIVENESS,
        },
      },
    );
    const first = result.current;
    expect(first).toEqual([]);

    rerender({ versions: undefined, liveness: LIVENESS });
    expect(result.current).toBe(first);
  });
});
