import { useCallback, useEffect, useState } from "react";

import {
  PROVISIONAL_INVENTORY_TIMEOUT_MS,
  isProvisionalInventoryExpired,
} from "../utils/inventoryProvenance";

/**
 * Bounds a menu's provisional state.
 *
 * Returns `expired` once the inventory has stayed provisional for the timeout,
 * so the menu can replace its spinner with an explicit retry. `retry` starts a
 * fresh window: the caller re-triggers discovery and the menu shows progress
 * again until either evidence lands or the bound lapses a second time.
 *
 * The timer is armed by the provisional transition, not by the menu opening, so
 * a menu opened long after discovery stalled does not spin for another window.
 * Each stretch carries its own window id, so a later stretch never inherits the
 * previous one's expiry.
 */
export function useProvisionalInventoryExpiry(
  provisional: boolean,
  timeoutMs: number = PROVISIONAL_INVENTORY_TIMEOUT_MS,
) {
  const [windowId, setWindowId] = useState(0);
  const [expiredWindow, setExpiredWindow] = useState<number | null>(null);
  // A new provisional stretch is a new window. Adjusting the window during
  // render keeps the previous stretch's expiry from painting for a frame before
  // an effect could clear it.
  const [previousProvisional, setPreviousProvisional] = useState(provisional);
  if (previousProvisional !== provisional) {
    setPreviousProvisional(provisional);
    setWindowId((current) => current + 1);
  }

  useEffect(() => {
    if (!provisional) return;
    const startedAt = Date.now();
    let timer: ReturnType<typeof setTimeout>;
    const check = () => {
      // Read the wall clock rather than trusting the timer: a clock correction
      // during playback must not declare a fresh window expired early.
      if (!isProvisionalInventoryExpired(true, startedAt, Date.now(), timeoutMs)) {
        timer = setTimeout(check, 1_000);
        return;
      }
      setExpiredWindow(windowId);
    };
    timer = setTimeout(check, timeoutMs);
    return () => clearTimeout(timer);
  }, [provisional, timeoutMs, windowId]);

  const retry = useCallback(() => setWindowId((current) => current + 1), []);

  return { expired: provisional && expiredWindow === windowId, retry };
}
