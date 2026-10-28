/**
 * Retry affordance for a track menu whose provisional inventory outlived its
 * discovery window, or whose discovery already failed. It is deliberately the
 * same weight as the menu's other small text controls rather than a new
 * primary action.
 */
export function DiscoveryRetryButton({ onRetry }: { onRetry: () => void }) {
  return (
    <button
      type="button"
      role="menuitem"
      className="shrink-0 rounded px-2 py-0.5 text-xs text-white/70 hover:bg-white/10 focus-visible:ring-2 focus-visible:ring-white/70 focus-visible:outline-none"
      onClick={onRetry}
    >
      Retry
    </button>
  );
}
