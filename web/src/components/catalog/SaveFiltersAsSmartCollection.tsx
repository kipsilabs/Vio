import { useId } from "react";
import { BookmarkPlus } from "lucide-react";

import { Button } from "@/components/ui/button";
import ViewTransitionLink from "@/components/ViewTransitionLink";
import { buildSaveAsSmartCollectionHref } from "@/pages/catalogSearchParams";
import type { CatalogSearchState } from "@/pages/catalogSearchParams";

/**
 * Deep link from the catalog filter surface into the smart-collection editor,
 * seeded with the current structured filters. A text search narrows the view
 * but is not part of the query definition, so it is excluded — the notice
 * says so before the viewer follows the link.
 */
export default function SaveFiltersAsSmartCollection({ state }: { state: CatalogSearchState }) {
  const noticeId = useId();
  const excludesTextSearch = Boolean(state.q);

  return (
    <div className="flex flex-wrap items-center justify-end gap-2">
      {excludesTextSearch ? (
        <p id={noticeId} className="text-muted-foreground text-xs">
          Text search isn&rsquo;t included — only the filters are saved.
        </p>
      ) : null}
      <Button asChild variant="outline" size="sm">
        <ViewTransitionLink
          to={buildSaveAsSmartCollectionHref(state)}
          aria-describedby={excludesTextSearch ? noticeId : undefined}
        >
          <BookmarkPlus className="mr-2 h-4 w-4" />
          Save filters as smart collection
        </ViewTransitionLink>
      </Button>
    </div>
  );
}
