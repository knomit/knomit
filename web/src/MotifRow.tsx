import { MOTIF_GLYPH } from './utils';
import { CELL_PAD_X } from './ConnectionsMenu';
import type { ResolvedMotif } from './useMotifClusters';

/** The id the overflow cell answers to, so the row and the panel agree on which
 *  thing is open without either inventing a motif name that does not exist. */
export const OVERFLOW = '\0overflow';

/** How many motif names the row draws before the rest become a count.
 *
 *  Not a width measurement, deliberately. Measuring would mean the row re-flowed
 *  as the panel was dragged, and the thing that would move is the version cell
 *  the reader may be reaching for. A fact carries at most three motifs; two
 *  whole names plus a count is what fits at the narrowest the pane can get
 *  (about 455px, when the list is dragged to its 60% limit on a 1280px window),
 *  and at that width the two longest real names in these knowledge bases —
 *  41 and 38 characters — would not both fit however cleverly it were measured.
 */
export const MAX_SHOWN_MOTIFS = 2;

export interface OrderedMotifs {
  shown: ResolvedMotif[];
  hidden: ResolvedMotif[];
}

/**
 * Order a fact's motifs for the row, and decide which ones it can draw.
 *
 * BY CARRIER COUNT, DESCENDING — never by the order the author typed them into
 * the file, which carries no information at all. The panel sorts the same way,
 * so the row and the panel can never disagree about the order of one list.
 *
 * A motif whose count has not arrived (or failed) sorts last rather than as a
 * zero: it is unknown, not small, and treating it as small would let a
 * transient network failure quietly demote the most-used motif on the fact.
 *
 * Names that do not fit are DROPPED WHOLE into the overflow count. They are
 * never truncated: a motif is a compressed claim whose meaning sits in its last
 * word, so half of one asserts something else — `failure-presents-as-…` reads
 * as the ordinary case, the exact inverse of the motif it came from.
 */
export function orderMotifs(motifs: ResolvedMotif[]): OrderedMotifs {
  const ranked = [...motifs].sort((a, b) => {
    const ca = a.cluster?.carrier_count;
    const cb = b.cluster?.carrier_count;
    // Unknown sorts after known, whatever the known value is.
    if (ca === undefined && cb === undefined) return a.motif.localeCompare(b.motif);
    if (ca === undefined) return 1;
    if (cb === undefined) return -1;
    // Name tie-break, so two renders of one fact cannot disagree about which of
    // two equally-carried motifs leads.
    return cb - ca || a.motif.localeCompare(b.motif);
  });
  return {
    shown: ranked.slice(0, MAX_SHOWN_MOTIFS),
    hidden: ranked.slice(MAX_SHOWN_MOTIFS),
  };
}

/**
 * The `+N` cell: the motifs the row had no room to name.
 *
 * A button like the others, opening the same panel — where every name is whole.
 * It is not a label saying "there is more": the row holds only controls, and a
 * count you cannot act on would be the one dead thing in it.
 */
export function MotifOverflowCell({ hidden, open, onToggle, panelId }: {
  hidden: ResolvedMotif[];
  open: boolean;
  onToggle: (motif: string) => void;
  panelId: string;
}) {
  return (
    <button
      type="button"
      data-testid="motif-overflow"
      data-count={hidden.length}
      onClick={() => onToggle(OVERFLOW)}
      aria-expanded={open}
      aria-controls={panelId}
      // The names ARE known — they came from the fact — so the reader can learn
      // what is behind the count without opening anything.
      title={`Also on this fact: ${hidden.map(h => h.motif).join(', ')}`}
      style={{
        color: open ? '#ffffff' : '#c8cfdb',
        display: 'inline-flex', alignItems: 'center', gap: 5,
        background: open ? '#151515' : 'none',
        boxShadow: open ? 'inset 0 -2px 0 currentColor' : 'none',
        border: 'none', outline: 'none', borderRadius: 0,
        padding: `3px ${CELL_PAD_X}px`, whiteSpace: 'nowrap', cursor: 'pointer',
        fontFamily: 'var(--k-font-mono)', fontSize: 11, lineHeight: 1.4,
      }}
    >
      <span aria-hidden style={{ fontSize: 10, opacity: 0.9, color: '#6d7788' }}>{MOTIF_GLYPH}</span>
      <b style={{ fontWeight: 600 }}>+{hidden.length}</b>
    </button>
  );
}
