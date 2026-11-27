import { cn } from "@/lib/utils";
import { useBranding } from "@/hooks/useBranding";

/** White-text wordmark; Vio only paints dark surfaces. */
const VIO_WORDMARK_SRC = "/vio-wordmark-sidebar.png";
const VIO_MARK_SRC = "/vio-icon-1024.png";

export type SiloBrandVariant = "wordmark" | "mark";

interface SiloBrandProps {
  className?: string;
  imageClassName?: string;
  variant?: SiloBrandVariant;
}

export function SiloBrand({ className, imageClassName, variant = "wordmark" }: SiloBrandProps) {
  const isMark = variant === "mark";
  const { serverName, wordmarkUrl, markUrl } = useBranding();
  const src = isMark ? (markUrl ?? VIO_MARK_SRC) : (wordmarkUrl ?? VIO_WORDMARK_SRC);

  return (
    <span className={cn("block shrink-0", !isMark && "overflow-hidden", className)}>
      <img
        src={src}
        alt={serverName}
        className={cn("h-full w-full object-contain", isMark && "rounded-lg", imageClassName)}
      />
    </span>
  );
}
