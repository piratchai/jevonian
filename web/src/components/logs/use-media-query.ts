import { useSyncExternalStore } from "react";

/**
 * Live CSS media-query match. `useSyncExternalStore` keeps it tear-free;
 * the query string is the subscription key, so a new value resubscribes.
 */
export function useMediaQuery(query: string): boolean {
  return useSyncExternalStore(
    (onChange) => {
      const list = window.matchMedia(query);
      list.addEventListener("change", onChange);
      return () => list.removeEventListener("change", onChange);
    },
    () => window.matchMedia(query).matches,
    () => false,
  );
}
