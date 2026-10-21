import {
  setProviderResultEnrichment,
  useProviderResultEnrichment,
} from "./useProviderResultDisplay";

export function ProviderResultDisplayToggle({
  buttonRef,
}: {
  buttonRef?: (element: HTMLButtonElement | null) => void;
}) {
  const enriched = useProviderResultEnrichment();
  return (
    <button
      ref={buttonRef}
      type="button"
      role="switch"
      aria-checked={enriched}
      className="flex w-full items-center justify-between gap-3 rounded-lg px-3 py-2 text-left text-xs hover:bg-current/10 focus-visible:outline-2"
      onClick={() => setProviderResultEnrichment(!enriched)}
    >
      <span>Enrich with quality, source, and languages</span>
      <span aria-hidden="true">{enriched ? "On" : "Off"}</span>
    </button>
  );
}

export function ProviderResultText({ text }: { text: string }) {
  return <span className="block text-sm break-words whitespace-pre-wrap">{text}</span>;
}
