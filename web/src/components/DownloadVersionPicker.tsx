import { useEffect, useLayoutEffect, useRef, useState } from "react";
import { Download, Loader2 } from "lucide-react";
import { toast } from "sonner";
import type { FileVersion } from "@/api/types";
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
  prepareDirectDownload,
  type PreparedDirectDownload,
} from "@/api/v2/directDownloads";
import { useDownloadCapability } from "@/hooks/queries/downloads";
import { buildQualitySummary, sortByResolution } from "@/pages/ItemDetail/components/VersionFlyout";

interface DownloadVersionPickerProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  versions: FileVersion[];
  title?: string;
  summaryBuilder?: (version: FileVersion) => string;
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

export default function DownloadVersionPicker({
  open,
  onOpenChange,
  versions,
  title,
  summaryBuilder,
}: DownloadVersionPickerProps) {
  const sorted = sortByResolution(versions);
  const [downloading, setDownloading] = useState<number | null>(null);
  const [prepared, setPrepared] = useState<PreparedSelection | null>(null);
  const {
    data: capability,
    isLoading: capabilityLoading,
    isError: capabilityError,
    refetch: refetchCapability,
  } = useDownloadCapability();
  const denialReason = capabilityDenialReason(capability);

  const active = useRef<symbol | null>(null);
  // The object URL must be released exactly once, whether the user saves it,
  // closes the dialog, or switches files underneath it.
  const preparedRef = useRef<PreparedDirectDownload | null>(null);
  const releasePrepared = () => {
    preparedRef.current?.dispose();
    preparedRef.current = null;
    setPrepared(null);
  };

  useLayoutEffect(() => {
    active.current = null;
    return () => {
      active.current = null;
    };
  }, [open, versions]);

  useEffect(() => {
    if (open) return;
    preparedRef.current?.dispose();
    preparedRef.current = null;
    setPrepared(null);
  }, [open]);

  useEffect(
    () => () => {
      preparedRef.current?.dispose();
      preparedRef.current = null;
    },
    [],
  );

  const handleDownload = async (version: FileVersion) => {
    if (active.current || !open || prepared) return;
    const attempt = Symbol();
    active.current = attempt;
    const isCurrent = () => active.current === attempt;
    setDownloading(version.file_id);
    try {
      const download = await prepareDirectDownload(version.file_id, isCurrent, version.file_name);
      if (isCurrent()) {
        preparedRef.current?.dispose();
        preparedRef.current = download;
        setPrepared({ version, download });
      } else {
        download.dispose();
      }
    } catch (error) {
      // A superseded attempt (profile switch, closed dialog) must not report
      // into the replacement authority; every other failure toasts a reason.
      if (isCurrent() && !(error instanceof StaleApiRequestContextError))
        toast.error(downloadFailureMessage(error));
    } finally {
      if (isCurrent()) {
        active.current = null;
        setDownloading(null);
      }
    }
  };

  const handleSave = () => {
    const selection = prepared;
    if (!selection) return;
    releasePrepared();
    onOpenChange(false);
  };

  const rowsBlocked =
    capabilityLoading ||
    capabilityError ||
    denialReason !== null ||
    downloading !== null ||
    prepared !== null;

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
            {sorted.map((version) => {
              const quality = summaryBuilder?.(version) || buildQualitySummary(version);
              const size = summaryBuilder ? "" : formatFileSize(version.file_size);
              const button = (
                <button
                  key={version.file_id}
                  type="button"
                  onClick={() => handleDownload(version)}
                  disabled={rowsBlocked}
                  className="border-border/50 bg-accent/30 hover:bg-accent/60 flex w-full items-center gap-3 rounded-xl border px-4 py-3 text-left transition-colors disabled:opacity-50"
                >
                  <span className="bg-primary/10 text-primary flex size-9 shrink-0 items-center justify-center rounded-full">
                    {downloading === version.file_id ? (
                      <Loader2 className="size-4 animate-spin" />
                    ) : (
                      <Download className="size-4" />
                    )}
                  </span>
                  <span className="min-w-0 flex-1">
                    <span className="text-foreground block text-sm font-medium">{quality}</span>
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

        {sorted.length > 1 && (
          <p className="text-muted-foreground text-xs">Larger files require more storage space.</p>
        )}
      </DialogContent>
    </Dialog>
  );
}
