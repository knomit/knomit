import { useCallback, useEffect, useState } from 'react';
import { api } from './api';

// The Authorizations tab's probe (F19 phase 3b). The list call IS the probe:
// null (404) means this server has no OAuth issuer — [oauth] unset, or the
// desktop, which never opens the OAuth listener — and then there is no tab. A
// refusal (403: say, loopback-anonymous without admin) still shows the tab,
// because the page is where the reason is explained.
//
// It runs only while the server pages are open and NOT on Authorizations:
// that page polls for itself and reports its count through `setCount`. It
// never runs on a read-only server, where the tab is omitted whatever the
// answer would be. 30s between probes; the result survives the pages closing,
// so reopening them shows the last known tabs at once.
export interface OAuthAvailability {
  available: boolean;
  /** Requests waiting, for the tab's badge; null when unknown. */
  count: number | null;
  /** The Authorizations page's own poll reports here. */
  setCount: (n: number) => void;
}

const PROBE_MS = 30_000;

export function useOAuthAvailability({ enabled, pageOpen }: {
  /** The server pages are on screen and the server is not read-only. */
  enabled: boolean;
  /** Authorizations is the page on screen (it polls itself). */
  pageOpen: boolean;
}): OAuthAvailability {
  const [available, setAvailable] = useState(false);
  const [count, setCountState] = useState<number | null>(null);
  useEffect(() => {
    if (!enabled || pageOpen) return;
    let cancelled = false;
    const probe = () => {
      api.listOAuthPending()
        .then(list => {
          if (cancelled) return;
          setAvailable(list !== null);
          setCountState(list === null ? null : list.length);
        })
        .catch(() => { if (!cancelled) { setAvailable(true); setCountState(null); } });
    };
    probe();
    const t = setInterval(probe, PROBE_MS);
    return () => { cancelled = true; clearInterval(t); };
  }, [enabled, pageOpen]);
  const setCount = useCallback((n: number) => setCountState(n), []);
  return { available, count, setCount };
}
