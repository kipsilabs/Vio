import {
  adminCreateBody,
  adminImportBody,
  adminMutationMessage,
  fetchAdminCollections,
  fetchAdminCollectionSnapshot,
  fetchAdminGroups,
  saveAdminArtwork,
  templateApplyBody,
  templateResultFromV2,
  adminUpdateBody,
} from "@/api/adminCollections";
import { ApiClientError, api } from "@/api/client";
import type {
  CreateLibraryCollectionRequest,
  ImportMDBListCollectionRequest,
  ImportTMDBCollectionRequest,
  ImportTMDBListCollectionRequest,
  ImportTraktCollectionRequest,
  UpdateLibraryCollectionRequest,
} from "@/api/types";
import { requiredETag } from "@/api/v2/etag";
import { adminJobFromV2 } from "@/api/v2/libraries";
import { v2, V2ProblemError } from "@/api/v2/request";
import { useAdminTaskJobs } from "@/hooks/queries/admin/taskJobs";
import type {
  ApplyCollectionTemplateBundleJobRequest,
  ApplyCollectionTemplateBundleRequest,
} from "@/lib/collectionTemplates";
import { SERVER_SCOPE } from "@/lib/collections/scope";
import { queryOptions, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { toast } from "sonner";
import { isTerminalItemDetailNotFound } from "../mediaSurfaceRefresh";
import { runBulkDelete, type BulkDeleteProgress } from "../bulkDelete";
import { invalidateAdminCollectionQueries } from "../collectionSurfaceRefresh";
import { adminKeys, catalogKeys, sectionKeys } from "../keys";

const ADMIN_STALE_TIME = 30_000;

function isLikelyRequestTimeout(error: unknown): boolean {
  if (error instanceof ApiClientError) {
    return (
      error.status === 408 || error.status === 502 || error.status === 503 || error.status === 504
    );
  }
  return error instanceof TypeError;
}

function applyTemplateBundleErrorMessage(error: unknown): string {
  if (isLikelyRequestTimeout(error)) {
    return "The apply request timed out. Silo may still be creating collections; refresh in a minute.";
  }
  return error instanceof Error ? error.message : "Failed to apply defaults";
}

export function useAdminCollectionSnapshot(id?: string) {
  return useQuery({
    queryKey: ["admin", "collections", "edit", id],
    queryFn: () => fetchAdminCollectionSnapshot(id!),
    enabled: !!id,
  });
}

function showArtworkErrors(result: { artworkErrors: string[] }) {
  if (result.artworkErrors.length)
    toast.warning("Collection saved, but artwork could not be saved", {
      description: result.artworkErrors.join(". "),
    });
}

export function useAdminCollectionCapabilities(enabled = true) {
  return useQuery({
    queryKey: SERVER_SCOPE.keys.capabilities,
    queryFn: () => v2("GET /api/v2/admin/collections/capabilities"),
    enabled,
    staleTime: Infinity,
  });
}

export function useAdminCollections(libraryId?: number) {
  return useQuery({
    queryKey: adminKeys.collections(libraryId),
    queryFn: () => fetchAdminCollections(libraryId),
    select: (data) => data.collections,
    staleTime: ADMIN_STALE_TIME,
  });
}

/**
 * A starter pack's dry run: what applying the pack to these libraries would
 * create, keep and leave out. It never deletes existing collections, and
 * carries `featured` only when the admin asked for hero banners.
 */
export function useAdminCollectionGroups(libraryId?: number) {
  return useQuery({
    queryKey: adminKeys.collectionGroups(libraryId),
    queryFn: () => fetchAdminGroups(libraryId!).then((data) => data.groups),
    staleTime: ADMIN_STALE_TIME,
    enabled: libraryId !== undefined,
  });
}

export function useCreateAdminCollection() {
  const queryClient = useQueryClient();

  return useMutation({
    retry: false,
    mutationFn: ({
      body,
      poster,
      backdrop,
    }: {
      body: CreateLibraryCollectionRequest;
      poster?: File | null;
      backdrop?: File | null;
    }) => {
      return v2("POST /api/v2/admin/collections", { body: adminCreateBody(body) }).then((value) =>
        saveAdminArtwork(value, body, poster, backdrop),
      );
    },
    onSuccess: (result) => {
      showArtworkErrors(result);
      toast.success("Collection created");
      void invalidateAdminCollectionQueries(queryClient);
    },
    onError: (error) => {
      toast.error(error instanceof Error ? error.message : "Failed to save");
    },
  });
}

export function useApplyCollectionTemplateBundle() {
  const queryClient = useQueryClient();

  return useMutation({
    retry: false,
    mutationFn: ({
      bundleId,
      body,
    }: {
      bundleId: string;
      body: ApplyCollectionTemplateBundleRequest;
    }) =>
      v2("POST /api/v2/admin/collections/template-bundles/{bundle_id}/apply", {
        path: { bundle_id: bundleId },
        body: templateApplyBody(body),
      }).then(templateResultFromV2),
    onSuccess: (result) => {
      if (!result.dry_run) {
        const created = result.created.length;
        const deleted = result.deleted?.length ?? 0;
        const syncQueued = result.sync_queued?.length ?? 0;
        const failed = result.failed.length;
        const deleteFailed = result.delete_failed?.length ?? 0;
        const failureCount = failed + deleteFailed;
        if (created > 0 || deleted > 0 || syncQueued > 0) {
          const message = [
            deleted > 0 ? `Deleted ${deleted}` : "",
            created > 0 ? `created ${created}` : "",
            syncQueued > 0 ? `queued ${syncQueued} syncs` : "",
            failureCount > 0 ? `${failureCount} failed` : "",
          ]
            .filter(Boolean)
            .join("; ");
          toast.success(message);
        }
        void invalidateAdminCollectionQueries(queryClient);
        void queryClient.invalidateQueries({ queryKey: sectionKeys.all });
      }
    },
    onError: (error) => {
      toast.error(applyTemplateBundleErrorMessage(error));
    },
  });
}

export function useUpdateAdminCollection() {
  const queryClient = useQueryClient();
  return useMutation({
    retry: false,
    mutationFn: ({
      id,
      etag,
      body,
      poster,
      backdrop,
      removeArtwork,
    }: {
      id: string;
      etag: string;
      body: UpdateLibraryCollectionRequest;
      poster?: File | null;
      backdrop?: File | null;
      removeArtwork?: ("poster" | "backdrop")[];
    }) =>
      v2("PATCH /api/v2/admin/collections/{id}", {
        path: { id },
        headers: { "If-Match": requiredETag(etag) },
        body: adminUpdateBody(body),
      }).then((value) => saveAdminArtwork(value, body, poster, backdrop, removeArtwork)),
    onSuccess: (result) => {
      showArtworkErrors(result);
      toast.success("Collection saved");
      void invalidateAdminCollectionQueries(queryClient);
    },
    onError: (error) => {
      toast.error(adminMutationMessage(error, "Failed to save"));
      void invalidateAdminCollectionQueries(queryClient);
    },
  });
}

export function useDeleteAdminCollection() {
  const queryClient = useQueryClient();

  return useMutation({
    retry: false,
    mutationFn: ({ id, libraryId, etag }: { id: string; libraryId: number; etag: string }) =>
      v2("DELETE /api/v2/admin/collections/{id}", {
        path: { id },
        headers: { "If-Match": requiredETag(etag) },
      }).then(() => libraryId),
    onSuccess: (_libraryId) => {
      toast.success("Collection deleted");
      void invalidateAdminCollectionQueries(queryClient);
    },
    onError: (error) => {
      toast.error(adminMutationMessage(error, "Failed to delete"));
      void invalidateAdminCollectionQueries(queryClient);
    },
  });
}

export function starterPackDryRunQuery(
  packId: string,
  body: Pick<ApplyCollectionTemplateBundleRequest, "library_ids" | "featured">,
) {
  return queryOptions({
    queryKey: adminKeys.starterPackDryRun(packId, body),
    queryFn: () =>
      v2("POST /api/v2/admin/collections/template-bundles/{bundle_id}/apply", {
        path: { bundle_id: packId },
        body: templateApplyBody({ ...body, dry_run: true, delete_existing: false }),
      }).then(templateResultFromV2),
    staleTime: 0,
    retry: false,
  });
}

/** A collection job, polled every 2 seconds until it ends. */
export function collectionJobQuery(jobId: string | null) {
  return queryOptions({
    queryKey: ["admin", "collection-job", jobId],
    queryFn: () => v2("GET /api/v2/admin/collection-jobs/{job_id}", { path: { job_id: jobId! } }),
    enabled: !!jobId,
    refetchInterval: (query) => (query.state.data?.terminal ? false : 2000),
  });
}

export function useQueueCollectionTemplateBundleApply() {
  const queryClient = useQueryClient();

  return useMutation({
    retry: false,
    mutationFn: ({
      bundleId,
      body,
    }: {
      bundleId: string;
      body: ApplyCollectionTemplateBundleJobRequest;
    }) =>
      v2("POST /api/v2/admin/collections/template-bundles/{bundle_id}/apply-job", {
        path: { bundle_id: bundleId },
        body: templateApplyBody(body),
      }),
    onSuccess: (job) => {
      queryClient.setQueryData(["admin", "collection-job", "accepted"], job.id);
      void queryClient.invalidateQueries({ queryKey: adminKeys.jobs("template_bundle_apply") });
      void queryClient.invalidateQueries({ queryKey: adminKeys.jobs("__all") });
    },
    onError: (error) => {
      if (error instanceof V2ProblemError && error.status === 409) {
        toast.error("A starter pack is already being added. Try again when it finishes.");
        return;
      }
      toast.error(error instanceof Error ? error.message : "Couldn't add the starter pack");
    },
  });
}

export function useTemplateBundleApplyJobs() {
  const accepted = useQuery<string | null>({
    queryKey: ["admin", "collection-job", "accepted"],
    queryFn: () => null,
    initialData: null,
    staleTime: Infinity,
  });
  const listed = useAdminTaskJobs("template_bundle_apply", 10);
  const job = useQuery(collectionJobQuery(accepted.data));
  const current = job.data
    ? {
        ...adminJobFromV2(job.data),
        result_payload: job.data.template_result
          ? templateResultFromV2(job.data.template_result)
          : {},
      }
    : null;
  return {
    ...listed,
    data: current
      ? [current, ...(listed.data ?? []).filter((row) => row.id !== current.id)].sort(
          (a, b) => Date.parse(b.requested_at) - Date.parse(a.requested_at),
        )
      : listed.data,
  };
}

/**
 * A list's one-field change. It reads the collection fresh for its ETag and
 * type, then sends only `collection_type` and `field`, so nothing else on the
 * collection can be overwritten by a stale list.
 */
export async function patchAdminCollectionField(
  id: string,
  field: { visibility: "visible" | "hidden" } | { featured: boolean },
) {
  const { collection, etag } = await fetchAdminCollectionSnapshot(id);
  await v2("PATCH /api/v2/admin/collections/{id}", {
    path: { id },
    headers: { "If-Match": requiredETag(etag) },
    body: { collection_type: collection.collection_type, ...field },
  });
}

/** The list's Collections tab switch. */
export function useSetAdminCollectionVisibility() {
  const queryClient = useQueryClient();
  return useMutation({
    retry: false,
    mutationFn: ({ id, visible }: { id: string; visible: boolean }) =>
      patchAdminCollectionField(id, { visibility: visible ? "visible" : "hidden" }),
    onError: (error) => {
      toast.error(SERVER_SCOPE.errorMessage(error, "Couldn't change it"));
    },
    onSettled: () => SERVER_SCOPE.invalidate(queryClient),
  });
}

/** Arrange's Pin to the start of its shelf (`featured`). Settles once the lists are read again. */
export function useSetAdminCollectionPin() {
  const queryClient = useQueryClient();
  return useMutation({
    retry: false,
    mutationFn: ({ id, pinned }: { id: string; pinned: boolean }) =>
      patchAdminCollectionField(id, { featured: pinned }),
    onError: (error, { pinned }) => {
      toast.error(
        SERVER_SCOPE.errorMessage(error, pinned ? "Couldn't pin it" : "Couldn't unpin it"),
      );
    },
    onSettled: () => SERVER_SCOPE.invalidate(queryClient),
  });
}

export function useDeleteAdminCollections() {
  const queryClient = useQueryClient();
  const [progress, setProgress] = useState<BulkDeleteProgress | null>(null);

  const mutation = useMutation({
    onMutate: (ids) => {
      setProgress({ completed: 0, total: new Set(ids.map((entry) => entry.id)).size });
    },
    mutationFn: (snapshots: { id: string; etag: string }[]) =>
      runBulkDelete(
        snapshots.map((entry) => entry.id),
        (id) =>
          v2("DELETE /api/v2/admin/collections/{id}", {
            path: { id },
            headers: { "If-Match": requiredETag(snapshots.find((entry) => entry.id === id)?.etag) },
          }).catch((error) => {
            if (error instanceof V2ProblemError && error.status === 412)
              throw new Error(adminMutationMessage(error, "Failed to delete"));
            throw error;
          }),
        (error) => {
          if (error instanceof V2ProblemError && error.status === 404) {
            return "deleted";
          }
          // 409 (problem type "conflict"): a row still shows it.
          if (error instanceof V2ProblemError && error.status === 409) {
            return "kept";
          }
          return "failed";
        },
        setProgress,
      ),
    onSuccess: async ({ requested, deleted, kept, failed, firstError }) => {
      const keptDescription = `Kept ${kept} collection${kept === 1 ? "" : "s"} in use by home or library sections`;

      if (failed === 0 && kept === 0) {
        toast.success(`Deleted ${deleted} collection${deleted === 1 ? "" : "s"}`);
      } else if (failed === 0) {
        toast.warning(`Deleted ${deleted} collection${deleted === 1 ? "" : "s"}`, {
          description: keptDescription,
        });
      } else if (deleted > 0 || kept > 0) {
        toast.warning(`Deleted ${deleted} of ${requested} collections`, {
          description: [kept > 0 ? keptDescription : "", firstError].filter(Boolean).join(". "),
        });
      } else {
        toast.error(`Failed to delete ${failed} collection${failed === 1 ? "" : "s"}`, {
          description: firstError,
        });
      }
      await invalidateAdminCollectionQueries(queryClient);
    },
    onSettled: () => {
      setProgress(null);
    },
  });

  return { ...mutation, progress };
}

export function useSyncAdminCollection() {
  const queryClient = useQueryClient();

  return useMutation({
    retry: false,
    mutationFn: ({ id, libraryId }: { id: string; libraryId: number }) =>
      v2("POST /api/v2/admin/collections/{id}/sync", { path: { id } }).then((data) => ({
        data,
        libraryId,
      })),
    onSuccess: ({ data, libraryId: _libraryId }) => {
      toast.success(
        data.status === "warning" ? "Collection synced with warnings" : "Collection synced",
      );
      void invalidateAdminCollectionQueries(queryClient);
    },
    onError: (error) => {
      toast.error(error instanceof Error ? error.message : "Sync failed");
    },
  });
}

export function useImportMDBListCollection() {
  const queryClient = useQueryClient();

  return useMutation({
    retry: false,
    mutationFn: ({
      body,
      poster,
      backdrop,
    }: {
      body: ImportMDBListCollectionRequest;
      poster?: File | null;
      backdrop?: File | null;
    }) => {
      return v2("POST /api/v2/admin/collections/import/mdblist", {
        body: adminImportBody(body),
      }).then(async (result) => ({
        ...result,
        ...(await saveAdminArtwork(result.collection, body, poster, backdrop)),
      }));
    },
    onSuccess: (result) => {
      showArtworkErrors(result);
      toast.success(
        result.sync_run?.status === "warning"
          ? "MDBList imported with warnings"
          : "MDBList imported",
      );
      void invalidateAdminCollectionQueries(queryClient);
    },
    onError: (error) => {
      toast.error(error instanceof Error ? error.message : "Import failed");
    },
  });
}

export function useImportTMDBCollection() {
  const queryClient = useQueryClient();

  return useMutation({
    retry: false,
    mutationFn: ({
      body,
      poster,
      backdrop,
    }: {
      body: ImportTMDBCollectionRequest;
      poster?: File | null;
      backdrop?: File | null;
    }) => {
      return v2("POST /api/v2/admin/collections/import/tmdb", { body: adminImportBody(body) }).then(
        async (result) => ({
          ...result,
          ...(await saveAdminArtwork(result.collection, body, poster, backdrop)),
        }),
      );
    },
    onSuccess: (result) => {
      showArtworkErrors(result);
      toast.success(
        result.sync_run?.status === "warning"
          ? "TMDB collection imported with warnings"
          : "TMDB collection imported",
      );
      void invalidateAdminCollectionQueries(queryClient);
    },
    onError: (error) => {
      toast.error(error instanceof Error ? error.message : "Import failed");
    },
  });
}

export function useImportTMDBListCollection() {
  const queryClient = useQueryClient();

  return useMutation({
    retry: false,
    mutationFn: ({
      body,
      poster,
      backdrop,
    }: {
      body: ImportTMDBListCollectionRequest;
      poster?: File | null;
      backdrop?: File | null;
    }) => {
      return v2("POST /api/v2/admin/collections/import/tmdb-list", {
        body: adminImportBody(body),
      }).then(async (result) => ({
        ...result,
        ...(await saveAdminArtwork(result.collection, body, poster, backdrop)),
      }));
    },
    onSuccess: (result) => {
      showArtworkErrors(result);
      toast.success(
        result.sync_run?.status === "warning"
          ? "TMDB list imported with warnings"
          : "TMDB list imported",
      );
      void invalidateAdminCollectionQueries(queryClient);
    },
    onError: (error) => {
      toast.error(error instanceof Error ? error.message : "Import failed");
    },
  });
}

export function useImportTraktCollection() {
  const queryClient = useQueryClient();

  return useMutation({
    retry: false,
    mutationFn: ({
      body,
      poster,
      backdrop,
    }: {
      body: ImportTraktCollectionRequest;
      poster?: File | null;
      backdrop?: File | null;
    }) => {
      return v2("POST /api/v2/admin/collections/import/trakt", {
        body: {
          ...adminImportBody(body),
          profile_id: body.profile_id === undefined ? undefined : String(body.profile_id),
        },
      }).then(async (result) => ({
        ...result,
        ...(await saveAdminArtwork(result.collection, body, poster, backdrop)),
      }));
    },
    onSuccess: (result) => {
      showArtworkErrors(result);
      const statusMessages: Record<string, string> = {
        warning: "Trakt collection imported with warnings",
        failed: "Trakt collection imported but sync failed",
      };
      const status = result.sync_run?.status ?? "";
      toast.success(statusMessages[status] ?? "Trakt collection imported");
      void invalidateAdminCollectionQueries(queryClient);
    },
    onError: (error) => {
      toast.error(error instanceof Error ? error.message : "Import failed");
    },
  });
}

export function usePurgeVirtualPlaybackItems() {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: (options?: { dryRun?: boolean; libraryId?: number }) =>
      api<{ success: boolean; files_deleted: number; items_deleted: number; message: string }>(
        `/admin/collections/purge-virtual?${new URLSearchParams({
          ...(options?.dryRun ? { dry_run: "true" } : {}),
          ...(options?.libraryId ? { library_id: String(options.libraryId) } : {}),
        })}`,
        { method: "POST" },
      ),
    onSuccess: (result) => {
      toast.success(result.message);
      void invalidateAdminCollectionQueries(queryClient);
      void queryClient.invalidateQueries({ queryKey: sectionKeys.all });
      void queryClient.invalidateQueries({
        queryKey: catalogKeys.all,
        predicate: (query) => !isTerminalItemDetailNotFound(query),
      });
    },
    onError: (error) => {
      toast.error(error instanceof Error ? error.message : "Virtual library purge failed");
    },
  });
}
