import { describe, expect, it } from "vitest";
import { v2Problem } from "@/api/v2/problems.test-support";
import { V2TimeoutError } from "@/api/v2/request";
import { queryClient } from "./query-client";

function retry(): (failureCount: number, error: Error) => boolean {
  const retryOption = queryClient.getDefaultOptions().queries?.retry;
  if (typeof retryOption !== "function") {
    throw new Error("expected the global query retry option to be a function");
  }
  return retryOption;
}

describe("queryClient query retry", () => {
  it("does not retry a 404", () => {
    expect(retry()(0, v2Problem(404, "not_found", "Not Found"))).toBe(false);
  });

  it("does not retry other 4xx problems", () => {
    expect(retry()(0, v2Problem(422, "validation_failed", "Invalid request"))).toBe(false);
  });

  it("retries a transient 5xx once", () => {
    const serverError = v2Problem(500, "internal_error", "Internal Server Error");
    expect(retry()(0, serverError)).toBe(true);
    expect(retry()(1, serverError)).toBe(false);
  });

  it("retries a status-less network failure once", () => {
    const networkError = new Error("network down");
    expect(retry()(0, networkError)).toBe(true);
    expect(retry()(1, networkError)).toBe(false);
  });

  it("does not retry a read that already waited out its deadline", () => {
    expect(retry()(0, new V2TimeoutError("listProfiles", 30_000))).toBe(false);
  });
});
