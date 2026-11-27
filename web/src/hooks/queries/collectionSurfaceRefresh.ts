import type { QueryClient } from "@tanstack/react-query";
import { catalogKeys, collectionKeys, libraryCollectionKeys } from "./keys";
import { isTerminalItemDetailNotFound } from "./mediaSurfaceRefresh";

export async function invalidateUserCollectionQueries(
  queryClient: QueryClient,
  collectionId?: string,
) {
  await Promise.all([
    queryClient.invalidateQueries({ queryKey: collectionKeys.all }),
    queryClient.invalidateQueries({
      queryKey: catalogKeys.all,
      predicate: (query) => !isTerminalItemDetailNotFound(query),
    }),
    ...(collectionId
      ? [queryClient.invalidateQueries({ queryKey: collectionKeys.items(collectionId) })]
      : []),
  ]);
}

export async function invalidateLibraryCollectionQueries(queryClient: QueryClient) {
  await Promise.all([
    queryClient.invalidateQueries({ queryKey: libraryCollectionKeys.all }),
    // The Collections page's Server collections list, which the grid there
    // reads beside each library's list.
    queryClient.invalidateQueries({ queryKey: collectionKeys.server() }),
    queryClient.invalidateQueries({ queryKey: catalogKeys.all }),
  ]);
}

export async function invalidateAdminCollectionQueries(queryClient: QueryClient) {
  await Promise.all([
    invalidateLibraryCollectionQueries(queryClient),
    queryClient.invalidateQueries({ queryKey: ["admin", "collections"] }),
    queryClient.invalidateQueries({ queryKey: ["admin", "collectionGroups"] }),
  ]);
}
