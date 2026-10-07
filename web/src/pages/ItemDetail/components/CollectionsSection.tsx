import type { ItemCollection } from "@/api/types";
import MediaCardArtwork from "@/components/MediaCardArtwork";
import MediaCarousel from "@/components/MediaCarousel";
import ViewTransitionLink from "@/components/ViewTransitionLink";
import { useUICustomization } from "@/hooks/useUICustomization";
import { carouselCardWidthClasses } from "@/lib/uiCustomization";
import { buildLibraryCollectionCatalogHref } from "@/pages/catalogSearchParams";

interface CollectionsSectionProps {
  collections?: ItemCollection[];
}

/**
 * The "Collections" row on a movie or series page: one chip per collection the
 * title belongs to, each opening that collection's browse view. The membership
 * list arrives on the item detail itself, so the row has no fetch of its own —
 * the page's loading and error states already cover it — and a title in no
 * collections renders no row.
 */
export default function CollectionsSection({ collections }: CollectionsSectionProps) {
  const { cardPresentation } = useUICustomization();

  if (!collections || collections.length === 0) return null;

  const posterWidthClasses = carouselCardWidthClasses(cardPresentation.poster_size);

  return (
    <MediaCarousel title="Collections" edgePadding={false}>
      {collections.map((collection) => (
        <div key={collection.id} className={posterWidthClasses}>
          <CollectionChip collection={collection} />
        </div>
      ))}
    </MediaCarousel>
  );
}

function CollectionChip({ collection }: { collection: ItemCollection }) {
  const href = buildLibraryCollectionCatalogHref(collection.id, collection.title);
  const countLabel = `${collection.item_count} ${collection.item_count === 1 ? "item" : "items"}`;

  return (
    <ViewTransitionLink to={href} className="group block">
      <div className="block overflow-hidden rounded-xl">
        <MediaCardArtwork
          src={collection.poster_url}
          alt={collection.title}
          fallbackLabel={collection.title}
          thumbhash={collection.poster_thumbhash}
          lazy
        />
      </div>
      <div className="px-0.5 pt-2.5">
        <div className="truncate text-[13px] font-semibold group-hover:underline">
          {collection.title}
        </div>
        <div className="text-muted-foreground mt-0.5 text-[11px]">{countLabel}</div>
      </div>
    </ViewTransitionLink>
  );
}
