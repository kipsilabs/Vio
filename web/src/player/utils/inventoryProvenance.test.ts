import { describe, expect, it } from "vitest";
import { isInventoryProvisional, isOverlayInventoryProvisional } from "./inventoryProvenance";

describe("isInventoryProvisional", () => {
  it("is provisional while the stamp-derived status is declared", () => {
    expect(isInventoryProvisional("declared", "")).toBe(true);
    expect(isInventoryProvisional("declared", undefined)).toBe(true);
  });

  it("is confident once the status is verified", () => {
    expect(isInventoryProvisional("verified", "")).toBe(false);
  });

  it("treats a declared or pending resolve as provisional even when the stamp reads verified", () => {
    expect(isInventoryProvisional("verified", "declared")).toBe(true);
    expect(isInventoryProvisional("verified", "pending")).toBe(true);
  });

  it("keeps a failed probe provisional because declared metadata is served", () => {
    expect(isInventoryProvisional("declared", "failed")).toBe(true);
  });

  it("trusts a verified resolve over a declared stamp", () => {
    expect(isInventoryProvisional("declared", "verified")).toBe(false);
  });

  it("stays confident when an older server publishes neither field", () => {
    expect(isInventoryProvisional(undefined, undefined)).toBe(false);
    expect(isInventoryProvisional("", "")).toBe(false);
  });

  it("treats tracks_pending as provisional even over verified stamp and provenance", () => {
    expect(isInventoryProvisional("verified", "verified", true)).toBe(true);
    expect(isInventoryProvisional("declared", "declared", true)).toBe(true);
    expect(isInventoryProvisional(undefined, undefined, true)).toBe(true);
  });

  it("ignores an absent or false tracks_pending flag", () => {
    expect(isInventoryProvisional("verified", "verified", false)).toBe(false);
    expect(isInventoryProvisional("verified", "verified", undefined)).toBe(false);
    expect(isInventoryProvisional("verified", "verified", null)).toBe(false);
  });
});

describe("isOverlayInventoryProvisional", () => {
  it("stays provisional while the plan reports declared or pending inventory", () => {
    expect(isOverlayInventoryProvisional(true, true, 2)).toBe(true);
    expect(isOverlayInventoryProvisional(true, false, 0)).toBe(true);
  });

  it("stays provisional while the rows are catalog fallback, not plan evidence", () => {
    expect(isOverlayInventoryProvisional(false, false, 2)).toBe(true);
  });

  it("treats plan-published rows with no provisional marker as final", () => {
    expect(isOverlayInventoryProvisional(false, true, 2)).toBe(false);
  });

  it("keeps an empty verified inventory final so the menu can show its empty state", () => {
    expect(isOverlayInventoryProvisional(false, false, 0)).toBe(false);
    expect(isOverlayInventoryProvisional(false, true, 0)).toBe(false);
  });
});
