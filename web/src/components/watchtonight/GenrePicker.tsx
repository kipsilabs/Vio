import { ADVANCED_FILTER_GENRES } from "@/components/advancedFilterOptions";
import { cn } from "@/lib/utils";

interface GenrePickerProps {
  selected: string[];
  onChange: (genres: string[]) => void;
}

export default function GenrePicker({ selected, onChange }: GenrePickerProps) {
  const selectedSet = new Set(selected);

  function toggle(genre: string) {
    if (selectedSet.has(genre)) {
      onChange(selected.filter((g) => g !== genre));
    } else {
      onChange([...selected, genre]);
    }
  }

  return (
    <div className="flex flex-wrap gap-2">
      {ADVANCED_FILTER_GENRES.map((genre) => {
        const isSelected = selectedSet.has(genre);
        return (
          <button
            key={genre}
            type="button"
            onClick={() => toggle(genre)}
            className={cn(
              "rounded-full border px-3.5 py-1.5 text-sm font-medium transition-colors",
              isSelected
                ? "border-primary bg-primary/15 text-primary"
                : "border-border text-muted-foreground hover:border-foreground/30 hover:text-foreground",
            )}
          >
            {genre}
          </button>
        );
      })}
    </div>
  );
}
