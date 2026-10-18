import { useQuery, useQueryClient } from "@tanstack/react-query";
import { discoverVirtualCandidates, prepareVirtualPlayback } from "@/api/v2/mediaCandidates";
import { fetchVirtualLibraryCapability } from "@/api/v2/virtualLibrary";
import { catalogKeys, itemKeys } from "@/hooks/queries/keys";

export function usePreplayDiscovery(
  mediaId: string | undefined,
  enabled: boolean,
  preparationFileId?: number,
  explicitPreparation = false,
) {
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
      await Promise.all([
        queryClient.invalidateQueries({
          queryKey: catalogKeys.itemDetail(mediaId!).slice(0, 4),
        }),
        queryClient.invalidateQueries({ queryKey: itemKeys.watchDetail(mediaId!) }),
      ]);
      return true;
    },
    enabled: enabled && !!mediaId && capability.data?.discover_before_playback === true,
    staleTime: 30_000,
    retry: false,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
  });

  useQuery({
    queryKey: ["virtual-candidates", "prepare", mediaId, preparationFileId],
    queryFn: async ({ signal }) => {
      await prepareVirtualPlayback(mediaId!, preparationFileId!, signal);
      return true;
    },
    enabled:
      enabled &&
      !!mediaId &&
      !!preparationFileId &&
      capability.data?.prepare_before_playback === true &&
      (explicitPreparation ||
        (!discovery.isFetching &&
          (capability.data?.discover_before_playback !== true || discovery.isSuccess))),
    staleTime: 120_000,
    retry: false,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
  });

  return { discovering: discovery.isFetching, failed: discovery.isError };
}
