import { useCallback, useEffect, useRef, useState } from "react";

import { filtersToParams, type LogFilters } from "@/components/logs/filter-types";
import { api, type LogFacets } from "@/lib/api";

const FACET_DEBOUNCE_MS = 300;
const FACET_LIVE_DEBOUNCE_MS = 800;

/**
 * Fetches /api/logs/facets for the current filter set, debounced so typing and
 * rapid toggling collapse into one request. A monotonically increasing request
 * id ignores stale responses. `refresh` re-asks with a longer debounce for live
 * records, so a busy stream does not hammer the endpoint.
 */
export function useLogFacets(filters: LogFilters): {
  facets: LogFacets | null;
  /** True while a request is in flight or the last one failed. */
  stale: boolean;
  refresh: () => void;
} {
  const [facets, setFacets] = useState<LogFacets | null>(null);
  const [stale, setStale] = useState(true);
  const requestId = useRef(0);
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);

  const schedule = useCallback(
    (delay: number) => {
      if (timer.current) clearTimeout(timer.current);
      timer.current = setTimeout(() => {
        const id = ++requestId.current;
        api
          .logFacets({ ...filtersToParams(filters) })
          .then((next) => {
            if (id !== requestId.current) return;
            setFacets(next);
            setStale(false);
          })
          .catch(() => {
            if (id !== requestId.current) return;
            // Keep the last good counts; mark them dimmed rather than empty.
            setStale(true);
          });
      }, delay);
    },
    [filters],
  );

  useEffect(() => {
    setStale(true);
    schedule(FACET_DEBOUNCE_MS);
    return () => {
      if (timer.current) clearTimeout(timer.current);
    };
  }, [schedule]);

  const refresh = useCallback(() => schedule(FACET_LIVE_DEBOUNCE_MS), [schedule]);

  return { facets, stale, refresh };
}
