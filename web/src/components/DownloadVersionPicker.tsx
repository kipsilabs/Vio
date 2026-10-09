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
import { useDownloadCapability } from "@/hooks/queries/downloads";
import {
  buildDetailLine,
  buildQualitySummary,
  sortByResolution,
} from "@/pages/ItemDetail/components/VersionFlyout";
import { isVersionUnavailable } from "@/pages/ItemDetail/components/versionAvailability";
import { isVirtualFileVersion } from "@/pages/ItemDetail/components/versionFormatUtils";

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
}

interface PreparedSelection {
  version: FileVersion;
  download: PreparedDirectDownload;
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

/** A concrete, user-actionable reason for a failed download attempt. */
function downloadFailureMessage(error: unknown): string {
  if (error instanceof DirectDownloadError) return error.message;
  return "Download could not be started. Check your connection and try again.";
}

/**
 * The reason a direct download of this version can never succeed, or null when
 * it can be offered. Both signals come from fields the item's versions already
 * carry, so the dialog never offers a row the preflight is known to refuse: a
 * virtual (provider-backed) version has no bytes on this host, and
 * `available === false` is the catalog's own liveness verdict.
 */
function versionDownloadRefusal(version: FileVersion): string | null {
  if (isVirtualFileVersion(version)) return "virtual";
  if (isVersionUnavailable(version)) return "unavailable";
  return null;
}

export default function DownloadVersionPicker({
  open,
  onOpenChange,
  versions,
  title,
  summaryBuilder,
  selectedFileId,
  onStaleVersion,
}: DownloadVersionPickerProps) {
  const sorted = sortByResolution(versions);
  const [downloading, setDownloading] = useState<number | null>(null);
  const [prepared, setPrepared] = useState<PreparedSelection | null>(null);
  const [refusalMessage, setRefusalMessage] = useState<string | null>(null);
  const {
    data: capability,
    isLoading: capabilityLoading,
    isError: capabilityError,
    refetch: refetchCapability,
  } = useDownloadCapability();
  const denialReason = capabilityDenialReason(capability);

  const active = useRef<symbol | null>(null);
  // One controller drives the in-flight preflight. Closing the dialog or
  // replacing the file selection aborts it, so no probe outlives its intent.
  const abortRef = useRef<AbortController | null>(null);

  // A reopen or a versions replacement invalidates any in-flight probe and any
  // prepared selection: rows become clickable again and no stale save link
  // survives to be clicked against the wrong file.
  useLayoutEffect(() => {
    abortRef.current?.abort();
    abortRef.current = null;
    active.current = null;
    setDownloading(null);
    setPrepared(null);
    setRefusalMessage(null);
    return () => {
      abortRef.current?.abort();
      abortRef.current = null;
      active.current = null;
    };
  }, [open, versions]);

  useEffect(
    () => () => {
      abortRef.current?.abort();
      abortRef.current = null;
    },
    [],
  );

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

  const handleSave = () => {
    // The link's href is the authenticated direct-download URL, so the
    // navigation outlives this click and there is nothing to revoke or dispose.
    // Closing resets the selection; the in-flight probe, if any, is aborted.
    onOpenChange(false);
  };

  const rowsBlocked =
    capabilityLoading ||
    capabilityError ||
    denialReason !== null ||
    downloading !== null ||
    prepared !== null;

  // Build the display rows once, then split them three ways: offerable rows,
  // rows whose refusal is knowable up front (kept out of the list so the
  // dialog never presents a dead choice), and label-less placeholders that
  // carry no quality, release identity, or size and are dropped.
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
    refusal: versionDownloadRefusal(version),
  }));
  const labeledRows = rows.filter(
    (row) => row.quality.trim() !== "" || row.detailLine.trim() !== "" || row.size.trim() !== "",
  );
  const downloadableRows = labeledRows.filter((row) => row.refusal === null);
  const blockedCount = labeledRows.length - downloadableRows.length;
  const virtualBlockedCount = labeledRows.filter((row) => isVirtualFileVersion(row.version)).length;
  const unavailableBlockedCount = blockedCount - virtualBlockedCount;

  const virtualNote =
    virtualBlockedCount === 0
      ? null
      : downloadableRows.length === 0
        ? "This item's versions stream from a provider and can't be downloaded directly. Play it instead."
        : `${virtualBlockedCount} virtual ${
            virtualBlockedCount === 1 ? "version streams" : "versions stream"
          } from a provider and ${virtualBlockedCount === 1 ? "isn't" : "aren't"} listed.`;
  const unavailableNote =
    unavailableBlockedCount === 0
      ? null
      : `${unavailableBlockedCount} ${
          unavailableBlockedCount === 1 ? "version is" : "versions are"
        } no longer available.`;

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

        {(virtualNote ||
          unavailableNote ||
          (downloadableRows.length === 0 && labeledRows.length === 0)) && (
          <div className="space-y-1" role="status">
            {virtualNote && <p className="text-muted-foreground text-sm">{virtualNote}</p>}
            {unavailableNote && <p className="text-muted-foreground text-sm">{unavailableNote}</p>}
            {downloadableRows.length === 0 && labeledRows.length === 0 && (
              <p className="text-muted-foreground text-sm">
                No downloadable files are available for this item.
              </p>
            )}
          </div>
        )}

        <TooltipProvider delayDuration={0}>
          <div className="space-y-2">
            {downloadableRows.map(({ version, quality, detailLine, size }) => {
              const isSelected = selectedFileId != null && version.file_id === selectedFileId;
              const button = (
                <button
                  key={version.file_id}
                  type="button"
                  onClick={() => handleDownload(version)}
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

        {downloadableRows.length > 1 && (
          <p className="text-muted-foreground text-xs">Larger files require more storage space.</p>
        )}
      </DialogContent>
    </Dialog>
  );
}
