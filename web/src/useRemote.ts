import { useCallback, useContext, useEffect, useRef, useState } from 'react';
import { api, type OriginResponse } from './api';
import { AccessSecretsLink } from './accessLink';

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
  // A return from the server pages (AccessSecretsLink.returns) re-reads the
  // origin QUIETLY: the linked secret may have been renamed, replaced or
  // deleted in Access, but the card stays as it is meanwhile (no "Loading…",
  // which would unmount an open credential box), and a failed re-read keeps
  // what was shown.
  // Only when nothing is loading: a quiet re-read that cancels a load in
  // flight would leave `loading` set for good.
  const { returns } = useContext(AccessSecretsLink);
  const readFor = useRef({ repo, enabled, nonce, returns });
  const loadingRef = useRef(false);

  useEffect(() => {
    if (!enabled) return;
    const last = readFor.current;
    const quiet = !loadingRef.current && last.returns !== returns && last.repo === repo && last.enabled === enabled && last.nonce === nonce;
    readFor.current = { repo, enabled, nonce, returns };
    let cancelled = false;
    if (!quiet) { loadingRef.current = true; setLoading(true); setErr(''); }
    api.getOrigin(repo)
      .then(o => { if (cancelled) return; setOrigin(o); if (quiet) setErr(''); })
      .catch(() => { if (!cancelled && !quiet) setErr('could not load remote status'); })
      .finally(() => { if (!cancelled && !quiet) { loadingRef.current = false; setLoading(false); } });
    return () => { cancelled = true; };
  }, [repo, enabled, nonce, returns]);

  const reload = useCallback(() => setNonce(n => n + 1), []);
  // When disabled, report "not connected, not loading" by derivation rather
  // than by writing state from the effect — nothing was ever fetched.
  if (!enabled) return { origin: null, loading: false, err: '', setErr, reload };
  return { origin, loading, err, setErr, reload };
}
