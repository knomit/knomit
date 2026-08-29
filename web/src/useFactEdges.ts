import { useState, useMemo, useRef, useEffect } from 'react';
import { api, isNotFound } from './api';
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
 *
 * A LIVE 404 IS A STALE CACHE, NOT AN ANSWER. Live edge fetches are anchored at
 * state.headCommit, which only the page-load bootstrap, SSE `status` events and
 * the post-task refresh ever move. One dropped broadcast pinned tabs to an old
 * commit indefinitely, and every fact created after it 404'd its edges — raw,
 * on a fact whose body loads fine at HEAD, reading like data corruption
 * (issue #178). So a live 404 re-reads the head and retries there ONCE, and
 * reports the head it learned through `onHead` so the rest of the app stops
 * being stale too. It gives up if the head has not actually moved: that 404 is
 * the real answer, and retrying it would be a request loop.
 *
 * This is confined to LIVE views on purpose. In history or diff mode the commit
 * is the user's pin — re-anchoring it would answer a question they did not ask,
 * which is what kb/invariants/ui/navigation/every-hop-is-path-plus-commit
 * forbids ("the target still exists" is never a reason to drop the pin).
 *
 * `onHead` is read through a ref rather than taken as a dependency: it exists to
 * report a discovery, and letting a caller's callback identity re-trigger the
 * fetch would make an unmemoized prop a refetch loop.
 *
 * WHICH RUN ACTUALLY RENDERS depends on the caller, and both are correct. In a
 * caller that re-anchors on the reported head (App does: SET_HEAD → headCommit →
 * `anchor`), reporting changes a dep, the effect re-runs, and the retry's own
 * fetches are discarded by their `stale()` guard — the fresh run is what
 * renders, at the cost of one redundant pair. Keeping the local retry anyway is
 * what makes the hook correct on its own: a caller that passes no `onHead`, or
 * ignores it, still recovers instead of sitting on the error. Either way the
 * head is re-read at most once per failure — a second 404 finds the head
 * unmoved and reports it, so there is no loop.
 */
export function useFactEdges(state: AppState, onHead?: (head: string) => void): FactEdges {
  const [edges, setEdges] = useState<{ incoming: RefGroup[]; outgoing: RefGroup[] }>({ incoming: [], outgoing: [] });
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // Synced in an effect, not during render: a ref write during render is both a
  // lint error and a real hazard under concurrent rendering.
  const onHeadRef = useRef(onHead);
  useEffect(() => { onHeadRef.current = onHead; });

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

    const settle = (e: { incoming: RefGroup[]; outgoing: RefGroup[] }) => {
      setEdges(e);
      setLoading(false);
    };
    // A FAILED FETCH IS NOT A ZERO, but the arrays are still cleared: the error
    // is what consumers render, and stale edges under a new fact would be worse
    // than none. See ConnectionsMenu.
    const fail = (err: unknown) => {
      setEdges({ incoming: [], outgoing: [] });
      setError(String(err));
      setLoading(false);
    };

    const run = (at: string, mayRefreshHead: boolean) => {
      api.explain(a.repo, a.branch, a.path, at, live ? undefined : { fallback: 'before' })
        .then(e => {
          if (stale()) return;
          settle({ incoming: e.incoming, outgoing: e.outgoing });
        })
        .catch(err => {
          if (stale()) return;
          // Only a live view anchored on a cached head can be wrong about WHERE
          // it is looking. Anything else — a lens context, a user's history
          // pin, a non-404 — is reported as-is.
          //
          // `!lensCtx` is redundant TODAY (edgeAnchorCommit returns '' for a
          // lens, so `at === ''` already excludes it) and is stated anyway,
          // because the thing it guards is not local: state.headCommit is the
          // WRITE repo's head, so if lens live edges ever became anchored on
          // the mount head, this would write a READ MOUNT's head into it. The
          // assumption is cheaper to pin here than to rediscover there.
          if (!mayRefreshHead || !live || lensCtx || at === '' || !isNotFound(err)) {
            fail(err);
            return;
          }
          api.status(a.repo, a.branch)
            .then(s => {
              if (stale()) return;
              if (!s.head || s.head === at) {
                fail(err); // the head has not moved; the 404 is the answer
                return;
              }
              onHeadRef.current?.(s.head);
              run(s.head, false);
            })
            .catch(() => { if (!stale()) fail(err); });
        });
    };
    run(anchor, true);
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
