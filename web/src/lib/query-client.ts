import { QueryClient } from "@tanstack/react-query";

import { V2TimeoutError } from "@/api/v2/request";

/**
 * A 401 or 403 describes the caller, not a transient fault: the session layer
 * has already tried a token refresh, so sending the same request again cannot
 * change the answer. Other 4xx failures (a dead catalog item's 404, a 422
 * validation refusal) are likewise permanent. Transient failures (5xx,
 * network) carry no client status and stay retryable.
 */
export function isClientError(error: unknown): boolean {
  const status = (error as { status?: unknown } | null | undefined)?.status;
  return typeof status === "number" && status >= 400 && status < 500;
}

/**
 * A timed-out read has already waited the full deadline. Retrying it would
 * hold the page on a loading state for a second deadline before the error and
 * its Try again appear, and a server that went quiet seldom recovers within
 * one more.
 */
function isTimeout(error: unknown): boolean {
  return error instanceof V2TimeoutError;
}

export const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 2 * 60_000,
      gcTime: 10 * 60_000,
      // One retry for transient failures; a 4xx (a dead catalog item's 404,
      // an auth refusal, a validation error) is permanent, so retrying it
      // just doubles the request.
      retry: (failureCount, error) =>
        failureCount < 1 && !isClientError(error) && !isTimeout(error),
      refetchOnWindowFocus: false,
      refetchOnReconnect: true,
      throwOnError: false,
    },
    mutations: {
      retry: 0,
    },
  },
});
