import { useEffect, useLayoutEffect, useRef, useState } from "react";
import { Download, Loader2 } from "lucide-react";
import { toast } from "sonner";
import type { FileVersion } from "@/api/types";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from "@/components/ui/tooltip";
import { formatFileSize } from "@/lib/mediaFormat";
import { StaleApiRequestContextError } from "@/api/client";
import {
  DirectDownloadError,
  isStaleFileError,
  prepareDirectDownload,
  type PreparedDirectDownload,
} from "@/api/v2/directDownloads";
import {
  createPreparedDownload,
  downloadPreparationErrorMessage,
  getPreparedDownload,
  isDownloadPreparing,
  isDownloadRefused,
  savePreparedDownload,
  type PreparedDownloadEntry,
  type PreparedDownloadQuality,
} from "@/api/v2/downloadPreparation";
import { deleteDownloadEntry } from "@/api/v2/downloadRegistry";
import { useDownloadCapability, type DownloadQualityOption } from "@/hooks/queries/downloads";
import { isVersionUnavailable } from "@/pages/ItemDetail/components/versionAvailability";
import { isVirtualFileVersion } from "@/pages/ItemDetail/components/versionFormatUtils";
import {
  buildDetailLine,
  buildQualitySummary,
  sortByResolution,
} from "@/pages/ItemDetail/components/VersionFlyout";

interface DownloadVersionPickerProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  versions: FileVersion[];
  title?: string;
  summaryBuilder?: (version: FileVersion) => string;
  /**
   * The file_id the item page currently has selected or playing. Its row gets
   * a visible "Playing" marker so the download choice matches what is on
   * screen. Omit when the caller has no active version.
   */
  selectedFileId?: number | null;
  /**
   * Called when a preflight reports the chosen file_id is stale (no longer
   * resolves). The caller re-fetches the item's versions, the same refresh a
   * `versions_updated` event triggers.
   */
  onStaleVersion?: (fileId: number) => void;
  /**
   * Catalog identity the server needs to create a download for a provider-backed
   * (virtual) version: the movie or series content id, plus the episode content
   * id when these versions belong to an episode. Without it, virtual rows are
   * not offered.
   */
  createTarget?: { contentId: string; episodeId?: string };
}

interface PreparedSelection {
  version: FileVersion;
  download: PreparedDirectDownload;
}

interface PendingVirtualDownload {
  id: string;
  status: string;
}

/** Why the server refuses downloads for this account, or null when it permits them. */
function capabilityDenialReason(
  capability: { enabled: boolean; allowed: boolean } | undefined,
): string | null {
  if (!capability) return null;
  if (!capability.enabled) return "Downloads are turned off on this server.";
  if (!capability.allowed) return "Downloads are not allowed for this account.";
  return null;
}

/** A concrete, user-actionable reason for a failed direct download attempt. */
function downloadFailureMessage(error: unknown): string {
  if (error instanceof DirectDownloadError) return error.message;
  return "Download could not be started. Check your connection and try again.";
}

/** The ordered quality choices a virtual download may request. */
function qualityOptions(
  capability: { quality_options?: DownloadQualityOption[] } | undefined,
): DownloadQualityOption[] {
  const options = capability?.quality_options?.filter((option) => option.preset);
  if (!options || options.length === 0) return [{ preset: "original" }];
  return options;
}

/** A plain-language label for one quality preset: "Original quality" or "10 Mbps · up to 1080p". */
function qualityLabel(option: DownloadQualityOption): string {
  if (option.preset === "original") return "Original quality";
  const rate = option.bitrate_kbps
    ? `${option.bitrate_kbps >= 1000 ? option.bitrate_kbps / 1000 : option.bitrate_kbps} ${
        option.bitrate_kbps >= 1000 ? "Mbps" : "Kbps"
      }`
    : option.preset;
  return option.max_height ? `${rate} · up to ${option.max_height}p` : rate;
}

