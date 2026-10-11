/**
 * Whether a plan's track inventory is still provisional.
 *
 * The plan publishes `inventory_status` ("declared" until a probe persists,
 * then "verified") and, for a virtual source, `inventory_provenance` from the
 * resolve that produced it ("declared", "pending", "verified", or "failed").
 * The status is stamp-derived, so a stale stamp can read "verified" while the
 * resolve actually served provider-declared metadata; provenance is
 * authoritative when present.
 *
 * The only confident state is positive probe evidence: an explicit "verified"
 * provenance, or no provenance plus a status that is not "declared". A failed
 * probe serves declared metadata too, so it stays provisional. Missing fields
 * (older servers, non-virtual sources) keep the previous confident behavior.
 */
/**
 * Reads the deferred-track-inventory state of a plan into one place.
 *
 * `tracks_pending` is the explicit signal: the server handed back a
 * first-byte URL with only the minimal recipe and the full enumeration is
 * still running. Older servers never send it, so the pair
 * (status, provenance) is the fallback — any non-verified provenance means
 * the list is still declared metadata. The only confident state is positive
 * probe evidence. Missing fields (older servers, non-virtual sources) keep
 * the previous confident behavior.
 */
export function isInventoryProvisional(
  status?: string | null,
  provenance?: string | null,
  tracksPending?: boolean | null,
): boolean {
  if (tracksPending === true) return true;
  if (provenance === "verified") return false;
  if (provenance === "declared" || provenance === "pending" || provenance === "failed") {
    return true;
  }
  return status === "declared";
}
