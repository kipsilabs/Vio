import { useEffect } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { discoverVirtualCandidates } from "@/api/v2/mediaCandidates";
import { fetchVirtualLibraryCapability } from "@/api/v2/virtualLibrary";
import { catalogKeys, itemKeys } from "@/hooks/queries/keys";

export function usePreplayDiscovery(mediaId: string | undefined, enabled: boolean) {
  const queryClient = useQueryClient();
  const capability = useQuery({
    queryKey: ["capabilities", "virtual-library"],
    queryFn: fetchVirtualLibraryCapability,
    enabled: enabled && !!mediaId,
    staleTime: 30_000,
    retry: false,
  });
  const discovery = useQuery({
    queryKey: ["virtual-candidates", "discover", mediaId],
    queryFn: async ({ signal }) => {
      await discoverVirtualCandidates(mediaId!, signal);
      return true;
    },
    enabled: enabled && !!mediaId && capability.data?.discover_before_playback === true,
    staleTime: 30_000,
    retry: false,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
  });

  useEffect(() => {
    if (!mediaId || !discovery.dataUpdatedAt) return;
    void queryClient.invalidateQueries({
      queryKey: catalogKeys.itemDetail(mediaId).slice(0, 4),
    });
    void queryClient.invalidateQueries({ queryKey: itemKeys.watchDetail(mediaId) });
  }, [mediaId, discovery.dataUpdatedAt, queryClient]);

  return { discovering: discovery.isFetching, failed: discovery.isError };
}
