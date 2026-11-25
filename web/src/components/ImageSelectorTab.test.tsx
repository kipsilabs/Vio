import { render } from "@testing-library/react";
import { expect, it, vi } from "vitest";

import type { ItemDetail } from "@/api/types";
import ImageSelectorTab from "./ImageSelectorTab";

const posterURL =
  "https://images.theposterdb.com/prod/public/images/posters/optimized/movies/2429765/pznhKMFj2ygmkcKlmVmEvJS8ZYtdKnd5G9Oqb2A4.jpg";

vi.mock("@/hooks/queries/items", () => ({
  useItemImages: () => ({
    data: {
      images: [
        {
          height: 3000,
          language: "en",
          original_url: posterURL,
          provider_id: "theposterdb",
          rating: 0,
          type: "poster",
          url: posterURL,
          width: 2000,
        },
      ],
      current: {},
    },
    isLoading: false,
    isError: false,
  }),
  useApplyItemImage: () => ({ mutate: vi.fn(), isPending: false }),
  useUploadItemImage: () => ({ mutate: vi.fn(), isPending: false }),
}));

vi.mock("sonner", () => ({ toast: { error: vi.fn() } }));

it("requests provider artwork without a referer", () => {
  const { container } = render(
    <ImageSelectorTab item={{ content_id: "movie-1", type: "movie" } as ItemDetail} enabled />,
  );

  const image = container.querySelector("img");
  expect(image).toHaveAttribute("referrerpolicy", "no-referrer");
});
