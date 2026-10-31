/** Compact pill used to tag tracks with codec/channel/default metadata. The
 *  `outline` variant is used for the default marker so it reads as a
 *  qualifier rather than another content facet. */
export function TrackBadge({
  children,
  variant = "solid",
}: {
  children: React.ReactNode;
  variant?: "solid" | "outline";
}) {
  const base =
    "inline-flex items-center rounded px-1.5 py-[1px] text-[9.5px] font-semibold tracking-wide whitespace-nowrap uppercase leading-4";
  const skin =
    variant === "outline" ? "border border-white/25 text-white/70" : "bg-white/10 text-white/75";
  return <span className={`${base} ${skin}`}>{children}</span>;
}
