import type { ReactNode } from "react";

import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import "@/styles/admin-settings.css";

interface SaveBarProps {
  dirtyCount: number;
  onSave: () => void;
  onDiscard: () => void;
  isSaving: boolean;
  saveLabel?: string;
  discardLabel?: string;
  /**
   * False while nothing staged can be saved as it stands, e.g. every edit is
   * waiting on a reload after another admin's change. Discard stays available.
   */
  canSave?: boolean;
  /** Replaces the default "N unsaved changes", e.g. to name the staged fields. */
  message?: ReactNode;
  /**
   * `"settings"` is the admin settings pill, centred beside the admin
   * sidebar. `"page"` docks a full-width bar along the bottom of the page
   * container in either shell, above the background playback bar.
   */
  placement?: "settings" | "page";
}

function plural(count: number, word: string) {
  return `${count} ${word}${count === 1 ? "" : "s"}`;
}

/**
 * The floating save bar: what is staged and the two actions. Hidden while
 * the page is clean, so a page with nothing staged has no permanent furniture at
 * the bottom of the viewport. It says nothing about restarts — the one restart
 * prompt is `RestartBanner`, rendered once by the admin shell at the top of
 * every admin page.
 */
export function SaveBar({
  dirtyCount,
  onSave,
  onDiscard,
  isSaving,
  saveLabel = "Save",
  discardLabel = "Discard",
  canSave = true,
  message,
  placement = "settings",
}: SaveBarProps) {
  if (dirtyCount <= 0) return null;

  const onPage = placement === "page";
  const text = message ?? plural(dirtyCount, "unsaved change");
  const saveDisabled = isSaving || !canSave;
  const saveText = isSaving ? "Saving..." : saveLabel;

  return (
    <>
      {/* In-flow scroll room. The bar is fixed, so without this the last row of
          the page parks under it at full scroll and cannot be reached. Sized to
          clear the bar (bottom 1.5rem + 3rem tall) with margin. */}
      <div aria-hidden="true" className="h-28" />
      {/* Scrim so page content dissolves under the bar instead of colliding.
          Stops at the desktop sidebar so it never tints the nav. */}
      <div
        aria-hidden="true"
        className={cn(
          "pointer-events-none fixed right-0 bottom-0 left-0 z-30 h-40 bg-gradient-to-t from-[var(--background)] via-[color-mix(in_srgb,var(--background)_72%,transparent)] to-transparent",
          onPage ? "left-[var(--app-sidebar-offset,0px)]" : "lg:left-[240px]",
        )}
      />
      {onPage ? (
        // Starts at whichever sidebar edge the shell publishes in
        // `--app-sidebar-offset` and rises by the `--main-inset-bottom` the
        // shell sets on <main> while the background playback bar shows. The
        // bar keeps the page container's 1000px measure, so it lines up with
        // the content above it. Only the message is announced; the buttons
        // stay out of the live region.
        <div className="pointer-events-none fixed right-0 bottom-[calc(var(--main-inset-bottom,0px)+0.75rem)] left-[var(--app-sidebar-offset,0px)] z-40 flex justify-center px-3 sm:px-6 lg:bottom-[calc(var(--main-inset-bottom,0px)+1.125rem)] lg:px-8 xl:px-10">
          <div
            role="region"
            aria-label="Unsaved changes"
            className="bg-popover/95 border-border pointer-events-auto flex w-full max-w-[1000px] items-center justify-between gap-2 rounded-2xl border py-2 pr-2 pl-4 shadow-2xl backdrop-blur-xl"
          >
            <span role="status" className="flex min-w-0 items-center gap-2 text-sm font-medium">
              <span aria-hidden="true" className="bg-warning size-[7px] shrink-0 rounded-full" />
              <span className="line-clamp-2 min-w-0 lg:line-clamp-1">{text}</span>
            </span>
            <span className="flex shrink-0 items-center gap-2">
              <Button variant="ghost" className="h-11 lg:h-8" onClick={onDiscard}>
                {discardLabel}
              </Button>
              <Button className="h-11 lg:h-8" onClick={() => onSave()} disabled={saveDisabled}>
                {saveText}
              </Button>
            </span>
          </div>
        </div>
      ) : (
        // `lg:left-[240px]` matches AdminLayout's `lg:ml-[240px]`: the pill
        // centers over the content column, not the whole viewport, and stays
        // clear of the sidebar it would otherwise paint under.
        <div
          role="status"
          className="pointer-events-none fixed right-0 bottom-6 left-0 z-40 flex justify-center px-4 lg:left-[240px]"
        >
          <div className="glass pointer-events-auto flex max-w-full items-center gap-3 rounded-full py-2 pr-2 pl-4 shadow-2xl backdrop-blur-xl sm:gap-4 sm:pl-5">
            <span className="min-w-0 truncate text-[13px] font-medium">{text}</span>
            <span className="flex shrink-0 items-center gap-1.5">
              <Button variant="ghost" size="sm" className="rounded-full" onClick={onDiscard}>
                {discardLabel}
              </Button>
              <Button
                size="sm"
                onClick={() => onSave()}
                disabled={saveDisabled}
                className="rounded-full bg-[var(--settings-accent)] text-[#15151a] hover:bg-[var(--settings-accent)] hover:brightness-110"
              >
                {saveText}
              </Button>
            </span>
          </div>
        </div>
      )}
    </>
  );
}
