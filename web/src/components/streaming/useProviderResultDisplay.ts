import { useSyncExternalStore } from "react";

const STORAGE_KEY = "vio.enrich-provider-results";
let sessionEnrichment: boolean | undefined;
const CHANGE_EVENT = "vio-provider-result-display";

function readEnrichment(): boolean {
  if (sessionEnrichment !== undefined) return sessionEnrichment;
  try {
    return localStorage.getItem(STORAGE_KEY) === "true";
  } catch {
    return false;
  }
}

function subscribe(onChange: () => void) {
  window.addEventListener("storage", onChange);
  window.addEventListener(CHANGE_EVENT, onChange);
  return () => {
    window.removeEventListener("storage", onChange);
    window.removeEventListener(CHANGE_EVENT, onChange);
  };
}

export function useProviderResultEnrichment() {
  const enriched = useSyncExternalStore(subscribe, readEnrichment, () => false);
  return enriched;
}

export function setProviderResultEnrichment(enriched: boolean) {
  try {
    localStorage.setItem(STORAGE_KEY, String(enriched));
    sessionEnrichment = undefined;
  } catch {
    // A blocked or full storage still permits a session-only choice.
    sessionEnrichment = enriched;
  }
  window.dispatchEvent(new Event(CHANGE_EVENT));
}

/** Virtual candidate labels are stored verbatim in edition_raw. */
export function providerResultText(version: {
  container?: string;
  file_path?: string;
  edition_raw?: string;
}): string | undefined {
  const virtual =
    version.container === "virtual" || version.file_path?.toLowerCase().startsWith("virtual://");
  const text = version.edition_raw;
  // Older rows sometimes used the internal candidate URI as their label.
  if (!virtual || !text?.trim() || /^virtual:\/\//i.test(text.trim())) return undefined;
  return text;
}
