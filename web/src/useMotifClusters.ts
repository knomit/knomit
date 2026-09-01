import { useState } from 'react';
import type { MotifCluster } from './api';
import type { MotifEndpoint } from './motifEndpoint';
import { canReadMotifCluster, motifEndpointKey, readMotifCluster } from './motifEndpoint';
import { useAsync } from './hooks';

/** One motif on a fact, and how far its resolution has got.
 *
 *  Three states kept apart on purpose. `loading` and `error` are both "no count
 *  to show", and collapsing either into a resolved zero would say the corpus
 *  holds nothing else of this shape — a claim the reader has no way to doubt.
 *  ConnectionsCell learned this for edge counts; the rule is recorded in
 *  kb/decisions/ui/connections/header-cells and applies verbatim here. */
export interface ResolvedMotif {
  /** The spelling the FACT carries — always known, never fetched. */
  motif: string;
  status: 'loading' | 'ok' | 'error';
  /** Present only when status is 'ok'. */
  cluster?: MotifCluster;
  /** Present only when status is 'error'; shown on hover, not as a count. */
  error?: string;
}

const NONE: ResolvedMotif[] = [];

/**
 * Resolve a fact's motif names into their clusters.
 *
 * The NAMES arrive on the fact itself and cost nothing; the count, the
 * definition, the carriers and the aliases are one request per motif — one to
 * three per opened fact, since a fact carries at most three. That asymmetry is
 * the whole shape of this hook: it returns an entry for every name immediately
 * so the header can draw them, and fills in the rest as answers land.
 *
 * Requests are per motif rather than batched because there is no batch endpoint,
 * and one failing must not take its neighbours with it — a fact with two motifs
 * where one cluster 500s should show one count and one warning, not two
 * warnings or a blank row.
 *
 * Keyed on the joined names, so a re-render is free and only a genuinely
 * different fact re-fetches. `stale()` guards the write-back: opening a second
 * fact while the first fact's requests are in flight must not paint the first
 * fact's counts beside the second fact's names.
 *
 * The counts are of whatever corpus `endpoint` names, which in a lens is the
 * WHOLE lens rather than the mount the fact happens to live on. That is the
 * number the reader can act on: the count sits beside a pivot, and the pivot
 * lists the lens. A mount-scoped count under a lens-scoped pivot would promise
 * one number and deliver another.
 */
export function useMotifClusters(
  endpoint: MotifEndpoint, motifs: string[] | undefined,
): ResolvedMotif[] {
  const names = motifs ?? NONE.map(String);
  const key = names.join('\0');
  const endpointKey = motifEndpointKey(endpoint);
  const [resolved, setResolved] = useState<Record<string, ResolvedMotif>>({});

  useAsync((stale) => {
    if (names.length === 0) return;
    // Nowhere to ask yet (App picks a repo from /api/v1/repos on mount) means
    // any request is a certain 404, which would render as a failed count on a
    // fact whose motifs are perfectly fine. Waiting is the honest state; the
    // entries below stay 'loading' until there is somewhere to ask.
    if (!canReadMotifCluster(endpoint)) return;

    setResolved({});
    for (const motif of names) {
      readMotifCluster(endpoint, motif)
        .then(cluster => {
          if (stale()) return;
          setResolved(prev => ({ ...prev, [motif]: { motif, status: 'ok', cluster } }));
        })
        .catch(err => {
          if (stale()) return;
          setResolved(prev => ({ ...prev, [motif]: { motif, status: 'error', error: String(err) } }));
        });
    }
    // `key` and `endpointKey`, not `motifs` and `endpoint`: both of those are
    // rebuilt on every render of the fact view, and depending on them would
    // re-issue every request each time.
  }, [key, endpointKey]);

  if (names.length === 0) return NONE;
  // Built from the NAMES, in the fact's own order — so an unanswered motif is a
  // row with no count rather than a row that is not there. The caller decides
  // the display order (carrier_count-descending); this preserves what the fact
  // said so that decision has something complete to sort.
  return names.map(motif => resolved[motif] ?? { motif, status: 'loading' as const });
}
