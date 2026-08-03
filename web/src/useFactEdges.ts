import { useState, useMemo } from 'react';
import { api } from './api';
import type { RefGroup } from './api';
import type { AppState } from './state';
import { factHistoryAnchor, edgeAnchorCommit, isLive, isLensContext } from './state';
import { useAsync } from './hooks';

export interface FactEdges {
  incoming: RefGroup[];
  outgoing: RefGroup[];
  loading: boolean;
  /** Fetch error, or null. Empty edges are NOT an error. */
  error: string | null;
  /**
   * Outgoing edge path → that edge's target_commit: the version of the target
   * the referrer actually reasoned over. RightPanel pins in-body ref hops to it
   * rather than to the referrer's own commit_hash, which 404s across PR merges.
   *
   * Empty in diff mode, where in-body hops are not offered.
   */
  refCommits: Map<string, string>;
}

const EMPTY: FactEdges = {
  incoming: [], outgoing: [], loading: false, error: null, refCommits: new Map(),
};

/**
 * The open fact's edges, fetched ONCE for every consumer.
 *
 * RightPanel and EdgesRail each used to call api.explain for the same fact, at
 * the same anchor, with the same fallback — two identical requests per fact
 * open, of which RightPanel discarded `incoming` and the rail used both halves.
 * The duplication was invisible because each call site was individually correct.
 *
 * Hoisting it here also means the OWNER of the layout knows the edge counts
 * directly. The rail used to report "I am empty" back up to App through a
 * callback so App could size its column; with the data owned above both, that
 * protocol is unnecessary.
 *
 * THE ANCHOR IS THE POINT. Both consumers were deliberately given the same
 * anchor (edgeAnchorCommit + fallback-before when not live) so the in-body refs
 * and the connections panel can never disagree about which version of a target
 * is being referenced. That guarantee is now structural rather than a comment
 * asking two call sites to stay in step.
 *
 * THE LENS GUARD IS LOAD-BEARING. In a lens context openFactSource points at the
 * WRITE repo until the open fact's mount resolves (SET_FACT_SOURCE, after
 * getLensFact). Fetching before then reads the wrong mount and yields an empty
 * edge set — so this waits, and re-runs when factSource lands. RightPanel had
 * this guard; EdgesRail did not, which is one way the two calls could differ.
 */
export function useFactEdges(state: AppState): FactEdges {
  const [edges, setEdges] = useState<{ incoming: RefGroup[]; outgoing: RefGroup[] }>({ incoming: [], outgoing: [] });
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const factPath = state.factPath;
  const lensCtx = isLensContext(state);
  const anchor = edgeAnchorCommit(state);
  const live = isLive(state);
  const inDiff = state.asOf.mode === 'diff';
  const a = factHistoryAnchor(state);

  useAsync((stale) => {
    const clear = () => { setEdges({ incoming: [], outgoing: [] }); setError(null); setLoading(false); };
    if (!factPath) { clear(); return; }
    if (lensCtx && !state.factSource) { clear(); return; }

    setLoading(true);
    setError(null);
    api.explain(a.repo, a.branch, a.path, anchor, live ? undefined : { fallback: 'before' })
      .then(e => {
        if (stale()) return;
        setEdges({ incoming: e.incoming, outgoing: e.outgoing });
      })
      .catch(err => {
        if (stale()) return;
        setEdges({ incoming: [], outgoing: [] });
        setError(String(err));
      })
      .finally(() => { if (!stale()) setLoading(false); });
    // Deps are the resolved anchor values, not `state`: every field that can
    // change WHICH edges these are appears here, and nothing else re-fetches.
  }, [factPath, a.repo, a.branch, a.path, anchor, live, lensCtx, state.factSource?.repo, state.factSource?.branch]);

  const refCommits = useMemo(() => {
    const m = new Map<string, string>();
    // Diff mode offers no in-body hops, so it gets no pins.
    if (inDiff) return m;
    for (const g of edges.outgoing) {
      const c = g.versions[0]?.commit;
      if (c) m.set(g.path, c);
    }
    return m;
  }, [edges.outgoing, inDiff]);

  if (!factPath) return EMPTY;
  return { incoming: edges.incoming, outgoing: edges.outgoing, loading, error, refCommits };
}
