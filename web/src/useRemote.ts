import { useCallback, useEffect, useState } from 'react';
import { api, type OriginResponse } from './api';

/** RemoteState is the loaded remote for one repo. RepoDetail owns it (rather
 *  than RemoteCard) because the detail pane's ⋯ menu has to know whether a
 *  remote exists before it can decide between offering "Connect a remote…" and
 *  rendering the card at all. */
export interface RemoteState {
  origin: OriginResponse | null;
  loading: boolean;
  err: string;
  setErr: (m: string) => void;
  reload: () => void;
}

/** useRemote loads (and reloads) a repo's origin. `enabled` is false when the
 *  deployment hides remote config — the hook still runs (hooks cannot be
 *  conditional) but issues no request and reports "not connected".
 *
 *  Lives in its own module so RemoteStatus.tsx exports only components and
 *  fast refresh keeps working. */
export function useRemote(repo: string, enabled = true): RemoteState {
  const [origin, setOrigin] = useState<OriginResponse | null>(null);
  const [loading, setLoading] = useState(enabled);
  const [err, setErr] = useState('');
  const [nonce, setNonce] = useState(0);

  useEffect(() => {
    if (!enabled) return;
    let cancelled = false;
    setLoading(true); setErr('');
    api.getOrigin(repo)
      .then(o => { if (!cancelled) setOrigin(o); })
      .catch(() => { if (!cancelled) setErr('could not load remote status'); })
      .finally(() => { if (!cancelled) setLoading(false); });
    return () => { cancelled = true; };
  }, [repo, enabled, nonce]);

  const reload = useCallback(() => setNonce(n => n + 1), []);
  // When disabled, report "not connected, not loading" by derivation rather
  // than by writing state from the effect — nothing was ever fetched.
  if (!enabled) return { origin: null, loading: false, err: '', setErr, reload };
  return { origin, loading, err, setErr, reload };
}
