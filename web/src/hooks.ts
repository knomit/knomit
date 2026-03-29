import { useEffect } from 'react';
import type { DependencyList } from 'react';

/**
 * Runs an async side-effect with automatic stale-flag management.
 * Equivalent to useEffect but provides a stale() checker so async
 * callbacks can bail out if the component re-ran before they resolved.
 * Pass all values read inside fn as deps (same rules as useEffect).
 */
export function useAsync(fn: (stale: () => boolean) => void, deps: DependencyList) {
  useEffect(() => {
    let isStale = false;
    fn(() => isStale);
    return () => { isStale = true; };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps);
}