export default function DownloadVersionPicker({
  open,
  onOpenChange,
  versions,
  title,
  summaryBuilder,
  selectedFileId,
  onStaleVersion,
  createTarget,
}: DownloadVersionPickerProps) {
  const sorted = sortByResolution(versions);
  const [downloading, setDownloading] = useState<number | null>(null);
  const [prepared, setPrepared] = useState<PreparedSelection | null>(null);
  const [refusalMessage, setRefusalMessage] = useState<string | null>(null);
  const [virtualSelection, setVirtualSelection] = useState<FileVersion | null>(null);
  const [virtualQuality, setVirtualQuality] = useState<PreparedDownloadQuality>("original");
  const [submitting, setSubmitting] = useState(false);
  const [preparing, setPreparing] = useState<PendingVirtualDownload | null>(null);
  const [virtualReady, setVirtualReady] = useState<{ id: string; filename: string } | null>(null);
  const {
    data: capability,
    isLoading: capabilityLoading,
    isError: capabilityError,
    refetch: refetchCapability,
  } = useDownloadCapability();
  const denialReason = capabilityDenialReason(capability);
  const options = qualityOptions(capability);

  const active = useRef<symbol | null>(null);
  // One controller drives the in-flight direct-download preflight. Closing the
  // dialog or replacing the file selection aborts it, so no probe outlives its
  // intent.
  const abortRef = useRef<AbortController | null>(null);
  // One controller drives the virtual create request; polling owns its own
  // controller so a slow tick is aborted by the effect cleanup.
  const prepareAbortRef = useRef<AbortController | null>(null);
  // The chosen version's name, retained so a later poll can suggest it as the
  // saved filename when the server does not provide one.
  const pendingFilenameRef = useRef("");

  // A reopen or a versions replacement invalidates any in-flight work and any
  // prepared selection: rows become clickable again and no stale save link
  // survives to be clicked against the wrong file.
  useLayoutEffect(() => {
    abortRef.current?.abort();
    abortRef.current = null;
    prepareAbortRef.current?.abort();
    prepareAbortRef.current = null;
    active.current = null;
    pendingFilenameRef.current = "";
    setDownloading(null);
    setPrepared(null);
    setRefusalMessage(null);
    setVirtualSelection(null);
    setVirtualQuality("original");
    setSubmitting(false);
    setPreparing(null);
    setVirtualReady(null);
    return () => {
      abortRef.current?.abort();
      abortRef.current = null;
      prepareAbortRef.current?.abort();
      prepareAbortRef.current = null;
      active.current = null;
    };
  }, [open, versions]);

  useEffect(
    () => () => {
      abortRef.current?.abort();
      abortRef.current = null;
      prepareAbortRef.current?.abort();
      prepareAbortRef.current = null;
    },
    [],
  );

  // Poll a preparing virtual download until the artifact is ready or refused.
  // The effect is keyed on the download id so a status update on the row does
  // not restart the interval.
  const preparingId = preparing?.id ?? null;
  useEffect(() => {
    if (!preparingId) return;
    const controller = new AbortController();
    const poll = async () => {
      let entry: PreparedDownloadEntry | null = null;
      try {
        entry = await getPreparedDownload(preparingId, controller.signal);
      } catch {
        return; // transient read failure: try again on the next tick
      }
      if (!entry) return;
      if (entry.status === "ready") {
        setVirtualReady({ id: preparingId, filename: pendingFilenameRef.current });
        setPreparing(null);
        return;
      }
      if (isDownloadRefused(entry.status)) {
        const message = "The server could not prepare this download.";
        setRefusalMessage(message);
        toast.error(message);
        setPreparing(null);
      }
    };
    const timer = window.setInterval(() => void poll(), 2000);
    void poll();
    return () => {
      controller.abort();
      window.clearInterval(timer);
    };
  }, [preparingId]);

  const handleDownload = async (version: FileVersion) => {
    if (active.current || !open || prepared) return;
    const attempt = Symbol();
    active.current = attempt;
    const controller = new AbortController();
    abortRef.current = controller;
    const isCurrent = () => active.current === attempt;
    setDownloading(version.file_id);
    setRefusalMessage(null);
    try {
      // Preflight only: probes with a ranged GET and resolves to the
      // authenticated URL. The bytes move on the user's own click of the save
      // link, not here.
      const download = await prepareDirectDownload(
        version.file_id,
        isCurrent,
        controller.signal,
        version.file_name,
      );
      if (isCurrent()) setPrepared({ version, download });
    } catch (error) {
      // A superseded attempt (profile switch, closed dialog, replaced selection)
      // must not report into the replacement authority; every other failure
      // surfaces a concrete reason. A stale file_id also asks the caller to
      // re-fetch the item's versions instead of leaving the user at a dead end.
      if (isCurrent() && !(error instanceof StaleApiRequestContextError)) {
        const message = downloadFailureMessage(error);
        setRefusalMessage(message);
        if (isStaleFileError(error)) {
          onStaleVersion?.(version.file_id);
        }
        toast.error(message);
      }
    } finally {
      if (isCurrent()) {
        active.current = null;
        abortRef.current = null;
        setDownloading(null);
      }
    }
  };

  const handlePrepareVirtual = async () => {
    if (!virtualSelection || !createTarget || active.current) return;
    const version = virtualSelection;
    const attempt = Symbol();
    active.current = attempt;
    const controller = new AbortController();
    prepareAbortRef.current = controller;
    const isCurrent = () => active.current === attempt;
    setRefusalMessage(null);
    setSubmitting(true);
    pendingFilenameRef.current = version.file_name ?? "";
    try {
      const entry = await createPreparedDownload(
        {
          contentId: createTarget.contentId,
          episodeId: createTarget.episodeId,
          mediaFileId: version.file_id,
          quality: virtualQuality,
        },
        controller.signal,
      );
      if (!isCurrent()) return;
      setVirtualSelection(null);
      if (entry.status === "ready") {
        setVirtualReady({ id: String(entry.id), filename: version.file_name ?? "" });
      } else if (isDownloadPreparing(entry.status)) {
        setPreparing({ id: String(entry.id), status: entry.status });
      } else {
        const message = "The server could not prepare this download.";
        setRefusalMessage(message);
        toast.error(message);
      }
    } catch (error) {
      if (isCurrent() && !(error instanceof StaleApiRequestContextError)) {
        const message = downloadPreparationErrorMessage(error);
        setRefusalMessage(message);
        toast.error(message);
      }
    } finally {
      if (isCurrent()) {
        active.current = null;
        prepareAbortRef.current = null;
        setSubmitting(false);
      }
    }
  };

  const handleCancelPrepare = async () => {
    const id = preparing?.id;
    setPreparing(null);
    if (!id) return;
    try {
      await deleteDownloadEntry(id);
    } catch {
      // Cancellation is best-effort: the row may already be gone or the
      // prepare may have completed. Neither is worth interrupting the user.
    }
  };

  const handleSavePrepared = async () => {
    if (!virtualReady) return;
    try {
      await savePreparedDownload(virtualReady.id, virtualReady.filename);
    } catch (error) {
      if (!(error instanceof StaleApiRequestContextError)) {
        const message = downloadPreparationErrorMessage(error);
        setRefusalMessage(message);
        toast.error(message);
      }
      return;
    }
    onOpenChange(false);
  };

  const handleSave = () => {
    // The link's href is the authenticated direct-download URL, so the
    // navigation outlives this click and there is nothing to revoke or dispose.
    // Closing resets the selection; the in-flight probe, if any, is aborted.
    onOpenChange(false);
  };

  const handleSelectVersion = (version: FileVersion) => {
    if (active.current || !open || prepared || virtualReady || preparing) return;
    if (isVirtualFileVersion(version)) {
      if (!createTarget) {
        const message = "This version can't be prepared for download from here.";
        setRefusalMessage(message);
        toast.error(message);
        return;
      }
      setRefusalMessage(null);
      setVirtualSelection(version);
      setVirtualQuality("original");
      return;
    }
    void handleDownload(version);
  };

  // Build the display rows once, then split them: rows whose refusal is
  // knowable from fields already on FileVersion are kept out of the list, and
  // label-less placeholder rows that carry no identity are dropped. A virtual
  // row is offerable now that it can be prepared, so it is no longer excluded.
  const rows = sorted.map((version) => ({
    version,
    quality: summaryBuilder?.(version) || buildQualitySummary(version),
    // A release name (or the pre-`release_name` edition) distinguishes two rows
    // that share a quality summary. buildDetailLine is the same release/size/
    // source line the item-page version picker and the in-player menu show, so
    // the wording stays consistent.
    detailLine: version.release_name || version.edition_raw ? buildDetailLine(version) : "",
    // The detail line already carries the size; only fall back to the bare size
    // when there is no release identity and no custom summary that states it.
    size:
      !(version.release_name || version.edition_raw) && !summaryBuilder
        ? formatFileSize(version.file_size)
        : "",
  }));
  const labeledRows = rows.filter(
    (row) => row.quality.trim() !== "" || row.detailLine.trim() !== "" || row.size.trim() !== "",
  );
  const offerableRows = labeledRows.filter(
    (row) =>
      !isVersionUnavailable(row.version) &&
      (createTarget !== undefined || !isVirtualFileVersion(row.version)),
  );
  const unavailableCount = labeledRows.filter((row) => isVersionUnavailable(row.version)).length;

  const rowsBlocked =
    capabilityLoading ||
    capabilityError ||
    denialReason !== null ||
    downloading !== null ||
    prepared !== null ||
    virtualSelection !== null ||
    submitting ||
    preparing !== null ||
    virtualReady !== null;

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-md">
        <DialogHeader>
          <DialogTitle>Download{title ? `: ${title}` : ""}</DialogTitle>
          <DialogDescription>
            Choose a file to download. Make sure you have enough disk space.
          </DialogDescription>
        </DialogHeader>

        {capabilityLoading && (
          <p className="text-muted-foreground text-sm" role="status">
            Checking download availability…
          </p>
        )}

        {capabilityError && (
          <div className="flex items-center justify-between gap-2" role="alert">
            <p className="text-muted-foreground text-sm">
              Couldn&#39;t check download availability.
            </p>
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={() => void refetchCapability()}
            >
              Retry
            </Button>
          </div>
        )}

        {denialReason && (
          <p className="text-muted-foreground text-sm" role="status">
            {denialReason}
          </p>
        )}

        {refusalMessage && (
          <p className="text-muted-foreground text-sm" role="status">
            {refusalMessage}
          </p>
        )}

        {unavailableCount > 0 && (
          <p className="text-muted-foreground text-sm" role="status">
            {unavailableCount === 1
              ? "1 version is no longer available."
              : `${unavailableCount} versions are no longer available.`}
          </p>
        )}

        {offerableRows.length === 0 && unavailableCount === 0 && (
          <p className="text-muted-foreground text-sm" role="status">
            No downloadable files are available for this item.
          </p>
        )}

        {virtualSelection && createTarget && (
          <div className="border-border/50 bg-accent/30 space-y-3 rounded-xl border px-4 py-3">
            <p className="text-foreground text-sm font-medium">
              Prepare {summaryBuilder?.(virtualSelection) || buildQualitySummary(virtualSelection)}{" "}
              for download.
            </p>
            <label className="text-muted-foreground block text-xs" htmlFor="download-quality">
              Quality
            </label>
            <select
              id="download-quality"
              value={virtualQuality}
              onChange={(event) => setVirtualQuality(event.target.value as PreparedDownloadQuality)}
              className="border-border/60 bg-background text-foreground w-full rounded-lg border px-3 py-2 text-sm"
            >
              {options.map((option) => (
                <option key={option.preset} value={option.preset}>
                  {qualityLabel(option)}
                </option>
              ))}
            </select>
            <p className="text-muted-foreground text-xs">
              This version streams from a provider. It will be prepared on the server before the
              download starts.
            </p>
            <div className="flex gap-2">
              <Button
                type="button"
                size="sm"
                disabled={submitting}
                onClick={() => void handlePrepareVirtual()}
              >
                {submitting ? (
                  <Loader2 className="size-4 animate-spin" />
                ) : (
                  <Download className="size-4" />
                )}
                Prepare download
              </Button>
              <Button
                type="button"
                variant="outline"
                size="sm"
                disabled={submitting}
                onClick={() => setVirtualSelection(null)}
              >
                Cancel
              </Button>
            </div>
          </div>
        )}

        {preparing && (
          <div className="border-border/50 bg-accent/30 space-y-3 rounded-xl border px-4 py-3">
            <p className="text-foreground flex items-center gap-2 text-sm font-medium">
              <Loader2 className="size-4 animate-spin" />
              Preparing your download…
            </p>
            <p className="text-muted-foreground text-xs">
              This can take a few minutes. You can cancel and try again later.
            </p>
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={() => void handleCancelPrepare()}
            >
              Cancel
            </Button>
          </div>
        )}

        {virtualReady && (
          <div className="border-border/50 bg-accent/30 space-y-2 rounded-xl border px-4 py-3">
            <p className="text-foreground text-sm font-medium">Your file is ready.</p>
            <Button
              type="button"
              variant="default"
              size="sm"
              onClick={() => void handleSavePrepared()}
            >
              <Download className="size-4" />
              Save file
            </Button>
          </div>
        )}

        {prepared && (
          <div className="border-border/50 bg-accent/30 space-y-2 rounded-xl border px-4 py-3">
            <p className="text-foreground text-sm font-medium">Your file is ready.</p>
            <Button asChild variant="default" size="sm">
              <a
                href={prepared.download.url}
                download={prepared.download.filename}
                onClick={handleSave}
              >
                <Download className="size-4" />
                Save file
              </a>
            </Button>
          </div>
        )}

        <TooltipProvider delayDuration={0}>
          <div className="space-y-2">
            {offerableRows.map(({ version, quality, detailLine, size }) => {
              const isSelected = selectedFileId != null && version.file_id === selectedFileId;
              const button = (
                <button
                  key={version.file_id}
                  type="button"
                  onClick={() => handleSelectVersion(version)}
                  disabled={rowsBlocked}
                  aria-current={isSelected ? "true" : undefined}
                  className={`bg-accent/30 hover:bg-accent/60 flex w-full items-center gap-3 rounded-xl border px-4 py-3 text-left transition-colors disabled:opacity-50 ${
                    isSelected ? "border-primary/60 bg-primary/5" : "border-border/50"
                  }`}
                >
                  <span className="bg-primary/10 text-primary flex size-9 shrink-0 items-center justify-center rounded-full">
                    {downloading === version.file_id ? (
                      <Loader2 className="size-4 animate-spin" />
                    ) : (
                      <Download className="size-4" />
                    )}
                  </span>
                  <span className="min-w-0 flex-1">
                    <span className="flex items-center gap-2">
                      <span className="text-foreground block text-sm font-medium">{quality}</span>
                      {isSelected && (
                        <Badge
                          variant="secondary"
                          className="shrink-0 px-1.5 py-0 text-[10px] font-medium"
                        >
                          Playing
                        </Badge>
                      )}
                    </span>
                    {detailLine && (
                      <span className="text-muted-foreground block text-xs">{detailLine}</span>
                    )}
                    {size && <span className="text-muted-foreground block text-xs">{size}</span>}
                  </span>
                </button>
              );

              const tooltip =
                denialReason ?? (capabilityError ? "Download availability is unknown." : null);
              if (!tooltip) return button;

              return (
                <Tooltip key={version.file_id}>
                  <TooltipTrigger asChild>
                    <span className="block w-full cursor-not-allowed">{button}</span>
                  </TooltipTrigger>
                  <TooltipContent>{tooltip}</TooltipContent>
                </Tooltip>
              );
            })}
          </div>
        </TooltipProvider>

        {offerableRows.length > 1 && (
          <p className="text-muted-foreground text-xs">Larger files require more storage space.</p>
        )}
      </DialogContent>
    </Dialog>
  );
}
